package main

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Claude history from agent-deck's `recall` rows/v2 (session-store design,
// option F). When `[recall] enabled = true` in agent-deck's config, the Claude
// adapter reads typed rows from `agent-deck recall timeline` instead of parsing
// the transcript itself; when recall is off, unsupported, or erroring, History()
// (the transcript parser in history.go) answers as before.
//
// Cost model: a recall call is a full agent-deck start (adeck's doc comment), and
// a tail read can carry megabytes of tool output. So each open session keeps its
// relevant rows (user / assistant / queued prompts) in memory: one `--tail` read
// to open, `--since <cursor>` deltas afterwards (at most one per
// recallMinInterval), and `--before <cursor>` pages only when scroll-back needs
// older rows than are cached. DECK_REMOTE_RECALL=off disables the path.

const (
	recallTail        = 120 // rows in the opening read
	recallPage        = 400 // rows per --before page
	recallMaxPages    = 6   // --before pages per request, to bound one call
	recallMinInterval = 2 * time.Second
	recallOffTTL      = 60 * time.Second // recall disabled/unsupported: do not re-probe sooner
	recallFailTTL     = 10 * time.Second // per-session error backoff
	recallMaxSessions = 32
)

// recallRow is the part of an agent-deck.recall.rows/v2 row this server keeps.
type recallRow struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	TS      string `json:"ts"`
	Body    string `json:"body"`
	Queued  *bool  `json:"queued"`
	RawType string `json:"raw_type"`
	Meta    struct {
		Delivery string `json:"delivery"`
	} `json:"meta"`
}

type recallTimeline struct {
	Session struct {
		NativeID string `json:"native_id"`
	} `json:"session"`
	Turns         []recallRow `json:"turns"`
	Updates       []recallRow `json:"updates"`
	Removed       []string    `json:"removed"`
	ThroughCursor string      `json:"through_cursor"`
	BeforeCursor  string      `json:"before_cursor"`
}

// recallKeep reports whether a row can become a thread message; everything else
// (tools, thinking, hooks, turn_end, other…) is not stored.
func recallKeep(r recallRow) bool {
	switch r.Kind {
	case "user", "assistant":
		return true
	case "system":
		return r.RawType == "attachment/queued_command"
	}
	return false
}

type recallSess struct {
	mu        sync.Mutex
	native    string
	rows      []recallRow // kept rows, source order
	through   string
	before    string // cursor for older rows; "" = none left
	lastFetch time.Time
	failUntil time.Time
}

type recallStore struct {
	mu       sync.Mutex
	off      time.Time // recall unavailable until
	sessions map[string]*recallSess
}

func newRecallStore() *recallStore { return &recallStore{sessions: map[string]*recallSess{}} }

func (rs *recallStore) session(key string) *recallSess {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if sv, ok := rs.sessions[key]; ok {
		return sv
	}
	if len(rs.sessions) >= recallMaxSessions {
		rs.sessions = map[string]*recallSess{}
	}
	sv := &recallSess{}
	rs.sessions[key] = sv
	return sv
}

func (rs *recallStore) disabled() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return time.Now().Before(rs.off)
}

func (rs *recallStore) setOff() {
	rs.mu.Lock()
	rs.off = time.Now().Add(recallOffTTL)
	rs.mu.Unlock()
}

// recallUnavailable classifies an error from the recall subcommand as "this agent-deck
// cannot serve rows at all" (recall off, or a build without the verb/flag) as
// opposed to a per-session failure.
func recallUnavailable(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "recall is off") || strings.Contains(m, "unknown command") ||
		strings.Contains(m, "unknown subcommand") || strings.Contains(m, "flag provided but not defined")
}

