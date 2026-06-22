package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type approveRequest struct {
	SessionID string `json:"sessionId"`
	// Profile optionally scopes the approve to a non-default agent-deck profile.
	Profile string `json:"profile"`
}

// claudePermissionMarkers are strings that appear in a Claude permission dialog
// but not at an ordinary idle prompt. We require one of these to be on screen
// before sending the approve keystroke, so a stray "1"+Enter can never land in a
// live composer or a non-permission state. Claude-only by design (v0).
var claudePermissionMarkers = []string{
	"do you want to proceed",
	"do you want to make this edit",
	"do you want to create",
	"yes, allow always",
	"yes, and don't ask again",
	"allow once",
	"no, and tell claude what to do differently",
}

func isClaudePermissionPrompt(pane string) bool {
	low := strings.ToLower(stripANSI(pane))
	for _, m := range claudePermissionMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// POST /api/rc/approve {sessionId} — guarded one-tap approve of a Claude
// permission dialog. Confirms a real dialog is on screen first (pane read),
// sends "1"+Enter, then polls to confirm it cleared. No-op (not an error) if no
// dialog is present, so it can never misfire.
func (s *server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Profile = strings.TrimSpace(req.Profile)
	if req.SessionID == "" {
		httpError(w, http.StatusBadRequest, "sessionId required")
		return
	}

	ctx, cancel := cliCtx(withProfile(r.Context(), req.Profile), 20*time.Second)
	defer cancel()

	se, err := s.findSession(ctx, req.SessionID)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if se.Tool != "claude" {
		httpError(w, http.StatusBadRequest, "guarded approve is Claude-only in v0")
		return
	}

	pane, err := s.sessionPane(ctx, se.ID)
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not read pane: "+err.Error())
		return
	}
	if !isClaudePermissionPrompt(pane) {
		// Safe no-op: nothing to approve.
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": se.ID, "approved": false, "reason": "no permission dialog on screen"})
		return
	}

	// Approve: select option 1 (Yes) and confirm.
	if err := s.sendKeyText(ctx, se.ID, "1"); err != nil {
		httpError(w, http.StatusBadGateway, "send-keys failed: "+err.Error())
		return
	}
	if err := s.sendKeyEnter(ctx, se.ID); err != nil {
		httpError(w, http.StatusBadGateway, "send-keys enter failed: "+err.Error())
		return
	}

	cleared := s.pollDialogCleared(ctx, se.ID)
	s.hub.publish(map[string]any{
		"type": "approve-result", "sessionId": se.ID,
		"approved": true, "cleared": cleared, "ts": time.Now().Unix(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": se.ID, "approved": true, "cleared": cleared})
}

// pollDialogCleared re-reads the pane a few times to confirm the permission
// dialog disappeared after the approve keystroke.
func (s *server) pollDialogCleared(ctx context.Context, id string) bool {
	for i := 0; i < 6; i++ {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(400 * time.Millisecond):
		}
		pane, err := s.sessionPane(ctx, id)
		if err == nil && !isClaudePermissionPrompt(pane) {
			return true
		}
	}
	return false
}
