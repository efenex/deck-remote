package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
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

// claudeDialogWindow is how many non-blank lines at the bottom of the pane we
// look at for a live dialog: question, options, footer, plus room for the
// background-task and status rows Claude draws under it.
const claudeDialogWindow = 20

// claudeDialogSelector matches the highlighted numbered option of a live
// dialog ("❯ 1. Yes"); an idle composer never draws one.
var claudeDialogSelector = regexp.MustCompile(`(?m)^[\s│]*❯\s*\d+\.`)

// claudeCountdownRE matches Claude's unattended-session timer, e.g.
// "⚠ Claude Code will automatically block this request in 0:16, to avoid holding up an
// unattended session". Anchored to a line that STARTS (past box borders) with the ⚠ and the
// full "Claude Code will automatically" lead-in, so a command or transcript text that merely
// mentions the words is never touched. A narrow terminal wraps the sentence: a bounded
// "\n + border/indent" continuation is allowed before the M:SS, never a colon or a longer run.
// The match ends at the end of the line holding the M:SS.
var claudeCountdownRE = regexp.MustCompile(`(?m)^[│ \t]*⚠[ \t]*Claude Code will[ \t]+automatically[ \t]+(?:block|deny|reject|cancel)(?:[^:\n]|\n[│ \t]*){0,160}?\bin(?:[ \t]+|\n[│ \t]*)(\d+):(\d{2})\b[^\n]*`)

// claudeNow is the clock behind a countdown's deadline (a test seam).
var claudeNow = time.Now

// stripClaudeCountdown removes the ticking auto-block line from the dialog at the bottom of a
// pane capture (the last claudeDialogWindow non-blank lines; the scrollback is left alone, so
// old text quoting the phrase can neither change the output nor set a deadline) and returns how
// long Claude says is left (-1 when there is no countdown).
func stripClaudeCountdown(pane string) (string, time.Duration) {
	lines := strings.Split(stripANSI(pane), "\n")
	start, seen := 0, 0
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if seen++; seen == claudeDialogWindow {
			start = i
			break
		}
	}
	head, tail := strings.Join(lines[:start], "\n"), strings.Join(lines[start:], "\n")
	remaining := time.Duration(-1)
	tail = claudeCountdownRE.ReplaceAllStringFunc(tail, func(m string) string {
		sub := claudeCountdownRE.FindStringSubmatch(m)
		min, _ := strconv.Atoi(sub[1])
		sec, _ := strconv.Atoi(sub[2])
		remaining = time.Duration(min*60+sec) * time.Second
		return ""
	})
	if start == 0 {
		return tail, remaining
	}
	return head + "\n" + tail, remaining
}

// isClaudePermissionPrompt reports whether a permission dialog is live at the
// bottom of the pane. The capture carries 2000 lines of scrollback, so matching
// anywhere in it would fire on an old dialog or on transcript text quoting a
// marker, and approve would then type "1"+Enter into the live composer.
func isClaudePermissionPrompt(pane string) bool {
	bottom := dialogExcerptLines(pane, claudeDialogWindow)
	if !claudeDialogSelector.MatchString(bottom) {
		return false
	}
	low := strings.ToLower(bottom)
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
	if !structuredHarness(se.Tool) {
		httpError(w, http.StatusBadRequest, "guarded approve is unavailable for this harness")
		return
	}

	pane, err := s.sessionPane(ctx, se.ID)
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not read pane: "+err.Error())
		return
	}
	info, infoErr := s.adapterFor(se).Attention(ctx, se, pane, true)
	if infoErr != nil || !info.Pending || info.Kind != "approval" {
		// Safe no-op: nothing to approve.
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": se.ID, "approved": false, "reason": "no permission dialog on screen"})
		return
	}
	if se.Tool != "claude" {
		result, respondErr := s.adapterFor(se).RespondAttention(ctx, se, attentionResponseRequest{
			SessionID: se.ID, AttentionID: info.ID, Action: "approve-once",
		})
		if respondErr != nil {
			httpError(w, http.StatusConflict, respondErr.Error())
			return
		}
		s.hub.publish(map[string]any{
			"type": "approve-result", "sessionId": se.ID,
			"approved": result.Responded, "cleared": result.Cleared, "ts": time.Now().Unix(),
		})
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": se.ID, "approved": result.Responded, "cleared": result.Cleared})
		return
	}

	// Close the read-to-key race for the compatibility endpoint too.
	pane, err = s.sessionPane(ctx, se.ID)
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not revalidate pane: "+err.Error())
		return
	}
	current, _ := s.adapterFor(se).Attention(ctx, se, pane, true)
	if !current.Pending || current.ID != info.ID {
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": se.ID, "approved": false, "reason": "permission dialog changed before approval"})
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
