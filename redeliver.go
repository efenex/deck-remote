package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// sendIDRE bounds what reaches the agent-deck CLI as a positional argument: a
// durable queue ID (ULID-like), never something that parses as a flag or path.
var sendIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

const (
	maxAutoReleases  = 3
	autoReleaseDelay = 30 * time.Second
)

// The owner is the only delivery authority, including remote targets. Never
// fall back to queue release or session send on an unsupported/uncertain result.
type releaseResult struct {
	SendID    string      `json:"send_id"`
	RequestID string      `json:"request_id"`
	Outcome   string      `json:"outcome"`
	Reason    string      `json:"reason"`
	Snapshot  *queuedSend `json:"snapshot"`
}

func (s *server) releaseSend(ctx context.Context, rec queuedSend, key string) (queuedSend, string, error) {
	out, err := s.adeck(ctx, "session", "redeliver", rec.SendID, "--request-id", key, "--json")
	if err != nil {
		if unknownRedeliver(string(out) + err.Error()) {
			rec.RedeliveryOutcome = "unsupported"
			return rec, "unverifiable: agent-deck does not support re-delivery", nil
		}
		return rec, "", fmt.Errorf("re-delivery outcome unavailable; retry this check: %w", err)
	}
	var result releaseResult
	if json.Unmarshal(out, &result) != nil {
		return rec, "", fmt.Errorf("re-delivery returned invalid JSON; retry this check")
	}
	// agent-deck's remote compatibility response deliberately omits IDs.
	if result.Outcome == "unsupported" && result.Reason == "remote_redeliver_unavailable" && result.SendID == "" && result.RequestID == "" && result.Snapshot == nil {
		rec.RedeliveryOutcome = "unsupported"
		return rec, "unverifiable: remote agent-deck does not support re-delivery", nil
	}
	if result.SendID != rec.SendID || result.RequestID != key {
		return rec, "", fmt.Errorf("re-delivery returned no matching operation receipt; retry this check")
	}
	if result.Snapshot != nil {
		if result.Snapshot.SessionID != rec.SessionID || result.Snapshot.State == "" {
			return rec, "", fmt.Errorf("re-delivery returned no matching session snapshot; retry this check")
		}
		snap := result.Snapshot
		rec.State, rec.Settled, rec.Attempts = snap.State, snap.Settled, snap.Attempts
		rec.LandedRowID, rec.LandedAt = snap.LandedRowID, snap.LandedAt
	}
	var note string
	switch result.Outcome {
	case "already_landed", "landed":
		if result.Snapshot == nil || rec.State != "landed" {
			return rec, "", fmt.Errorf("re-delivery returned no landed snapshot; retry this check")
		}
		note = "landed"
	case "blocked_unknown":
		note = "may have landed; not re-sent · check again"
	case "unsupported":
		note = "unverifiable"
	case "pending":
		note = "in progress"
	case "conflict", "not_found", "refused":
		note = strings.ReplaceAll(result.Outcome, "_", " ")
	default:
		return rec, "", fmt.Errorf("re-delivery returned an unknown outcome; retry this check")
	}
	// A replay's immutable decision can be older than its current snapshot.
	if rec.State == "landed" {
		note = "landed"
	} else if result.Reason != "" && result.Outcome != "blocked_unknown" {
		reason := strings.Join(strings.Fields(strings.ReplaceAll(result.Reason, "_", " ")), " ")
		if len(reason) > 160 {
			reason = reason[:160]
		}
		note += ": " + reason
	}
	rec.RedeliveryOutcome = result.Outcome
	rec.Reason = note
	rec.UpdatedAt = time.Now()
	return rec, note, nil
}

func unknownRedeliver(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "unknown command") || strings.Contains(message, "unknown session command")
}

// Status recovery is read-capable: --no-resume never starts a queue worker.
func (s *server) handleSendStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !sendIDRE.MatchString(q.Get("sendId")) || q.Get("requestId") == "" || q.Get("sessionId") == "" {
		httpError(w, http.StatusBadRequest, "requestId, sendId and sessionId required")
		return
	}
	ctx, cancel := context.WithTimeout(withProfile(r.Context(), reqProfile(r)), 15*time.Second)
	defer cancel()
	rec, err := s.sendStatus(ctx, q.Get("sendId"))
	if err != nil {
		httpError(w, http.StatusBadGateway, "Delivery status unavailable: "+err.Error())
		return
	}
	if rec.SendID != q.Get("sendId") || rec.SessionID != q.Get("sessionId") {
		httpError(w, http.StatusConflict, "send does not belong to this session")
		return
	}
	it := &trackedSend{RequestID: q.Get("requestId"), SendID: rec.SendID, SessionID: rec.SessionID}
	writeJSON(w, http.StatusOK, sendStateEvent(it, rec))
}

