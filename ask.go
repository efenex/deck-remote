package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxTurn bounds how long a single agent turn may run before we give up waiting.
const maxTurn = 60 * time.Minute

// sessionQueues serializes work per session so two concurrent /ask calls never
// interleave keystrokes into the same tmux composer. Each session id gets one
// goroutine draining a FIFO channel.
type sessionQueues struct {
	mu sync.Mutex
	q  map[string]chan func()
}

func newSessionQueues() *sessionQueues {
	return &sessionQueues{q: make(map[string]chan func())}
}

// submit enqueues fn for serial execution on the session's queue. Returns false
// if the queue is full (caller should report busy).
func (sq *sessionQueues) submit(id string, fn func()) bool {
	sq.mu.Lock()
	ch, ok := sq.q[id]
	if !ok {
		ch = make(chan func(), 32)
		sq.q[id] = ch
		go func() {
			for f := range ch {
				f()
			}
		}()
	}
	sq.mu.Unlock()
	select {
	case ch <- fn:
		return true
	default:
		return false
	}
}

var reqCounter atomic.Uint64

func newRequestID() string {
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), reqCounter.Add(1))
}

type askRequest struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	// Profile optionally scopes the turn to a non-default agent-deck profile
	// (matches the ?profile= override on the GET surface). Empty = cfg.profile.
	Profile string `json:"profile"`
	// Delivery controls Codex while it is busy: auto (default) queues with Tab,
	// queue explicitly queues, and steer injects into the current turn with Enter.
	Delivery string `json:"delivery"`
}

// POST /api/rc/ask {sessionId, text} — inject a prompt and deliver the reply
// asynchronously over SSE. Returns immediately with a requestId; a "reply"
// event (matching requestId) follows when the turn completes.
func (s *server) handleAsk(w http.ResponseWriter, r *http.Request) {
	s.ask(w, r, false)
}

// POST /api/rc/slash {sessionId, text} — same as ask but the text is a slash
// command (must start with '/'). Reuses the CLI's slash-readiness gate.
func (s *server) handleSlash(w http.ResponseWriter, r *http.Request) {
	s.ask(w, r, true)
}

func (s *server) ask(w http.ResponseWriter, r *http.Request, slash bool) {
	var req askRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Text = strings.TrimSpace(req.Text)
	req.Profile = strings.TrimSpace(req.Profile)
	req.Delivery = strings.ToLower(strings.TrimSpace(req.Delivery))
	if req.SessionID == "" || req.Text == "" {
		httpError(w, http.StatusBadRequest, "sessionId and text required")
		return
	}
	if slash && !strings.HasPrefix(req.Text, "/") {
		req.Text = "/" + req.Text
	}
	if req.Delivery == "" {
		req.Delivery = "auto"
	}
	if req.Delivery != "auto" && req.Delivery != "queue" && req.Delivery != "steer" {
		httpError(w, http.StatusBadRequest, "delivery must be auto, queue, or steer")
		return
	}

	// Resolve to a concrete session id and reject unknown sessions early.
	rctx, cancel := cliCtx(withProfile(r.Context(), req.Profile), 10*time.Second)
	se, err := s.findSession(rctx, req.SessionID)
	cancel()
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if se.Tool == "codex" {
		s.handleCodexAsk(w, r, se, req, slash)
		return
	}
	if req.Delivery == "steer" {
		httpError(w, http.StatusBadRequest, "steer delivery is supported only by Codex")
		return
	}

	reqID := newRequestID()
	run := s.runTurn
	if slash {
		run = s.runSlash
	}
	// CRITICAL: runTurn/runSlash build their OWN detached context.Background();
	// capture req.Profile here so the background turn runs against the same
	// profile the resolve used (else a multi-minute turn silently hits default).
	prof := req.Profile
	queued := s.queue.submit(se.ID, func() { run(se, req.Text, reqID, prof) })
	if !queued {
		httpError(w, http.StatusTooManyRequests, "session busy: queue full")
		return
	}

	s.hub.publish(map[string]any{
		"type": "ask-state", "state": "sent",
		"requestId": reqID, "sessionId": se.ID, "text": req.Text, "slash": slash,
		"ts": time.Now().Unix(),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"requestId": reqID, "sessionId": se.ID, "status": "sent"})
}

type codexDeliveryOutcome struct {
	result deliveryResult
	snap   harnessSnapshot
	err    error
}

