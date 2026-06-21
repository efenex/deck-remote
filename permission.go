package main

import (
	"net/http"
	"strings"
	"time"
)

// GET /api/rc/permission?id=<id> — report whether a REAL Claude permission
// dialog is currently on the session's pane, and if so return the actual dialog
// text. The PWA surfaces the Approve affordance ONLY when {pending:true}, with
// this text — so approval is never driven by agent-deck's (unreliable) status,
// and the user always sees exactly what they're approving.
//
// Best-effort: if the pane can't be read (e.g. the registry's tmux name is stale
// for a churning session) it returns {pending:false, unavailable:true} rather
// than guessing.
func (s *server) handlePermission(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing id")
		return
	}
	ctx, cancel := cliCtx(r.Context(), 12*time.Second)
	defer cancel()

	se, err := s.findSession(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if se.Tool != "claude" {
		writeJSON(w, http.StatusOK, map[string]any{"pending": false})
		return
	}
	pane, err := s.sessionPane(ctx, se.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"pending": false, "unavailable": true})
		return
	}
	if !isClaudePermissionPrompt(pane) {
		writeJSON(w, http.StatusOK, map[string]any{"pending": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": true, "text": dialogExcerpt(pane)})
}

// dialogExcerpt returns the trailing non-empty lines of the pane, which for a
// Claude permission prompt is the dialog (question + numbered options).
func dialogExcerpt(pane string) string {
	raw := strings.Split(strings.TrimRight(stripANSI(pane), "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimRight(l, " "))
		}
	}
	start := len(lines) - 16
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:], "\n")
}
