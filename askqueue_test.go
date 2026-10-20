package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// askTestServer isolates deck-remote's state dir so tracked sends never touch
// the real ~/.agent-deck.
func askTestServer(t *testing.T, bin string) *server {
	t.Helper()
	t.Setenv("DECK_REMOTE_STATE_DIR", t.TempDir())
	return testServer(t, config{bin: bin, token: "tok", homeDir: t.TempDir()})
}

// fakeAdeckAskQueue stubs agent-deck for the ask path. `list` returns one idle
// Claude session "s1"; a `--wait` send blocks (a long turn); `--queue` sends
// return a queued record with an increasing send_id; `send-status` reports the
// state held in <dir>/state (default "queued"). Every invocation is logged.
func fakeAdeckAskQueue(t *testing.T) (bin, argvLog, stateFile string) {
	t.Helper()
	dir := t.TempDir()
	msgLog := filepath.Join(dir, "msgs.log")
	failFile := filepath.Join(dir, "fail")
	t.Setenv("FAKE_ADECK_DIR", dir)
	bin = filepath.Join(dir, "agent-deck")
	argvLog = filepath.Join(dir, "argv.log")
	stateFile = filepath.Join(dir, "state")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + argvLog + `
case "$*" in
*"list --json"*)
  echo '[{"id":"s1","title":"s1","tool":"claude","status":"running"}]'; exit 0;;
*"session output"*) echo ''; exit 0;;
*"send-status"*)
  id=$(echo "$*" | sed 's/.*send-status \([A-Z0-9]*\).*/\1/')
  st=$(cat ` + stateFile + ` 2>/dev/null || echo queued)
  echo "{\"send_id\":\"$id\",\"session_id\":\"s1\",\"state\":\"$st\",\"reason\":\"boom\"}"; exit 0;;
*"session send"*)
  msg=$(cat)
  printf '%s\n' "$msg" >> ` + msgLog + `
  [ -f ` + failFile + ` ] && { echo '{"success":false,"error":"target not running","delivery":"pane_gone"}'; exit 1; }
  case "$*" in
  *--wait*) sleep 30; exit 0;;
  *--queue*)
    n=$(grep -c -- '--queue' ` + argvLog + `)
    echo "{\"success\":true,\"delivery\":\"queued\",\"send_id\":\"01SEND$n\",\"state\":\"queued\",\"session_id\":\"s1\"}"; exit 0;;
  esac;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog, stateFile
}

func postAsk(t *testing.T, s *server, text string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/rc/ask", strings.NewReader(`{"sessionId":"s1","text":"`+text+`"}`))
	s.handleAsk(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ask %q: status %d body %s", text, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func sendLines(t *testing.T, argvLog string) []string {
	t.Helper()
	b, _ := os.ReadFile(argvLog)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "session send") {
			out = append(out, l)
		}
	}
	return out
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Regression (2026-10-20): the first ask to an idle session ran `send --wait`
// inside the per-session queue worker, blocking it for the whole turn. The asks
// that followed were accepted ("sent") but sat undelivered in memory behind it.
// Every ask must reach agent-deck promptly and in order, no matter what the
// first one's turn is doing.
func TestAsk_NeverBlockedByLongTurn(t *testing.T) {
	bin, argvLog, _ := fakeAdeckAskQueue(t)
	s := askTestServer(t, bin)
	for _, txt := range []string{"one", "two", "three", "four"} {
		postAsk(t, s, txt)
	}
	waitFor(t, 5*time.Second, "all 4 asks to reach agent-deck", func() bool {
		return len(sendLines(t, argvLog)) >= 4
	})
	for i, l := range sendLines(t, argvLog) {
		if strings.Contains(l, "--wait") {
			t.Errorf("send %d must not block on the turn: %s", i, l)
		}
		if !strings.Contains(l, "--queue") {
			t.Errorf("send %d should use agent-deck's durable queue: %s", i, l)
		}
	}
}

// Delivery order is arrival order, and each text reaches agent-deck once.
func TestAsk_DeliveredInOrderExactlyOnce(t *testing.T) {
	bin, argvLog, _ := fakeAdeckAskQueue(t)
	s := askTestServer(t, bin)
	want := []string{"first", "second", "third"}
	for _, txt := range want {
		postAsk(t, s, txt)
	}
	waitFor(t, 5*time.Second, "3 queued sends", func() bool { return len(sendLines(t, argvLog)) >= 3 })
	time.Sleep(100 * time.Millisecond)
	got := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(os.Getenv("FAKE_ADECK_DIR"), "msgs.log"))), "\n")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("delivery order = %v, want %v", got, want)
	}
}

type askEvent struct {
	State     string `json:"state"`
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Position  int    `json:"position"`
	Error     string `json:"error"`
	Retryable bool   `json:"retryable"`
}

func nextAskEvent(t *testing.T, ch chan []byte, d time.Duration) askEvent {
	t.Helper()
	select {
	case b := <-ch:
		var e askEvent
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		return e
	case <-time.After(d):
		t.Fatal("no event")
	}
	return askEvent{}
}

// The PWA sees sent -> queued (n) -> delivered, driven by send-status.
func TestAsk_StatusProgression(t *testing.T) {
	bin, _, stateFile := fakeAdeckAskQueue(t)
	s := askTestServer(t, bin)
	ch := s.hub.subscribe()
	r1 := postAsk(t, s, "a")["requestId"].(string)
	postAsk(t, s, "b")

	if e := nextAskEvent(t, ch, 3*time.Second); e.State != "sent" || e.RequestID != r1 {
		t.Fatalf("event 1 = %+v, want sent %s", e, r1)
	}
	var queued []askEvent
	for len(queued) < 2 {
		e := nextAskEvent(t, ch, 3*time.Second)
		if e.State == "queued" {
			queued = append(queued, e)
		}
	}
	pos := map[int]bool{}
	for _, e := range queued {
		pos[e.Position] = true
	}
	if !pos[1] || !pos[2] {
		t.Errorf("queued positions = %v, want 1 and 2", queued)
	}

	if err := os.WriteFile(stateFile, []byte("landed"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.pollSendsOnce()
	delivered := 0
	for delivered < 2 {
		if e := nextAskEvent(t, ch, 3*time.Second); e.State == "delivered" {
			delivered++
		}
	}
	s.sends.mu.Lock()
	left := len(s.sends.items)
	s.sends.mu.Unlock()
	if left != 0 {
		t.Errorf("%d finished sends still tracked", left)
	}
}

// A send agent-deck gives up on surfaces as failed + retryable with its reason.
func TestAsk_FailedIsRetryableWithReason(t *testing.T) {
	bin, _, stateFile := fakeAdeckAskQueue(t)
	s := askTestServer(t, bin)
	ch := s.hub.subscribe()
	postAsk(t, s, "x")
	nextAskEvent(t, ch, 3*time.Second) // sent
	nextAskEvent(t, ch, 3*time.Second) // queued
	if err := os.WriteFile(stateFile, []byte("failed"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.pollSendsOnce()
	e := nextAskEvent(t, ch, 3*time.Second)
	if e.State != "failed" || !e.Retryable || !strings.Contains(e.Error, "boom") {
		t.Fatalf("event = %+v", e)
	}
}

// agent-deck rejecting the enqueue itself (nothing typed) is reported failed.
func TestAsk_EnqueueRejectedIsFailed(t *testing.T) {
	bin, _, _ := fakeAdeckAskQueue(t)
	if err := os.WriteFile(filepath.Join(os.Getenv("FAKE_ADECK_DIR"), "fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s := askTestServer(t, bin)
	ch := s.hub.subscribe()
	postAsk(t, s, "x")
	nextAskEvent(t, ch, 3*time.Second) // sent
	e := nextAskEvent(t, ch, 3*time.Second)
	if e.State != "failed" || !e.Retryable || !strings.Contains(e.Error, "target not running") {
		t.Fatalf("event = %+v", e)
	}
}

func TestMapSendState(t *testing.T) {
	cases := []struct {
		in   queuedSend
		want uiSendState
	}{
		{queuedSend{State: "queued"}, uiSendState{State: "queued"}},
		{queuedSend{State: "typing"}, uiSendState{State: "delivering"}},
		{queuedSend{State: "typed"}, uiSendState{State: "delivering"}},
		{queuedSend{State: "submitted"}, uiSendState{State: "delivering"}},
		{queuedSend{State: "landed"}, uiSendState{State: "delivered", Final: true}},
		{queuedSend{State: "submitted", Settled: true}, uiSendState{State: "delivered", Final: true}},
		// typed but never confirmed: may be sitting in the composer -> no retry
		{queuedSend{State: "typed", Settled: true}, uiSendState{State: "unconfirmed", Final: true}},
		{queuedSend{State: "failed"}, uiSendState{State: "failed", Final: true, Retryable: true}},
	}
	for _, c := range cases {
		if got := mapSendState(c.in); got != c.want {
			t.Errorf("mapSendState(%+v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

// Open sends survive a deck-remote restart: the next process re-follows them
// (agent-deck's own queue keeps delivering regardless).
func TestSendTrackerResumesAfterRestart(t *testing.T) {
	bin, _, stateFile := fakeAdeckAskQueue(t)
	s := askTestServer(t, bin)
	ch := s.hub.subscribe()
	req := postAsk(t, s, "x")["requestId"].(string)
	nextAskEvent(t, ch, 3*time.Second)
	nextAskEvent(t, ch, 3*time.Second)

	s2 := testServer(t, config{bin: bin, token: "tok", homeDir: t.TempDir()}) // same DECK_REMOTE_STATE_DIR
	ch2 := s2.hub.subscribe()
	s2.sends.load()
	if len(s2.sends.items) != 1 || s2.sends.items[req] == nil {
		t.Fatalf("restarted tracker = %+v, want request %s", s2.sends.items, req)
	}
	if err := os.WriteFile(stateFile, []byte("landed"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2.pollSendsOnce()
	if e := nextAskEvent(t, ch2, 3*time.Second); e.State != "delivered" || e.RequestID != req {
		t.Fatalf("after restart event = %+v", e)
	}
	b, _ := os.ReadFile(s2.sends.path)
	if strings.Contains(string(b), req) {
		t.Errorf("finished send still persisted: %s", b)
	}
}