func (s *server) handleCodexAsk(w http.ResponseWriter, r *http.Request, se sessionInfo, req askRequest, slash bool) {
	reqID := newRequestID()
	done := make(chan codexDeliveryOutcome, 1)
	prof := req.Profile
	deliveryCtx, deliveryCancel := context.WithTimeout(withProfile(context.Background(), prof), 20*time.Second)
	defer deliveryCancel()
	queued := s.queue.submit(se.ID, func() {
		adapter := s.adapterFor(se)
		snap, err := adapter.Snapshot(deliveryCtx, se, "", false)
		if err != nil || snap.DegradedReason != "" {
			if err == nil {
				err = fmt.Errorf("%s", snap.DegradedReason)
			}
			done <- codexDeliveryOutcome{snap: snap, err: err}
			return
		}
		pane, paneErr := s.sessionPane(deliveryCtx, se.ID)
		if paneErr != nil {
			done <- codexDeliveryOutcome{snap: snap, err: fmt.Errorf("could not read live pane: %w", paneErr)}
			return
		}
		result, err := adapter.Deliver(deliveryCtx, se, req.Text, req.Delivery, pane, true)
		done <- codexDeliveryOutcome{result: result, snap: snap, err: err}
	})
	if !queued {
		httpError(w, http.StatusTooManyRequests, "session busy: queue full")
		return
	}
	var outcome codexDeliveryOutcome
	select {
	case <-r.Context().Done():
		httpError(w, http.StatusRequestTimeout, "delivery timed out")
		return
	case outcome = <-done:
	}
	if outcome.err != nil {
		httpError(w, http.StatusConflict, outcome.err.Error())
		return
	}
	state := outcome.result.State
	s.hub.publish(map[string]any{
		"type": "ask-state", "state": state, "requestId": reqID,
		"sessionId": se.ID, "text": req.Text, "slash": slash, "ts": time.Now().Unix(),
	})
	if slash {
		s.hub.publish(map[string]any{
			"type": "slash-result", "requestId": reqID, "sessionId": se.ID,
			"command": req.Text, "ok": true, "state": state, "ts": time.Now().Unix(),
		})
	} else {
		s.codexReqs.add(se.ID, codexPendingRequest{
			RequestID: reqID, Text: req.Text, Baseline: outcome.snap.Sequence,
		})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"requestId": reqID, "sessionId": se.ID, "status": state,
	})
}

// runTurn sends the prompt with --wait in a background goroutine (off the HTTP
// request) so a multi-minute turn is fine, then publishes the reply over SSE.
// --wait reuses agent-deck's own turn-completion + fresh-output logic, so we
// don't reimplement busy/idle detection.
func (s *server) runTurn(se sessionInfo, text, reqID, prof string) {
	ctx, cancel := context.WithTimeout(withProfile(context.Background(), prof), maxTurn)
	defer cancel()

	// If the session is mid-turn, deliver with --no-wait: agent-deck injects the
	// text into Claude's live composer immediately and Claude QUEUES it natively
	// (runs it after the current turn). The alternative (--wait) gates on the
	// session going idle first (waitForAgentReady), which stalls indefinitely when
	// the session is driven elsewhere (e.g. the laptop) and also blocks this
	// session's queue. --no-wait still verifies delivery (agent-deck #876), and the
	// reply surfaces via the watcher reply-settle + transcript reconcile.
	if s.sessionBusy(ctx, se.ID) {
		dctx, dcancel := context.WithTimeout(withProfile(context.Background(), prof), 30*time.Second)
		defer dcancel()
		log.Printf("ask: runTurn QUEUE (busy) session=%s req=%s text=%q", se.ID, reqID, text)
		if err := s.sendNoWait(dctx, se.ID, text); err != nil {
			log.Printf("ask: runTurn QUEUE ERROR session=%s req=%s: %v", se.ID, reqID, err)
			ev := map[string]any{
				"type": "reply", "requestId": reqID, "sessionId": se.ID,
				"error": "couldn't deliver while busy: " + err.Error(), "ts": time.Now().Unix(),
			}
			for k, v := range sendErrorFields(err) {
				ev[k] = v
			}
			s.hub.publish(ev)
			return
		}
		s.hub.publish(map[string]any{
			"type": "ask-state", "state": "queued",
			"requestId": reqID, "sessionId": se.ID, "ts": time.Now().Unix(),
		})
		return
	}

	log.Printf("ask: runTurn START session=%s req=%s text=%q", se.ID, reqID, text)
	// --json makes agent-deck emit its delivery status object first and the
	// reply body after it; adeckSend hands back only the body.
	out, err := s.adeckSend(ctx, "session", "send", se.ID, text,
		"--wait", "--timeout", fmt.Sprintf("%.0fs", maxTurn.Seconds()))
	if err != nil {
		log.Printf("ask: runTurn ERROR session=%s req=%s: %v", se.ID, reqID, err)
		ev := map[string]any{
			"type": "reply", "requestId": reqID, "sessionId": se.ID,
			"error": err.Error(), "ts": time.Now().Unix(),
		}
		for k, v := range sendErrorFields(err) {
			ev[k] = v
		}
		s.hub.publish(ev)
		return
	}
	reply := cleanReplyContent(strings.TrimRight(out, "\n"))
	log.Printf("ask: runTurn OK session=%s req=%s replyLen=%d", se.ID, reqID, len(reply))
	s.hub.publish(map[string]any{
		"type": "reply", "requestId": reqID, "sessionId": se.ID,
		"content": reply, "ts": time.Now().Unix(),
	})
}

