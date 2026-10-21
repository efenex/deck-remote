package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Context / quota data (task deckremote-ctx-quota-view, 2026-10-21).
//
// The numbers claude-pulse shows in Claude Code's statusline (context %, 5 h and
// 7 d rate limits) reach agent-deck as one statusline record per Claude session,
// stored in the profile's state.db (table claude_statuslines; the stock CLI
// reads the same row with `agent-deck usage statusline --session <id>`).
// Tower's board.py/ctx.py use that CLI once per session. Here that would be one
// agent-deck exec per session per refresh — the process churn the polling budget
// (3862fac) removed — so the whole table is read with ONE `sqlite3 -readonly`
// per statuslineTTL instead, independent of the number of sessions, and not an
// agent-deck exec (execAdeck stays 0; counted separately as execSqlite).
//
// Rate limits are per account and the shared `agent-deck usage` cache mixes
// them, so the quota is taken from the sessions' own records and reported per
// group (same rule as tower's board.py); groups whose records agree on both
// reset times are the same account and share one entry.

const (
	statuslineTTL     = 5 * time.Second
	statuslineTimeout = 3 * time.Second
	// A record older than this belongs to a session nobody is driving: no ctx
	// bar for it (its quota windows still age out through resets_at).
	statuslineMaxAge = 12 * time.Hour
)

