package main

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Live "what is the agent doing right now" parsed from the tmux pane. Best-effort
// and Claude-Code-UI-specific: when Claude is working it renders a spinner line
// like "✽ Channelling… (1m 12s · ↓ 2.1k tokens)" and in-progress tool calls like
// "⏺ Bash(…)" / "⏺ Task(…)" with "⎿ Running…". A Task(…) tool IS a subagent.

type activityInfo struct {
	Working     bool   `json:"working"`
	Activity    string `json:"activity,omitempty"`
	CurrentTool string `json:"currentTool,omitempty"`
	// Stalled is set by the watcher's stall detection (NOT by parseActivity): the
	// agent still reports working=true, but the spinner label has been frozen
	// across consecutive polls — a "frozen/stalled spinner", not live work.
	Stalled bool `json:"stalled,omitempty"`
}

// spinnerLineRe matches the live "thinking" line by its "· ↓ <count> tokens"
// counter. The numeric count (digit or "~") is REQUIRED: without it, prose that
// merely mentions "↓ tokens" — e.g. this parser's own documentation scrolled into
// the pane — would be misread as live work (it was).
var spinnerLineRe = regexp.MustCompile(`·\s*↓\s*[~\d][^)\n]*token`)

// toolCallRe pulls "Bash(…)" / "Task(…)" / "Edit(…)" out of a "⏺ Tool(…)" line.
var toolCallRe = regexp.MustCompile(`([A-Z][A-Za-z]+\([^\n]*)`)

// leadingGlyphRe strips the spinner glyph + spaces at the start of a line.
var leadingGlyphRe = regexp.MustCompile(`^[^\p{L}\p{N}]+`)

// liveTailLines bounds how far up from the bottom of the pane we look for the
// live status line / in-progress tool. Claude renders these at the very bottom
// while working; identical markers higher up are scrollback (completed tool
// calls, message text, docs) and must NOT be read as live activity.
const liveTailLines = 12

func parseActivity(pane string) activityInfo {
	clean := stripANSI(pane)
	lines := strings.Split(clean, "\n")

	// Live region: the last few non-empty lines, bottom-first.
	tail := make([]string, 0, liveTailLines)
	for i := len(lines) - 1; i >= 0 && len(tail) < liveTailLines; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			tail = append(tail, t)
		}
	}

	var a activityInfo
	// Primary signal: a real spinner line (with a token count) in the live region.
	for _, t := range tail {
		if spinnerLineRe.MatchString(t) {
			a.Working = true
			a.Activity = strings.TrimSpace(leadingGlyphRe.ReplaceAllString(t, ""))
			break
		}
	}
	// Fallback signal: "esc to interrupt" in the live region (not reliably present,
	// so secondary). Restricted to the tail so scrollback can't trip it.
	if !a.Working {
		for _, t := range tail {
			if strings.Contains(strings.ToLower(t), "esc to interrupt") {
				a.Working = true
				break
			}
		}
	}
	if !a.Working {
		return a // idle: no live activity, and a "current tool" would be meaningless
	}

	// Current tool: the closest-to-bottom "⏺ Tool(…)" in the live region.
	for _, t := range tail {
		idx := strings.Index(t, "⏺ ")
		if idx < 0 {
			continue
		}
		cand := strings.TrimSpace(t[idx+len("⏺ "):])
		if m := toolCallRe.FindString(cand); m != "" {
			// trim absurdly long arg dumps to a readable snippet
			if r := []rune(m); len(r) > 80 {
				m = string(r[:80]) + "…)"
			}
			a.CurrentTool = m
			break
		}
	}
	return a
}

// activityFreshFor bounds how long a cached activity entry is served before a
// read re-scrapes. The watcher refreshes every watchInterval (6s); past this
// window we assume it fell behind (a slow sweep, a session it skipped, a churned
// tmux id) and capture on demand, so a read never serves indefinitely-stale
// state — this is what surfaces the current activity when the PWA (re)opens.
const activityFreshFor = 10 * time.Second

// liveActivity returns the session's parsed live activity. It serves the watcher's
// activity cache (the single pane-reader) while fresh, and otherwise — a cold miss
// OR a stale entry — does an on-demand capture+parse so the value reflects reality
// at read time. The on-demand path reseeds the cache so stall detection keeps
// accumulating. A scrape failure falls back to the last cached value (stale beats
// empty) rather than blanking the session.
func (s *server) liveActivity(ctx context.Context, id string) activityInfo {
	if st, ok := s.acts.get(id); ok && time.Since(st.updatedAt) < activityFreshFor {
		return st.info()
	}
	pane, err := s.sessionPane(ctx, id)
	if err != nil {
		if st, ok := s.acts.get(id); ok {
			return st.info()
		}
		return activityInfo{}
	}
	parsed := parseActivity(pane)
	st := s.acts.update(id, parsed)
	parsed.Stalled = st.Stalled
	return parsed
}

// GET /api/rc/activity?id=<id> — live activity for one session (the detail view
// polls this). Best-effort: empty/working=false if the pane can't be read.
func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing id")
		return
	}
	ctx, cancel := cliCtx(withProfile(r.Context(), reqProfile(r)), 10*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if se.Tool != "claude" {
		writeJSON(w, http.StatusOK, activityInfo{})
		return
	}
	writeJSON(w, http.StatusOK, s.liveActivity(ctx, se.ID))
}
