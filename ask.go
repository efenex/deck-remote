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
	if req.SessionID == "" || req.Text == "" {
		httpError(w, http.StatusBadRequest, "sessionId and text required")
		return
	}
	if slash && !strings.HasPrefix(req.Text, "/") {
		req.Text = "/" + req.Text
	}

	// Resolve to a concrete session id and reject unknown sessions early.
	rctx, cancel := cliCtx(withProfile(r.Context(), req.Profile), 10*time.Second)
	se, err := s.findSession(rctx, req.SessionID)
	cancel()
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
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

// runTurn sends the prompt with --wait in a background goroutine (off the HTTP
// request) so a multi-minute turn is fine, then publishes the reply over SSE.
// --wait reuses agent-deck's own turn-completion + fresh-output logic, so we
// don't reimplement busy/idle detection.
func (s *server) runTurn(se sessionInfo, text, reqID, prof string) {
	ctx, cancel := context.WithTimeout(withProfile(context.Background(), prof), maxTurn)
	defer cancel()

	log.Printf("ask: runTurn START session=%s req=%s text=%q", se.ID, reqID, text)
	out, err := s.adeck(ctx, "session", "send", se.ID, text,
		"--wait", "--timeout", fmt.Sprintf("%.0fs", maxTurn.Seconds()))
	if err != nil {
		log.Printf("ask: runTurn ERROR session=%s req=%s: %v", se.ID, reqID, err)
		s.hub.publish(map[string]any{
			"type": "reply", "requestId": reqID, "sessionId": se.ID,
			"error": err.Error(), "ts": time.Now().Unix(),
		})
		return
	}
	reply := cleanReplyContent(strings.TrimRight(string(out), "\n"))
	log.Printf("ask: runTurn OK session=%s req=%s replyLen=%d", se.ID, reqID, len(reply))
	s.hub.publish(map[string]any{
		"type": "reply", "requestId": reqID, "sessionId": se.ID,
		"content": reply, "ts": time.Now().Unix(),
	})
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
	_, err := s.adeck(ctx, "session", "send", se.ID, text, "--timeout", "30s")
	if err != nil {
		log.Printf("slash: ERROR session=%s req=%s: %v", se.ID, reqID, err)
		s.hub.publish(map[string]any{
			"type": "slash-result", "requestId": reqID, "sessionId": se.ID,
			"command": text, "error": err.Error(), "ts": time.Now().Unix(),
		})
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