// statuslineRecord is the subset of the Claude Code statusLine payload we use.
type statuslineModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type statuslineRecord struct {
	Model         statuslineModel `json:"model"`
	ContextWindow struct {
		UsedPercentage    *float64 `json:"used_percentage"`
		ContextWindowSize int64    `json:"context_window_size"`
		TotalInputTokens  int64    `json:"total_input_tokens"`
	} `json:"context_window"`
	RateLimits struct {
		FiveHour *rateWindow `json:"five_hour"`
		SevenDay *rateWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

type rateWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       int64    `json:"resets_at"` // unix SECONDS (captured_at is milliseconds)
}

// statuslineRow is one claude_statuslines row as printed by `sqlite3 -json`.
type statuslineRow struct {
	ClaudeSessionID string `json:"claude_session_id"`
	SessionID       string `json:"session_id"`
	CapturedAt      int64  `json:"captured_at"` // unix MILLISECONDS
	Record          string `json:"record"`
}

type slEntry struct {
	rec        statuslineRecord
	capturedAt time.Time
}

// ctxInfo is the per-session context occupancy sent to the PWA.
type ctxInfo struct {
	Pct    float64 `json:"pct"`
	Tokens int64   `json:"tokens,omitempty"`
	Window int64   `json:"window,omitempty"`
	Model  string  `json:"model,omitempty"` // display name ("Opus 5.5"), else the id minus "claude-"
	AsOf   int64   `json:"asOf"`            // unix s of the statusline capture
}

type quotaWindow struct {
	Pct      float64 `json:"pct"`
	ResetsAt int64   `json:"resetsAt,omitempty"` // unix s
}

// quotaInfo is one account's 5 h / 7 d usage, with the groups it was read from.
type quotaInfo struct {
	Groups   []string     `json:"groups"`
	FiveHour *quotaWindow `json:"fiveHour,omitempty"`
	SevenDay *quotaWindow `json:"sevenDay,omitempty"`
	AsOf     int64        `json:"asOf"` // unix s of the freshest record behind it
}

// statuslineSnap indexes a read of the table.
type statuslineSnap struct {
	byClaude   map[string]slEntry
	byInstance map[string]slEntry // agent-deck session id → freshest record
}

type statuslineStore struct {
	mu     sync.Mutex // also single-flights the read
	m      map[string]statuslineCacheEntry
	run    func(ctx context.Context, db string) ([]byte, error) // test seam
	home   string
	warned time.Time

	execSqlite atomic.Int64
}

type statuslineCacheEntry struct {
	snap *statuslineSnap
	at   time.Time
}

func newStatuslineStore(home string) *statuslineStore {
	return &statuslineStore{m: map[string]statuslineCacheEntry{}, home: home}
}

func (st *statuslineStore) dbPath(profile string) string {
	if profile == "" {
		profile = "default"
	}
	return filepath.Join(st.home, ".agent-deck", "profiles", profile, "state.db")
}

const statuslineQuery = "select claude_session_id, coalesce(session_id,'') as session_id, captured_at, record from claude_statuslines"

func (st *statuslineStore) exec(ctx context.Context, db string) ([]byte, error) {
	if st.run != nil {
		return st.run(ctx, db)
	}
	if _, err := os.Stat(db); err != nil {
		return nil, err
	}
	bin := "sqlite3"
	if _, err := exec.LookPath(bin); err != nil {
		bin = "/usr/bin/sqlite3" // launchd's PATH may not carry it; stock on macOS
	}
	st.execSqlite.Add(1)
	return exec.CommandContext(ctx, bin, "-readonly", "-cmd", ".timeout 1500", "-json", db, statuslineQuery).Output()
}

// snapshot returns the indexed table for profile, at most statuslineTTL old. A
// failed read serves the previous snapshot (stale beats blank) or nil, and logs
// at most once a minute: a host without sqlite3 just has no ctx bars.
func (st *statuslineStore) snapshot(ctx context.Context, profile string) *statuslineSnap {
	st.mu.Lock()
	defer st.mu.Unlock()
	if e, ok := st.m[profile]; ok && time.Since(e.at) < statuslineTTL {
		return e.snap
	}
	rctx, cancel := context.WithTimeout(ctx, statuslineTimeout)
	defer cancel()
	out, err := st.exec(rctx, st.dbPath(profile))
	if err != nil {
		if time.Since(st.warned) > time.Minute {
			st.warned = time.Now()
			log.Printf("statusline: read %s: %v", profile, err)
		}
		e := st.m[profile]
		e.at = time.Now() // back off for one TTL either way
		st.m[profile] = e
		return e.snap
	}
	snap := parseStatuslineRows(out)
	st.m[profile] = statuslineCacheEntry{snap: snap, at: time.Now()}
	return snap
}

// parseStatuslineRows decodes `sqlite3 -json` output (empty output = no rows).
// Unparseable rows are skipped. A claude id appears once in the table; an
// agent-deck session id can appear for several (it rotates on /clear), so the
// freshest wins.
func parseStatuslineRows(out []byte) *statuslineSnap {
	snap := &statuslineSnap{byClaude: map[string]slEntry{}, byInstance: map[string]slEntry{}}
	if len(strings.TrimSpace(string(out))) == 0 {
		return snap
	}
	var rows []statuslineRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return snap
	}
	for _, r := range rows {
		var rec statuslineRecord
		if json.Unmarshal([]byte(r.Record), &rec) != nil || r.CapturedAt <= 0 {
			continue
		}
		e := slEntry{rec: rec, capturedAt: time.UnixMilli(r.CapturedAt)}
		if r.ClaudeSessionID != "" {
			snap.byClaude[r.ClaudeSessionID] = e
		}
		if r.SessionID != "" && e.capturedAt.After(snap.byInstance[r.SessionID].capturedAt) {
			snap.byInstance[r.SessionID] = e
		}
	}
	return snap
}

// entryFor joins a listed session to its record: by Claude session id (the
// record of an older conversation, before a /clear, does not match), else by
// the agent-deck id when the list carries no Claude id yet.
func (sn *statuslineSnap) entryFor(se sessionInfo) (slEntry, bool) {
	if sn == nil {
		return slEntry{}, false
	}
	if se.ClaudeSessionID != "" {
		e, ok := sn.byClaude[se.ClaudeSessionID]
		return e, ok
	}
	e, ok := sn.byInstance[se.ID]
	return e, ok
}

func modelLabel(m statuslineModel) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return strings.TrimPrefix(m.ID, "claude-")
}

