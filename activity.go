package main

import (
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
}

// spinnerLineRe matches the thinking line by its distinctive "· ↓ <n> tokens".
var spinnerLineRe = regexp.MustCompile(`·\s*↓.*token`)

// toolCallRe pulls "Bash(…)" / "Task(…)" / "Edit(…)" out of a "⏺ Tool(…)" line.
var toolCallRe = regexp.MustCompile(`([A-Z][A-Za-z]+\([^\n]*)`)

// leadingGlyphRe strips the spinner glyph + spaces at the start of a line.
var leadingGlyphRe = regexp.MustCompile(`^[^\p{L}\p{N}]+`)

func parseActivity(pane string) activityInfo {
	clean := stripANSI(pane)
	low := strings.ToLower(clean)
	var a activityInfo
	if strings.Contains(low, "esc to interrupt") {
		a.Working = true
	}
	lines := strings.Split(clean, "\n")

	// Thinking line: scan from the bottom for the "· ↓ … tokens" spinner.
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if spinnerLineRe.MatchString(t) {
			a.Working = true
			a.Activity = strings.TrimSpace(leadingGlyphRe.ReplaceAllString(t, ""))
			break
		}
	}

	// Current tool: the last "⏺ Tool(…)" line whose result is still "Running…".
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
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

// GET /api/rc/activity?id=<id> — live activity for one session (the detail view
// polls this). Best-effort: empty/working=false if the pane can't be read.
func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing id")
		return
	}
	ctx, cancel := cliCtx(r.Context(), 10*time.Second)
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
	pane, err := s.sessionPane(ctx, se.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, activityInfo{})
		return
	}
	writeJSON(w, http.StatusOK, parseActivity(pane))
}