// historyWindow returns the newest messages of a Claude session from recall:
// at least `need` of them when that many exist (the caller windows by offset from
// the newest), and hasOlder when more can be paged. ok=false means "use the
// transcript parser".
func (a claudeAdapter) historyWindow(ctx context.Context, se sessionInfo, need int) (msgs []histMsg, hasOlder, ok bool) {
	rs := a.s.recall
	if rs == nil || os.Getenv("DECK_REMOTE_RECALL") == "off" || rs.disabled() {
		return nil, false, false
	}
	sv := rs.session(profileFrom(ctx, a.s.cfg.profile) + "\x00" + se.ID)
	sv.mu.Lock()
	defer sv.mu.Unlock()
	now := time.Now()
	if now.Before(sv.failUntil) {
		return nil, false, false
	}
	fail := func(err error) ([]histMsg, bool, bool) {
		if recallUnavailable(err) {
			rs.setOff()
		} else {
			sv.failUntil = time.Now().Add(recallFailTTL)
		}
		sv.native, sv.rows, sv.through, sv.before = "", nil, "", ""
		return nil, false, false
	}

	open := func() error {
		tl, err := a.s.recallRead(ctx, se.ID, "--tail", strconv.Itoa(recallTail))
		if err != nil {
			return err
		}
		sv.native, sv.rows, sv.before = tl.Session.NativeID, nil, tl.BeforeCursor
		sv.apply(tl)
		sv.lastFetch = now
		return nil
	}
	if sv.through == "" {
		if err := open(); err != nil {
			return fail(err)
		}
	} else if now.Sub(sv.lastFetch) >= recallMinInterval {
		tl, err := a.s.recallRead(ctx, se.ID, "--since", sv.through)
		if err != nil {
			return fail(err)
		}
		if tl.Session.NativeID != "" && sv.native != "" && tl.Session.NativeID != sv.native {
			// /clear: the session now points at a new conversation. Start over
			// (history across /clear is design slice F4).
			if err := open(); err != nil {
				return fail(err)
			}
		} else {
			sv.apply(tl)
			sv.lastFetch = now
		}
	}

	msgs = recallMessages(sv.rows)
	for pages := 0; len(msgs) < need && sv.before != "" && pages < recallMaxPages; pages++ {
		tl, err := a.s.recallRead(ctx, se.ID, "--before", sv.before, "--limit", strconv.Itoa(recallPage))
		if err != nil {
			break // serve what is cached; scroll-back retries on the next request
		}
		older := make([]recallRow, 0, len(tl.Turns))
		for _, r := range tl.Turns {
			if recallKeep(r) {
				older = append(older, r)
			}
		}
		sv.rows = append(older, sv.rows...)
		sv.before = tl.BeforeCursor
		msgs = recallMessages(sv.rows)
	}
	return msgs, sv.before != "", true
}

// recallRead runs `agent-deck recall timeline --json <flags> <id>` (flags first:
// agent-deck's flag parser stops at the first positional argument).
func (s *server) recallRead(ctx context.Context, id string, flags ...string) (recallTimeline, error) {
	var tl recallTimeline
	args := append([]string{"recall", "timeline", "--json"}, flags...)
	args = append(args, id)
	err := s.adeckJSON(ctx, &tl, args...)
	return tl, err
}

// apply merges one timeline response (a tail read, or a --since delta) into the
// cached rows: new rows append (or replace the row with the same id), updates
// merge by id, removed ids drop.
func (sv *recallSess) apply(tl recallTimeline) {
	for _, r := range tl.Turns {
		if !recallKeep(r) {
			continue
		}
		if i := sv.index(r.ID); i >= 0 {
			sv.rows[i] = r
		} else {
			sv.rows = append(sv.rows, r)
		}
	}
	for _, u := range tl.Updates {
		i := sv.index(u.ID)
		if i < 0 {
			continue // an update to a row we did not keep
		}
		row := sv.rows[i]
		if u.Body != "" {
			row.Body = u.Body
		}
		if u.Queued != nil {
			row.Queued = u.Queued
		}
		if u.Meta.Delivery != "" {
			row.Meta.Delivery = u.Meta.Delivery
		}
		if u.TS != "" {
			row.TS = u.TS
		}
		sv.rows = append(sv.rows[:i], sv.rows[i+1:]...)
		if u.Queued != nil && !*u.Queued && u.TS != "" {
			sv.rows = append(sv.rows, row) // an absorbed message moves to its absorb time
		} else {
			sv.rows = append(sv.rows[:i], append([]recallRow{row}, sv.rows[i:]...)...)
		}
	}
	for _, id := range tl.Removed {
		if i := sv.index(id); i >= 0 {
			sv.rows = append(sv.rows[:i], sv.rows[i+1:]...)
		}
	}
	if tl.ThroughCursor != "" {
		sv.through = tl.ThroughCursor
	}
}

