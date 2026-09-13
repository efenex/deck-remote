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

// GET /api/rc/sessions — sessions + best-effort harness snapshots. The PWA is
// detail-first: agent-deck's status is unreliable in
// some setups (stale registry / churn), so we surface the real last reply
// (transcript-based, name-independent) rather than a status chip.
func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := cliCtx(withProfile(r.Context(), reqProfile(r)), 15*time.Second)
	defer cancel()
	sessions, err := s.listSessions(ctx)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}

	// Concurrently fetch structured snapshots (bounded). Unsupported/custom
	// harnesses remain listed with terminal-only capabilities.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rctx, rc := context.WithTimeout(ctx, 5*time.Second)
			defer rc()
			adapter := s.adapterFor(sessions[i])
			snap, snapErr := adapter.Snapshot(rctx, sessions[i], "", false)
			if snapErr == nil {
				if snap.DegradedReason == "" {
					sessions[i].Capabilities = adapter.Capabilities(rctx, sessions[i])
				}
				sessions[i].LastReply = preview(snap.LastReply, 160)
				sessions[i].LastActivity = snap.LastActivity
				sessions[i].Working = snap.Activity.Working
				sessions[i].Activity = snap.Activity.Activity
				sessions[i].CurrentTool = snap.Activity.CurrentTool
				sessions[i].Stalled = snap.Activity.Stalled
				sessions[i].State = snap.State
				sessions[i].DegradedReason = snap.DegradedReason
			}
			if attn, ok := s.attn.get(sessions[i].ID); ok {
				sessions[i].NeedsAttention = attn.Pending
			} else {
				sessions[i].NeedsAttention = snap.Attention.Pending
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
	ctx, cancel := cliCtx(withProfile(r.Context(), reqProfile(r)), 10*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if se.Tool == "codex" {
		snap, snapErr := s.adapterFor(se).Snapshot(ctx, se, "", false)
		if snapErr != nil || snap.DegradedReason != "" {
			if snapErr != nil {
				httpError(w, http.StatusBadGateway, snapErr.Error())
			} else {
				httpError(w, http.StatusNotImplemented, snap.DegradedReason)
			}
			return
		}
		out := replyOutput{Content: snap.LastReply, Role: "assistant"}
		if snap.LastActivity > 0 {
			out.Timestamp = time.Unix(snap.LastActivity, 0).UTC().Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	out, err := s.claudeReply(ctx, se)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	// Sanitize raw/structured content (e.g. a serialized tool_use block) so the
	// client never renders a reply turn as a literal JSON object/array.
	out.Content = cleanReplyContent(out.Content)
	writeJSON(w, http.StatusOK, out)
}

// GET /api/rc/status?id=<id|title> — quick status for one session.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]any{"id": se.ID, "title": se.Title, "status": se.Status, "tool": se.Tool})
}
