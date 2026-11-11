package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Delivery of a phone message to a Claude session goes through agent-deck's
// durable send queue (`session send --queue`), never through a blocking
// `--wait` inside the per-session worker.
//
// Why (2026-10-20): the worker used to run the first ask of an idle session as
// `send --wait --timeout 3600s` INSIDE the per-session FIFO, so for the whole
// turn (up to an hour) every later ask was accepted ("sent", 202) into the
// in-memory channel but never reached agent-deck — and was lost outright if
// deck-remote restarted first. `--queue` returns at once with a send_id,
// delivers in send order at most once from agent-deck's own on-disk queue
// (survives a deck-remote restart), and `send-status` reports where each
// message is, which is what the PWA now shows.

const (
	sendPollEvery = 3 * time.Second
	sendLandedMax = 2 * time.Minute // no transcript proof is uncertainty, never permission to resend
	// A queued send has a 30m delivery budget in agent-deck; give up polling a
	// little after that rather than forever.
	sendTrackMax = 40 * time.Minute
)

// queuedSend is the part of agent-deck's send record (`--queue --json` reply
// and `send-status --json`) deck-remote reads.
type queuedSend struct {
	SendID       string    `json:"send_id"`
	SessionID    string    `json:"session_id"`
	State        string    `json:"state"`
	Reason       string    `json:"reason"`
	TargetStatus string    `json:"target_status"`
	Attempts     int       `json:"attempts"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	SentAt       time.Time `json:"sent_at"`
	LandedAt     time.Time `json:"landed_at"`
	Settled      bool      `json:"settled"`
	LandedRowID  string    `json:"landed_row_id"`
}

// enqueueSend hands text to agent-deck's durable queue and returns its record.
// A failure here means nothing was typed (agent-deck rejected the send at
// once, e.g. target not running), so it is safe to retry.
func (s *server) enqueueSend(ctx context.Context, id, text string, images ...string) (queuedSend, error) {
	flags := append([]string{"--queue"}, s.typedSendFlags(ctx)...)
	if len(images) > 0 && !s.imageOK {
		return queuedSend{}, fmt.Errorf("agent-deck lacks session send --image")
	}
	for _, path := range images {
		flags = append(flags, "--image", path)
	}
	out, execErr := s.adeckIn(ctx, []byte(text), append(sendArgs(id, flags...), "--json")...)
	var rec queuedSend
	decErr := json.NewDecoder(bytes.NewReader(out)).Decode(&rec)
	if execErr != nil {
		var p sendDeliveryPayload
		_ = json.NewDecoder(bytes.NewReader(out)).Decode(&p)
		msg := p.Error
		if msg == "" {
			msg = execErr.Error()
		}
		return rec, &sendError{Delivery: p.Delivery, Retryable: true, msg: msg}
	}
	if decErr != nil || rec.SendID == "" {
		return rec, &sendError{msg: "agent-deck did not return a send id (needs `session send --queue`, v1.16.17+)"}
	}
	return rec, nil
}

// typedSendFlags returns the flags that make a phone send reach the target as
// the operator's typed input. With send_transport = "auto" in agent-deck's
// config, a send goes over Claude Code's messaging socket, which frames it as
// "Another Claude session sent a message" — wrong for the operator's own
// phone. Prefer --require-input-prompt: it persists through the queue into
// the child, forces tmux and preserves occupied drafts. Older versions use
// --transport tmux, or the legacy call when neither flag exists. Only phone
// sends use this; tower actions keep the socket. Probed once per process.
func (s *server) typedSendFlags(ctx context.Context) []string {
	// Probe until it yields help text. A timed-out or failed probe has no output
	// and must not pin the process to the legacy socket path (peer-framed,
	// unguarded sends whose landing the queue then misjudges) until restart.
	s.typedMu.Lock()
	if !s.typedProbed {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		// Read the help even on a non-zero exit (flag.ContinueOnError).
		out, _ := s.adeckIn(pctx, nil, "session", "send", "--help")
		cancel()
		s.typedOK = strings.Contains(string(out), "-transport")
		s.promptOK = strings.Contains(string(out), "-require-input-prompt")
		s.imageOK = strings.Contains(string(out), "-image")
		s.typedProbed = strings.TrimSpace(string(out)) != ""
		log.Printf("send: agent-deck --require-input-prompt supported=%v --transport supported=%v probed=%v", s.promptOK, s.typedOK, s.typedProbed)
	}
	s.typedMu.Unlock()
	// Newer agent-deck removed --transport. Unlike the old outer-call flag,
	// this guard is persisted in the queue record and inherited by its child;
	// it both preserves an occupied draft and pins delivery to typed input.
	if s.promptOK {
		return []string{"--require-input-prompt"}
	}
	if s.typedOK {
		return []string{"--transport", "tmux"}
	}
	return nil
}

// sendStatus reads a queued send's current record.
func (s *server) sendStatus(ctx context.Context, sendID string) (queuedSend, error) {
	var rec queuedSend
	err := s.adeckJSON(ctx, &rec, "session", "send-status", sendID, "--json")
	return rec, err
}

// uiSendState is one step of a phone message's life as the PWA shows it.
type uiSendState struct {
	State     string // queued | delivering | sent | delivered (landed) | lost | failed
	Final     bool
	Retryable bool
}

// mapSendState translates an agent-deck send record into the PWA's states.
//
// Only landed is proof of transcript delivery. A settled typed/submitted
// record is lost/unconfirmed, NOT delivered and NOT safe to blindly resend.
// The durable queue remains the delivery authority; we do not re-enqueue text.
func mapSendState(r queuedSend) uiSendState {
	if (r.State == "typing" || r.State == "typed" || r.State == "submitted") && !r.SentAt.IsZero() && time.Since(r.SentAt) >= sendLandedMax {
		return uiSendState{State: "lost", Final: r.Settled}
	}
	switch r.State {
	case "landed":
		return uiSendState{State: "delivered", Final: true}
	case "failed":
		return uiSendState{State: "failed", Final: true, Retryable: true}
	case "cancelled":
		return uiSendState{State: "failed", Final: true}
	case "submitted":
		if r.Settled {
			return uiSendState{State: "lost", Final: true}
		}
		return uiSendState{State: "sent"}
	case "typed":
		if r.Settled {
			return uiSendState{State: "lost", Final: true}
		}
		return uiSendState{State: "delivering"}
	case "typing":
		return uiSendState{State: "delivering"}
	default:
		return uiSendState{State: "queued"}
	}
}

// trackedSend is a queued phone message deck-remote is still following.
type trackedSend struct {
	RequestID       string `json:"requestId"`
	SessionID       string `json:"sessionId"`
	SendID          string `json:"sendId"`
	Profile         string `json:"profile,omitempty"`
	Images          int    `json:"images,omitempty"`
	Created         int64  `json:"created"`
	AutoAttempts    int    `json:"autoAttempts,omitempty"`
	LastAutoAttempt int64  `json:"lastAutoAttempt,omitempty"`

	lastProbe int64  // readiness-check backoff, guarded by sendTracker.mu
	releasing bool   // guarded by sendTracker.mu; manual/automatic releases never overlap
	state     string // last observed ui state (not persisted)
	key       string // last published state+position, to publish only changes
}

// sendTracker follows queued sends until they finish, publishes each change on
// the SSE hub, and persists the open set so a deck-remote restart resumes
// following (agent-deck itself keeps delivering regardless).
type sendTracker struct {
	mu      sync.Mutex
	items   map[string]*trackedSend // by requestId
	path    string
	running bool
}

func newSendTracker(dir string) *sendTracker {
	t := &sendTracker{items: map[string]*trackedSend{}}
	if dir != "" {
		t.path = filepath.Join(dir, "ask-pending.json")
	}
	return t
}

// saveLocked writes the open set atomically. The message text is deliberately
// not stored — only ids.
func (t *sendTracker) saveLocked() error {
	if t.path == "" {
		return nil
	}
	list := make([]*trackedSend, 0, len(t.items))
	for _, it := range t.items {
		list = append(list, it)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SendID < list[j].SendID })
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("ask: persist pending sends: %v", err)
		return err
	}
	if err := os.Rename(tmp, t.path); err != nil {
		log.Printf("ask: persist pending sends: %v", err)
		return err
	}
	return nil
}

func (t *sendTracker) load() {
	if t.path == "" {
		return
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var list []*trackedSend
	if json.Unmarshal(b, &list) != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, it := range list {
		if it.RequestID != "" && it.SendID != "" {
			t.items[it.RequestID] = it
		}
	}
}

// position is the 1-based place of it among the session's unfinished sends,
// in send order (agent-deck send ids sort by creation time).
func (t *sendTracker) positionLocked(it *trackedSend) int {
	n := 1
	for _, o := range t.items {
		if o != it && o.SessionID == it.SessionID && o.SendID < it.SendID {
			n++
		}
	}
	return n
}

// track registers a freshly queued send, publishes its first state and makes
// sure the poller is running.
func (s *server) trackSend(it *trackedSend, rec queuedSend) {
	t := s.sends
	it.Created = time.Now().Unix()
	t.mu.Lock()
	t.items[it.RequestID] = it
	t.saveLocked()
	t.mu.Unlock()
	s.observeSend(it, rec)
	s.startSendPoller()
}

// observeSend maps rec to a UI state and publishes it when it changed. A final
// state drops the send from the tracker.
func (s *server) observeSend(it *trackedSend, rec queuedSend) {
	st := mapSendState(rec)
	t := s.sends
	t.mu.Lock()
	pos := 0
	if st.State == "queued" {
		pos = t.positionLocked(it)
	}
	reason := sendReason(rec)
	key := fmt.Sprintf("%s/%d/%s/%s/%d/%s", st.State, pos, reason, rec.TargetStatus, rec.Attempts, rec.UpdatedAt)
	changed := key != it.key
	it.key, it.state = key, st.State
	if st.Final {
		delete(t.items, it.RequestID)
		t.saveLocked()
	}
	t.mu.Unlock()
	if !changed {
		return
	}
	ev := sendStateEvent(it, rec)
	if pos > 0 {
		ev["position"] = pos
	}
	log.Printf("ask: send %s req=%s session=%s -> %s pos=%d %s", it.SendID, it.RequestID, it.SessionID, st.State, pos, rec.Reason)
	s.hub.publish(ev)
}

// sendStateEvent is shared by SSE and explicit status/re-delivery checks, so a
// missed SSE event can be recovered without creating a second send.
func sendStateEvent(it *trackedSend, rec queuedSend) map[string]any {
	st := mapSendState(rec)
	at := rec.UpdatedAt
	if st.State == "lost" && !rec.Settled && !rec.SentAt.IsZero() {
		at = rec.SentAt.Add(sendLandedMax)
	}
	if st.State == "delivered" && !rec.LandedAt.IsZero() {
		at = rec.LandedAt
	}
	if at.IsZero() {
		at = time.Now()
	}
	ev := map[string]any{
		"type": "ask-state", "state": st.State, "requestId": it.RequestID,
		"sessionId": it.SessionID, "sendId": it.SendID, "ts": time.Now().Unix(),
		"statusTs": at.Unix(), "images": it.Images, "reason": sendReason(rec),
		"targetStatus": rec.TargetStatus, "attempts": rec.Attempts,
		"landedRowId": rec.LandedRowID,
		"retryable":   st.Retryable && it.Images == 0,
	}
	if st.State == "failed" {
		ev["error"] = failReason(rec.Reason)
	}
	return ev
}

func sendReason(rec queuedSend) string {
	reason := strings.TrimSpace(rec.Reason)
	if reason == "" {
		switch rec.State {
		case "queued":
			reason = "Waiting for the target to accept input"
		case "typing":
			reason = "Handing the message to the target composer"
		case "typed":
			reason = "Input attempted; submission not confirmed"
		case "submitted":
			reason = "Submission confirmed; waiting for transcript evidence"
		case "landed":
			reason = "Seen in the target transcript"
		case "failed":
			reason = "agent-deck gave up before typing"
		case "cancelled":
			reason = "Cancelled before typing"
		}
	}
	if rec.Settled && (rec.State == "typed" || rec.State == "submitted") {
		reason += "; no landed trace before agent-deck stopped watching — delivery is unconfirmed"
	} else if mapSendState(rec).State == "lost" {
		reason += "; no landed trace after 2 minutes — delivery is unconfirmed, still checking"
	}
	return reason
}

func failReason(r string) string {
	r = strings.TrimSpace(r)
	if r == "" {
		return "agent-deck gave up delivering this message"
	}
	return "Not delivered: " + r
}

// startSendPoller starts the (single) poll loop if it is not already running.
// It exits when nothing is left to follow.
func (s *server) startSendPoller() {
	t := s.sends
	t.mu.Lock()
	if t.running || len(t.items) == 0 {
		t.mu.Unlock()
		return
	}
	t.running = true
	t.mu.Unlock()
	go func() {
		for {
			time.Sleep(sendPollEvery)
			if !s.pollSendsOnce() {
				return
			}
		}
	}()
}

// pollSendsOnce checks every open send once, oldest first. Returns false (and
// clears the running flag) when none remain.
func (s *server) pollSendsOnce() bool {
	t := s.sends
	t.mu.Lock()
	open := make([]*trackedSend, 0, len(t.items))
	for _, it := range t.items {
		open = append(open, it)
	}
	t.mu.Unlock()
	sort.Slice(open, func(i, j int) bool { return open[i].SendID < open[j].SendID })
	for _, it := range open {
		ctx, cancel := context.WithTimeout(withProfile(context.Background(), it.Profile), 15*time.Second)
		rec, err := s.sendStatus(ctx, it.SendID)
		cancel()
		if err != nil {
			if time.Since(time.Unix(it.Created, 0)) > sendTrackMax {
				s.observeSend(it, queuedSend{State: "typed", Settled: true, Reason: "Delivery status unavailable for 40 minutes; nothing re-sent"})
			}
			continue
		}
		s.observeSend(it, rec)
		if rec.State == "queued" {
			go func(it *trackedSend, rec queuedSend) {
				ctx, cancel := context.WithTimeout(withProfile(context.Background(), it.Profile), 3*time.Minute+15*time.Second)
				defer cancel()
				updated, reason := s.maybeAutoRelease(ctx, it, rec)
				if reason != "" {
					s.observeSend(it, updated)
					ev := sendStateEvent(it, updated)
					ev["redeliveryReason"] = reason
					s.hub.publish(ev)
				}
			}(it, rec)
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.items) == 0 {
		t.running = false
		return false
	}
	return true
}

// resumeSends re-follows the sends a previous deck-remote process left open.
func (s *server) resumeSends() {
	s.sends.load()
	s.sends.mu.Lock()
	n := len(s.sends.items)
	s.sends.mu.Unlock()
	if n > 0 {
		log.Printf("ask: resuming %d queued send(s) from previous run", n)
		s.startSendPoller()
	}
}

// publishAskFailed reports a send agent-deck rejected before queueing it.
func (s *server) publishAskFailed(se sessionInfo, reqID string, err error) {
	msg := "Not sent: " + err.Error()
	var sErr *sendError
	if errors.As(err, &sErr) && (sErr.Delivery == "menu_open" || sErr.Delivery == "composer_blocked") {
		msg = "Not sent: a menu or permission prompt is open in the session. Answer it first, then resend."
	}
	ev := map[string]any{
		"type": "ask-state", "state": "failed", "requestId": reqID,
		"sessionId": se.ID, "error": msg, "retryable": true, "ts": time.Now().Unix(),
	}
	s.hub.publish(ev)
}