// ctxFor builds the context block for one record; nil when it carries no
// usable context figure or is too old to describe a live session.
func ctxFor(e slEntry, now time.Time) *ctxInfo {
	cw := e.rec.ContextWindow
	if now.Sub(e.capturedAt) > statuslineMaxAge {
		return nil
	}
	var pct float64
	switch {
	case cw.UsedPercentage != nil:
		pct = *cw.UsedPercentage
	case cw.ContextWindowSize > 0 && cw.TotalInputTokens > 0:
		pct = 100 * float64(cw.TotalInputTokens) / float64(cw.ContextWindowSize)
	default:
		return nil
	}
	return &ctxInfo{Pct: pct, Tokens: cw.TotalInputTokens, Window: cw.ContextWindowSize, Model: modelLabel(e.rec.Model), AsOf: e.capturedAt.Unix()}
}

// quotaWindowOf converts a record window; nil when absent or already past its
// reset (the figure then describes the previous window).
func quotaWindowOf(w *rateWindow, now time.Time) *quotaWindow {
	if w == nil || w.UsedPercentage == nil {
		return nil
	}
	if w.ResetsAt > 0 && w.ResetsAt <= now.Unix() {
		return nil
	}
	return &quotaWindow{Pct: *w.UsedPercentage, ResetsAt: w.ResetsAt}
}

func sameWindow(a, b *quotaWindow) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Pct == b.Pct && a.ResetsAt == b.ResetsAt
}

// sameAccount: two groups read from one account. Both windows must carry a
// reset time and agree on both to the second (a second-resolution pair does not
// collide across accounts); failing that, identical figures. Records captured a
// minute apart can differ by a percent, so equal percentages are not required.
func sameAccount(a, b quotaInfo) bool {
	if a.FiveHour != nil && b.FiveHour != nil && a.SevenDay != nil && b.SevenDay != nil &&
		a.FiveHour.ResetsAt > 0 && a.SevenDay.ResetsAt > 0 {
		return a.FiveHour.ResetsAt == b.FiveHour.ResetsAt && a.SevenDay.ResetsAt == b.SevenDay.ResetsAt
	}
	return sameWindow(a.FiveHour, b.FiveHour) && sameWindow(a.SevenDay, b.SevenDay)
}

// quotaByGroup takes, per group, the freshest record that carries rate limits,
// then merges groups that are one account (sameAccount), keeping the figures of
// the fresher record. Never mixes accounts: anything not provably the same
// stays a separate entry.
func quotaByGroup(sessions []sessionInfo, sn *statuslineSnap, now time.Time) []quotaInfo {
	if sn == nil {
		return nil
	}
	best := map[string]slEntry{}
	for _, se := range sessions {
		if se.Archived {
			continue
		}
		e, ok := sn.entryFor(se)
		if !ok || (e.rec.RateLimits.FiveHour == nil && e.rec.RateLimits.SevenDay == nil) {
			continue
		}
		if cur, have := best[se.Group]; !have || e.capturedAt.After(cur.capturedAt) {
			best[se.Group] = e
		}
	}
	groups := make([]string, 0, len(best))
	for g := range best {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	var out []quotaInfo
	for _, g := range groups {
		e := best[g]
		q := quotaInfo{Groups: []string{g}, FiveHour: quotaWindowOf(e.rec.RateLimits.FiveHour, now), SevenDay: quotaWindowOf(e.rec.RateLimits.SevenDay, now), AsOf: e.capturedAt.Unix()}
		if q.FiveHour == nil && q.SevenDay == nil {
			continue
		}
		merged := false
		for i := range out {
			if sameAccount(out[i], q) {
				groups := append(out[i].Groups, g)
				if q.AsOf > out[i].AsOf {
					out[i] = q
				}
				out[i].Groups = groups
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, q)
		}
	}
	return out
}

// annotateCtx attaches the context block to every Claude session with a fresh
// record and returns the quota entries.
func annotateCtx(sessions []sessionInfo, sn *statuslineSnap, now time.Time) []quotaInfo {
	if sn == nil {
		return nil
	}
	for i := range sessions {
		if sessions[i].Tool != "claude" {
			continue
		}
		if e, ok := sn.entryFor(sessions[i]); ok {
			sessions[i].Ctx = ctxFor(e, now)
		}
	}
	return quotaByGroup(sessions, sn, now)
}
