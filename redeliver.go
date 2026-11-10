package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	maxAutoReleases  = 3
	autoReleaseDelay = 30 * time.Second
)

// A release operates on the ORIGINAL durable send ID. Never implement it as a
// second session send: a missing landed row is not evidence that nothing landed.
// In particular, installed agent-deck can miss socket-envelope transcript rows.
type releaseResult struct {
	queuedSend
	Outcome string `json:"outcome"`
}

func (s *server) releaseSend(ctx context.Context, rec queuedSend) (queuedSend, string) {
	switch rec.State {
	case "landed":
		return rec, "Already seen in the target transcript; nothing re-sent"
	case "failed", "cancelled":
		return rec, "Re-delivery unavailable: agent-deck cannot reopen this send ID; no new message was created"
	case "queued":
		// agent-deck rechecks under its per-target lock. If it landed after our
		// status read, release returns already_sent without typing again.
	default:
		return rec, "Re-delivery blocked: this message may already have reached the target; agent-deck cannot safely retry this send ID"
	}
	var result releaseResult
	if err := s.adeckJSON(ctx, &result, "session", "queue", "release", rec.SendID, "--json"); err != nil {
		return rec, "Re-delivery outcome unavailable: " + err.Error() + "; no new message was created"
	}
	if result.SendID != rec.SendID || result.SessionID != rec.SessionID || result.State == "" {
		return rec, "Re-delivery returned no matching queue receipt; check delivery before trying again"
	}
	// Exit 0 includes refusals. Even outcome=delivered means only submitted,
	// unless STATE is landed; always map the record, never the outcome alone.
	note := "Re-delivery: " + result.Outcome
	if result.Outcome == "delivered" {
		if result.State == "landed" {
			note = "Re-delivery landed in the target transcript"
		} else {
			note = "Re-delivery submitted; transcript landing not yet confirmed"
		}
	} else if result.Outcome == "" {
		note = "Re-delivery returned no outcome; check the original send before trying again"
	}
	return result.queuedSend, note
}

// POST /api/rc/ask/redeliver, behind the same typed-input gate as /ask.
// send-status can restart an unfinished queue worker, so even a check-only
// request belongs behind this gate, not on a read-capable GET endpoint.
func (s *server) handleRedeliver(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID string `json:"requestId"`
		SendID    string `json:"sendId"`
		SessionID string `json:"sessionId"`
		Profile   string `json:"profile"`
		CheckOnly bool   `json:"checkOnly"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.SendID) == "" || req.RequestID == "" || req.SessionID == "" {
		httpError(w, http.StatusBadRequest, "requestId, sendId and sessionId required")
		return
	}
	ctx, cancel := context.WithTimeout(withProfile(r.Context(), req.Profile), 3*time.Minute+15*time.Second)
	defer cancel()
	rec, err := s.sendStatus(ctx, req.SendID)
	if err != nil {
		httpError(w, http.StatusBadGateway, "Cannot verify delivery; nothing re-sent: "+err.Error())
		return
	}
	if rec.SendID != req.SendID || rec.SessionID != req.SessionID {
		httpError(w, http.StatusConflict, "send does not belong to this session")
		return
	}
	t := s.sends
	t.mu.Lock()
	it := t.items[req.RequestID]
	if it != nil && (it.SendID != req.SendID || it.SessionID != req.SessionID || profileFrom(ctx, s.cfg.profile) != profileFrom(withProfile(context.Background(), it.Profile), s.cfg.profile)) {
		t.mu.Unlock()
		httpError(w, http.StatusConflict, "send does not match the phone request")
		return
	}
	if req.CheckOnly {
		t.mu.Unlock()
		if it == nil {
			it = &trackedSend{RequestID: req.RequestID, SessionID: req.SessionID, SendID: req.SendID, Profile: req.Profile}
		}
		writeJSON(w, http.StatusOK, sendStateEvent(it, rec))
		return
	}
	if it != nil && it.releasing {
		t.mu.Unlock()
		httpError(w, http.StatusConflict, "Re-delivery is already being checked")
		return
	}
	if it != nil {
		it.releasing = true
	}
	t.mu.Unlock()
	if it != nil {
		defer func() { t.mu.Lock(); it.releasing = false; t.mu.Unlock() }()
	} else {
		// Completed records have left our open set. They may still be checked,
		// but a queued record without its original phone receipt is not released.
		if rec.State == "queued" {
			httpError(w, http.StatusConflict, "Original phone request is unavailable; nothing re-sent")
			return
		}
		it = &trackedSend{RequestID: req.RequestID, SessionID: req.SessionID, SendID: req.SendID, Profile: req.Profile}
	}
	rec, reason := s.releaseSend(ctx, rec)
	ev := sendStateEvent(it, rec)
	ev["redeliveryReason"] = reason
	s.hub.publish(ev)
	writeJSON(w, http.StatusOK, ev)
}

// maybeAutoRelease only nudges an UNSENT queue record after fresh evidence that
// a turn finished. The guarded queue child makes the final empty-composer check
// under agent-deck's send lock (the snapshot alone cannot prove an empty draft).
// The budget is for our additional release calls, not agent-deck's own queue
// retries. We never restart a failed/lost/possibly typed send.
func (s *server) maybeAutoRelease(ctx context.Context, it *trackedSend, rec queuedSend) (queuedSend, string) {
	if rec.SendID != it.SendID || rec.SessionID != it.SessionID || rec.State != "queued" || rec.Attempts >= maxAutoReleases {
		return rec, ""
	}
	t := s.sends
	t.mu.Lock()
	due := t.items[it.RequestID] == it && !it.releasing && t.positionLocked(it) == 1 && it.AutoAttempts < maxAutoReleases &&
		time.Since(time.Unix(it.Created, 0)) >= autoReleaseDelay &&
		time.Since(time.Unix(it.LastAutoAttempt, 0)) >= autoReleaseDelay &&
		time.Since(time.Unix(it.lastProbe, 0)) >= autoReleaseDelay
	if due {
		it.releasing = true
		it.lastProbe = time.Now().Unix()
	}
	t.mu.Unlock()
	if !due {
		return rec, ""
	}
	defer func() { t.mu.Lock(); it.releasing = false; t.mu.Unlock() }()
	var target struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		HookStatus string `json:"hook_status"`
		HookFresh  bool   `json:"hook_status_fresh"`
		Substate   string `json:"substate"`
	}
	showCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := s.adeckJSON(showCtx, &target, "session", "show", it.SessionID, "--json")
	cancel()
	if err != nil ||
		target.ID != it.SessionID || !target.HookFresh || target.HookStatus != "waiting" ||
		target.Substate != "idle-at-empty-prompt" || (target.Status != "idle" && target.Status != "waiting") {
		return rec, ""
	}
	t.mu.Lock()
	if t.items[it.RequestID] != it || it.AutoAttempts >= maxAutoReleases {
		t.mu.Unlock()
		return rec, ""
	}
	it.AutoAttempts++
	it.LastAutoAttempt = time.Now().Unix()
	attempt := it.AutoAttempts
	err = t.saveLocked()
	t.mu.Unlock()
	if err != nil {
		return rec, "Automatic re-delivery blocked: could not persist the attempt budget"
	}
	rec, reason := s.releaseSend(ctx, rec)
	return rec, fmt.Sprintf("Automatic re-delivery check %d/%d: %s", attempt, maxAutoReleases, reason)
}
