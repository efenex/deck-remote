package main

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	recallChainOffTTL = 10 * time.Minute // binary without `timeline --chain`: do not re-probe sooner
)

// recallRow is the part of an agent-deck.recall.rows/v2 row this server keeps.
type recallRow struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	TS    string `json:"ts"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// Detail and IsError: a `!` shell row (kind bash, meta.user_shell) carries the
	// command in Detail; its output arrives as an update with Body (+ IsError when
	// the stream was stderr only).
	Detail  string `json:"detail"`
	IsError *bool  `json:"is_error"`
	Queued  *bool  `json:"queued"`
	RawType string `json:"raw_type"`
	// Origin and Seg are additive rows/v2 fields from forks that carry them
	// (human | peer:<id> | hook | task-notification | system; the conversation id
	// of the segment a row belongs to). Older binaries omit both.
	Origin string `json:"origin"`
	Seg    string `json:"seg"`
	Meta   struct {
		Delivery  string `json:"delivery"`
		UserShell bool   `json:"user_shell"`
		Boundary  string `json:"boundary"`
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
	case "command":
		_, ok := parseSlash(r.Body)
		return ok
	case "bash": // the Bash tool, unless the fork marks it as the operator's `!` line
		return r.Meta.UserShell
	case "system":
		if r.Meta.Boundary != "" || r.RawType == "chain/boundary" {
			return true
		}
		if r.RawType == "attachment/queued_command" {
			return true
		}
		_, _, _, ok := recallPeer(r)
		return ok
	}
	return false
}

// recallHiddenOrigin: rows the fork attributes to hooks, background-task
// notifications or the system are never thread messages.
func recallHiddenOrigin(o string) bool {
	return o == "hook" || o == "task-notification" || o == "system"
}

// recallPeer reports whether a row is another session's message: by the fork's
// origin field when present, else by the envelope (Claude's preamble,
// agent-deck's [agent-deck from:], the cross-session form).
func recallPeer(r recallRow) (from, name, text string, ok bool) {
	if strings.HasPrefix(r.Origin, "peer:") {
		from, name, text, ok = parsePeer(strings.TrimSpace(r.Body))
		if !ok {
			text = strings.TrimSpace(r.Body)
		}
		return strings.TrimPrefix(r.Origin, "peer:"), name, text, text != ""
	}
	if r.Origin != "" && r.Origin != "human" {
		return "", "", "", false
	}
	return parsePeer(strings.TrimSpace(r.Body))
}

type recallSess struct {
	mu        sync.Mutex
	native    string
	rows      []recallRow // kept rows, source order
	through   string
	before    string // cursor for older rows; "" = none left
	chain     bool   // rows (and the before cursor) span /clear segments (timeline --chain)
	lastFetch time.Time
	failUntil time.Time

	// Follower state (recall_follow.go).
	lastAsk          time.Time // last historyWindow call: a follower stops when this goes stale
	fol              *recallFollower
	followBlockUntil time.Time // follower keeps dying: poll until then
}

type recallStore struct {
	mu         sync.Mutex
	off        time.Time // recall unavailable until
	sessions   map[string]*recallSess
	chainOff   time.Time // `timeline --chain` unsupported until (older agent-deck)
	follow     bool      // stream with `recall follow` instead of polling --since
	nFollowers atomic.Int32
}

func newRecallStore() *recallStore {
	return &recallStore{sessions: map[string]*recallSess{}, follow: os.Getenv("DECK_REMOTE_RECALL_FOLLOW") != "off"}
}

func (rs *recallStore) session(key string) *recallSess {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if sv, ok := rs.sessions[key]; ok {
		return sv
	}
	if len(rs.sessions) >= recallMaxSessions {
		for _, old := range rs.sessions { // do not orphan their followers
			old.mu.Lock()
			old.stopFollow()
			old.mu.Unlock()
		}
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
	sv.lastAsk = now
	if now.Before(sv.failUntil) {
		return nil, false, false
	}
	fail := func(err error) ([]histMsg, bool, bool) {
		if recallUnavailable(err) {
			rs.setOff()
		} else {
			sv.failUntil = time.Now().Add(recallFailTTL)
		}
		sv.stopFollow()
		sv.native, sv.rows, sv.through, sv.before = "", nil, "", ""
		return nil, false, false
	}

	open := func() error {
		tl, chained, err := a.s.recallReadChain(ctx, rs, se.ID, "--tail", strconv.Itoa(recallTail))
		if err != nil {
			return err
		}
		sv.stopFollow()
		sv.chain = chained
		sv.native, sv.rows, sv.before = tl.Session.NativeID, nil, tl.BeforeCursor
		sv.apply(tl)
		sv.lastFetch = now
		return nil
	}
	if sv.through != "" && se.ClaudeSessionID != "" && sv.native != "" && se.ClaudeSessionID != sv.native {
		sv.through = "" // /clear: the session points at a new conversation; a follower is bound to the old file
	}
	if sv.through == "" {
		if err := open(); err != nil {
			return fail(err)
		}
	} else if sv.followHealthy() && now.Sub(sv.lastFetch) < recallSafetyEvery {
		// rows arrive by stream: no exec
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

	a.s.ensureFollow(sv, se.ID, profileFrom(ctx, a.s.cfg.profile))
	msgs = recallMessages(sv.rows)
	for pages := 0; len(msgs) < need && sv.before != "" && pages < recallMaxPages; pages++ {
		var tl recallTimeline
		var err error
		if sv.chain { // the cursor may be a "seg:<id>" one: only --chain reads it
			tl, err = a.s.recallRead(ctx, se.ID, "--chain", "--before", sv.before, "--limit", strconv.Itoa(recallPage))
		} else {
			tl, err = a.s.recallRead(ctx, se.ID, "--before", sv.before, "--limit", strconv.Itoa(recallPage))
		}
		if err != nil {
			break // serve what is cached; scroll-back retries on the next request
		}
		if len(tl.Turns) == 0 && len(tl.Updates) == 0 {
			sv.before = "" // an empty page is the start of the chain
			break
		}
		older := make([]recallRow, 0, len(tl.Turns))
		for _, r := range tl.Turns {
			// A boundary can end the next-older page while also sitting in the
			// newer one; its id (boundary:<native_id>) is stable, so keep one.
			if recallKeep(r) && !(strings.HasPrefix(r.ID, "boundary:") && sv.index(r.ID) >= 0) {
				older = append(older, r)
			}
		}
		sv.rows = append(older, sv.rows...)
		sv.before = tl.BeforeCursor
		msgs = recallMessages(sv.rows)
	}
	return msgs, sv.before != "", true
}

// recallReadChain is a tail read that spans /clear when the binary can
// (`recall timeline --chain`, rows carry seg and boundary rows mark each clear);
// an older agent-deck rejects the flag, which is remembered for
// recallChainOffTTL, and the read repeats as a single-segment one.
func (s *server) recallReadChain(ctx context.Context, rs *recallStore, id string, flags ...string) (tl recallTimeline, chained bool, err error) {
	if os.Getenv("DECK_REMOTE_RECALL_CHAIN") != "off" && !rs.chainDisabled() {
		tl, err = s.recallRead(ctx, id, append([]string{"--chain"}, flags...)...)
		if err == nil {
			return tl, true, nil
		}
		if !recallChainUnsupported(err) {
			return tl, false, err
		}
		rs.mu.Lock()
		rs.chainOff = time.Now().Add(recallChainOffTTL)
		rs.mu.Unlock()
	}
	tl, err = s.recallRead(ctx, id, flags...)
	return tl, false, err
}

func (rs *recallStore) chainDisabled() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return time.Now().Before(rs.chainOff)
}

// recallChainUnsupported: this agent-deck has no `timeline --chain`.
func recallChainUnsupported(err error) bool {
	m := strings.ToLower(err.Error())
	return (strings.Contains(m, "flag provided but not defined") || strings.Contains(m, "unknown flag")) && strings.Contains(m, "chain")
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
		if u.Detail != "" {
			row.Detail = u.Detail
		}
		if u.IsError != nil {
			row.IsError = u.IsError
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

// queuedTUIRE marks a queued prompt the TUI itself typed (suggestion picks), not
// the operator's: left out of the thread.
var queuedTUIRE = regexp.MustCompile(`^\s*\[tui\] `)

// recallMessages maps rows/v2 rows onto the thread's histMsg rows, with the same
// filters the transcript parser applies (synthetic-text gates, `!` lines folded
// into bash rows). Peer messages and slash commands become collapsed "peer" /
// "slash" rows (operator decision D4); task-notifications stay out. A row's
// origin field wins when the fork provides it, otherwise a peer is recognised by
// its envelope (peers.go).
func recallMessages(rows []recallRow) []histMsg {
	var msgs []histMsg
	for _, r := range rows {
		ts := tsUnix(r.TS)
		if recallHiddenOrigin(r.Origin) {
			continue
		}
		switch r.Kind {
		case "assistant":
			if t := strings.TrimSpace(r.Body); t != "" {
				msgs = append(msgs, histMsg{Role: "reply", Content: t, Ts: ts, ID: r.ID})
			}
		case "command":
			if cmd, ok := parseSlash(r.Body); ok {
				msgs = append(msgs, slashMsg(cmd, ts, r.ID))
			}
		case "bash": // only reached for the operator's `!` line (recallKeep)
			cmd := strings.TrimSpace(r.Detail)
			if cmd == "" {
				cmd = strings.TrimSpace(r.Title)
			}
			if cmd == "" {
				continue
			}
			msgs = dropQueuedTwin(msgs, cmd)
			m := histMsg{Role: "bash", Content: cmd, Ts: ts, ID: r.ID}
			if out := strings.Trim(r.Body, "\r\n"); out != "" {
				if r.IsError != nil && *r.IsError {
					m.Stderr = out
				} else {
					m.Stdout = out
				}
			}
			msgs = append(msgs, m)
		case "user":
			raw, _ := json.Marshal(r.Body)
			if cmd, so, se, isIn, ok := bashEntry(raw); ok {
				if isIn {
					msgs = dropQueuedTwin(msgs, cmd)
				}
				msgs = addBash(msgs, cmd, so, se, isIn, ts, r.ID)
				continue
			}
			queued := strings.HasPrefix(r.ID, "queue:") || r.Meta.Delivery != ""
			if from, name, text, ok := recallPeer(r); ok {
				msgs = append(msgs, peerMsg(from, name, text, ts, r.ID, queued))
				continue
			}
			t := humanText(raw)
			if t == "" {
				continue
			}
			if queued && queuedTUIRE.MatchString(t) {
				continue
			}
			msgs = append(msgs, histMsg{Role: "user", Content: t, Ts: ts, ID: r.ID, Queued: queued})
		case "system":
			if r.Meta.Boundary != "" || r.RawType == "chain/boundary" {
				msgs = append(msgs, histMsg{Role: "boundary", Kind: "boundary", Content: boundaryLabel(r), Ts: ts, ID: r.ID})
				continue
			}
			if from, name, text, ok := recallPeer(r); ok {
				msgs = append(msgs, peerMsg(from, name, text, ts, r.ID, r.RawType == "attachment/queued_command"))
				continue
			}
			if r.RawType != "attachment/queued_command" || queuedTUIRE.MatchString(r.Body) {
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

func boundaryLabel(r recallRow) string {
	if r.Title != "" {
		return r.Title
	}
	return "Conversation cleared"
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
