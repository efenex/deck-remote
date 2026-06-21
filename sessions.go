package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// preview collapses a reply to a single-line snippet of at most n runes.
func preview(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// GET /api/rc/sessions — sessions + a best-effort last-reply preview for each
// Claude session. The PWA is detail-first: agent-deck's status is unreliable in
// some setups (stale registry / churn), so we surface the real last reply
// (transcript-based, name-independent) rather than a status chip.
func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := cliCtx(r.Context(), 15*time.Second)
	defer cancel()
	sessions, err := s.listSessions(ctx)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Concurrently fetch last-reply previews (bounded). Failures (no transcript,
	// non-Claude) just leave LastReply empty — never an error for the list.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range sessions {
		if sessions[i].Tool != "claude" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rctx, rc := context.WithTimeout(ctx, 5*time.Second)
			defer rc()
			if out, err := s.sessionReply(rctx, sessions[i].ID); err == nil {
				sessions[i].LastReply = preview(out.Content, 160)
			}
			if pane, err := s.sessionPane(rctx, sessions[i].ID); err == nil {
				act := parseActivity(pane)
				sessions[i].Working = act.Working
				sessions[i].Activity = act.Activity
				sessions[i].CurrentTool = act.CurrentTool
			}
		}(i)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// GET /api/rc/reply?id=<id|title> — last clean reply (wraps `session output --json`).
func (s *server) handleReply(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing id")
		return
	}
	ctx, cancel := cliCtx(r.Context(), 10*time.Second)
	defer cancel()
	out, err := s.sessionReply(ctx, id)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/rc/status?id=<id|title> — quick status for one session.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]any{"id": se.ID, "title": se.Title, "status": se.Status, "tool": se.Tool})
}