// POST /api/rc/ask/redeliver remains behind the typed-input gate. operationId
// identifies a persisted user tap; requestId still identifies the phone bubble.
func (s *server) handleRedeliver(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID   string `json:"requestId"`
		OperationID string `json:"operationId"`
		SendID      string `json:"sendId"`
		SessionID   string `json:"sessionId"`
		Profile     string `json:"profile"`
		CheckOnly   bool   `json:"checkOnly"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || !sendIDRE.MatchString(req.SendID) || req.RequestID == "" || req.SessionID == "" ||
		(!req.CheckOnly && !sendIDRE.MatchString(req.OperationID)) {
		httpError(w, http.StatusBadRequest, "requestId, operationId, sendId and sessionId required")
		return
	}
	// Compatibility for older pages; the new UI uses the read-only GET route.
	if req.CheckOnly {
		q := r.URL.Query()
		q.Set("requestId", req.RequestID)
		q.Set("sendId", req.SendID)
		q.Set("sessionId", req.SessionID)
		q.Set("profile", req.Profile)
		r.URL.RawQuery = q.Encode()
		s.handleSendStatus(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(withProfile(context.WithoutCancel(r.Context()), req.Profile), 3*time.Minute+15*time.Second)
	defer cancel()
	t := s.sends
	t.mu.Lock()
	it := t.items[req.RequestID]
	if it != nil && (it.SendID != req.SendID || it.SessionID != req.SessionID || profileFrom(ctx, s.cfg.profile) != profileFrom(withProfile(context.Background(), it.Profile), s.cfg.profile)) {
		t.mu.Unlock()
		httpError(w, http.StatusConflict, "send does not match the phone request")
		return
	}
	t.mu.Unlock()
	rec := queuedSend{SendID: req.SendID, SessionID: req.SessionID}
	if it == nil {
		// Settled sends leave the tracker. Verify ownership without resuming them.
		var err error
		rec, err = s.sendStatus(ctx, req.SendID)
		if err != nil {
			outcome, reason := "", ""
			if errors.Is(err, errSendNotFound) {
				outcome, reason = "not_found", "Original send not found"
			}
			if errors.Is(err, errSendStatusUnsupported) {
				outcome, reason = "unsupported", "unverifiable: agent-deck lacks read-only status"
			}
			if outcome != "" {
				it = &trackedSend{RequestID: req.RequestID, SessionID: req.SessionID, SendID: req.SendID}
				ev := sendStateEvent(it, queuedSend{RedeliveryOutcome: outcome, Reason: reason})
				ev["redeliveryReason"], ev["operationId"] = reason, req.OperationID
				writeJSON(w, http.StatusOK, ev)
				return
			}
			httpError(w, http.StatusBadGateway, "Cannot verify original send: "+err.Error())
			return
		}
		if rec.SendID != req.SendID || rec.SessionID != req.SessionID {
			httpError(w, http.StatusConflict, "send does not belong to this session")
			return
		}
		it = &trackedSend{RequestID: req.RequestID, SessionID: req.SessionID, SendID: req.SendID, Profile: req.Profile}
	}
	// Idempotency and concurrent actions are serialized by agent-deck's key and
	// record locks. Do not reject a replay just because the first HTTP call lives.
	rec, reason, err := s.releaseSend(ctx, rec, req.OperationID)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	ev := sendStateEvent(it, rec)
	ev["redeliveryReason"], ev["operationId"] = reason, req.OperationID
	ev["profile"] = req.Profile
	s.hub.publish(ev)
	if !rec.Settled && (rec.State == "queued" || rec.State == "typing" || rec.State == "typed" || rec.State == "submitted") {
		// A failed send left the open set. Follow the reopened same ID so its
		// eventual landing reaches the phone without another tap/reconnect.
		t.mu.Lock()
		if t.items[it.RequestID] == nil {
			it.Created = time.Now().Unix()
			t.items[it.RequestID] = it
			_ = t.saveLocked()
		}
		t.mu.Unlock()
		s.startSendPoller()
	}
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
	// Keep automatic nudges queue-only. An atomic redeliver here could reopen
	// a send that failed AFTER the queued snapshot, without a deliberate tap.
	// This is the pre-existing automatic control path, never a manual fallback.
	var result struct {
		queuedSend
		Outcome string `json:"outcome"`
	}
	reason := "Automatic queue release outcome unavailable; check again"
	if err := s.adeckJSON(ctx, &result, "session", "queue", "release", rec.SendID, "--json"); err == nil &&
		result.SendID == rec.SendID && result.SessionID == rec.SessionID && result.State != "" {
		rec = result.queuedSend
		reason = "Queue release: " + result.Outcome
	}
	return rec, fmt.Sprintf("Automatic re-delivery check %d/%d: %s", attempt, maxAutoReleases, reason)
}