func (sv *recallSess) index(id string) int {
	for i := len(sv.rows) - 1; i >= 0; i-- {
		if sv.rows[i].ID == id {
			return i
		}
	}
	return -1
}

// queuedPeerRE marks a queued prompt as another session's (or the TUI's), not
// the operator's. rows/v2 has no origin field, so this is a stopgap for the
// transcript path's origin.kind filter until the fork carries one.
var queuedPeerRE = regexp.MustCompile(`^\s*(\[agent-deck from:|<cross-session-message|\[tui\] )`)

// recallMessages maps rows/v2 rows onto the thread's histMsg rows, with the same
// filters the transcript parser applies (synthetic-text gates, `!` lines folded
// into bash rows). Peer messages and task-notifications are left out, as in the
// transcript path (design §6.4 is open); rows/v2 has no origin field, so
// a queued prompt is recognised as a peer's by its prefix (queuedPeerRE)
// until the fork adds one.
func recallMessages(rows []recallRow) []histMsg {
	var msgs []histMsg
	for _, r := range rows {
		ts := tsUnix(r.TS)
		switch r.Kind {
		case "assistant":
			if t := strings.TrimSpace(r.Body); t != "" {
				msgs = append(msgs, histMsg{Role: "reply", Content: t, Ts: ts, ID: r.ID})
			}
		case "user":
			raw, _ := json.Marshal(r.Body)
			if cmd, so, se, isIn, ok := bashEntry(raw); ok {
				if isIn {
					msgs = dropQueuedTwin(msgs, cmd)
				}
				msgs = addBash(msgs, cmd, so, se, isIn, ts, r.ID)
				continue
			}
			t := humanText(raw)
			if t == "" {
				continue
			}
			queued := strings.HasPrefix(r.ID, "queue:") || r.Meta.Delivery != ""
			if queued && queuedPeerRE.MatchString(t) {
				continue // a peer's message sent while busy: hidden, like the transcript path (design §6.4)
			}
			msgs = append(msgs, histMsg{Role: "user", Content: t, Ts: ts, ID: r.ID, Queued: queued})
		case "system": // attachment/queued_command (recallKeep)
			if queuedPeerRE.MatchString(r.Body) {
				continue
			}
			raw, _ := json.Marshal(r.Body)
			t := humanText(raw) // drops task-notification, slash wrappers, reminders
			if t == "" {
				continue
			}
			msgs = append(msgs, histMsg{Role: "user", Content: t, Ts: ts, ID: queuedID(r.TS, r.Body), Queued: true, Origin: "human"})
		}
	}
	return msgs
}

// dropQueuedTwin removes the raw queued copy of a `!` line typed while the agent
// worked: recall emits the enqueue as a plain user row (no <bash-input> tags)
// and later the tagged row, whose hash differs, so it never replaces the first.
func dropQueuedTwin(msgs []histMsg, cmd string) []histMsg {
	for i, n := len(msgs)-1, 0; i >= 0 && n < 12; i, n = i-1, n+1 {
		if m := msgs[i]; m.Role == "user" && m.Queued && strings.TrimSpace(m.Content) == cmd {
			return append(msgs[:i], msgs[i+1:]...)
		}
	}
	return msgs
}