// sessionBusy reports whether the session is mid-turn, to choose the delivery
// mode for runTurn. It reads the pane FRESH (not just the activity cache) so a
// stale entry can't misroute delivery; on a read failure it falls back to the
// cache, else assumes idle (the --wait path, which gates for readiness anyway).
func (s *server) sessionBusy(ctx context.Context, id string) bool {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	pane, err := s.sessionPane(pctx, id)
	if err != nil {
		if st, ok := s.acts.get(id); ok {
			return st.Working
		}
		return false
	}
	parsed := parseActivity(pane)
	s.acts.update(id, parsed) // keep the cache warm with this read
	return parsed.Working
}

// printingSlashes are slash commands that render output INTO the pane (rather
// than just acting, like /clear). For these we capture the pane after a short
// settle and return the (ANSI-stripped) text so the PWA can show it. Keep this
// conservative: an unknown slash is treated as non-printing.
var printingSlashes = map[string]bool{
	"/context": true,
	"/cost":    true,
	"/status":  true,
	"/model":   true,
	"/help":    true,
}

// slashCaptureSettle is how long we wait after sending a printing slash before
// snapshotting the pane, giving the TUI time to render the output.
const slashCaptureSettle = 1200 * time.Millisecond

// isPrintingSlash reports whether text (a slash command line) is in the
// printing allowlist. It matches on the first whitespace-delimited token so
// "/model sonnet" still resolves to "/model".
func isPrintingSlash(text string) bool {
	cmd := strings.TrimSpace(text)
	if i := strings.IndexAny(cmd, " \t"); i >= 0 {
		cmd = cmd[:i]
	}
	return printingSlashes[strings.ToLower(cmd)]
}

// runSlash delivers a slash command (e.g. /clear, /compact) using the CLI's
// DEFAULT send mode — NOT --wait and NOT --no-wait. Default mode runs the
// readiness + slash-registration gate (#966) then sends, and returns WITHOUT
// waiting for a reply: slash commands produce no assistant message (and /clear
// even starts a fresh session), so --wait would return a stale/wrong reply.
// --no-wait would skip the gate and risk the slash being dropped.
//
// For printing slashes (/context, /cost, …) the output lands in the pane rather
// than as an assistant message, so after sending we capture the pane and attach
// the diff text to the slash-result event.
func (s *server) runSlash(se sessionInfo, text, reqID, prof string) {
	ctx, cancel := context.WithTimeout(withProfile(context.Background(), prof), 90*time.Second)
	defer cancel()
	log.Printf("slash: START session=%s req=%s cmd=%q", se.ID, reqID, text)

	// Snapshot the pane BEFORE sending so we can tell whether a printing slash
	// actually changed the screen (and only return text if it did).
	printing := isPrintingSlash(text)
	var before string
	if printing {
		if p, perr := s.sessionPane(ctx, se.ID); perr == nil {
			before = stripANSI(p)
		}
	}

	// --timeout bounds the readiness/gate wait (a busy session); default mode
	// (no --wait/--no-wait) gates + sends, then prints "Sent message" and returns.
	_, err := s.adeckSend(ctx, "session", "send", se.ID, text, "--timeout", "30s")
	if err != nil {
		log.Printf("slash: ERROR session=%s req=%s: %v", se.ID, reqID, err)
		ev := map[string]any{
			"type": "slash-result", "requestId": reqID, "sessionId": se.ID,
			"command": text, "error": err.Error(), "ts": time.Now().Unix(),
		}
		for k, v := range sendErrorFields(err) {
			ev[k] = v
		}
		s.hub.publish(ev)
		return
	}

	event := map[string]any{
		"type": "slash-result", "requestId": reqID, "sessionId": se.ID,
		"command": text, "ok": true, "ts": time.Now().Unix(),
	}
	if printing {
		if out := s.captureSlashOutput(ctx, se.ID, before); out != "" {
			event["output"] = out
		}
	}
	log.Printf("slash: OK session=%s req=%s cmd=%q", se.ID, reqID, text)
	s.hub.publish(event)
}

// captureSlashOutput waits a short settle then snapshots the pane, returning the
// ANSI-stripped capture only when it differs from `before` (so we never echo a
// stale/unchanged screen). Empty string means "nothing new to show".
func (s *server) captureSlashOutput(ctx context.Context, id, before string) string {
	select {
	case <-ctx.Done():
		return ""
	case <-time.After(slashCaptureSettle):
	}
	raw, err := s.sessionPane(ctx, id)
	if err != nil {
		return ""
	}
	after := strings.TrimRight(stripANSI(raw), "\n")
	if after == "" || strings.TrimRight(before, "\n") == after {
		return "" // pane did not change — be conservative, return nothing
	}
	return after
}
