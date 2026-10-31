package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A fake agent-deck CLI for the handler tests below: the test binary re-executes itself (see
// TestFakeAdeckHelper). The "menu" is a counter in a file: k digit presses so far on the
// four-question fixture; `session output` renders the sim at that point, `send-keys --text` takes
// a moment (a slow host) and then bumps the counter.
func fakeAdeckBinary(t *testing.T) (bin, state string) {
	t.Helper()
	dir := t.TempDir()
	bin, state = filepath.Join(dir, "agent-deck"), filepath.Join(dir, "presses")
	if err := os.WriteFile(state, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nFAKE_ADECK=1 FAKE_ADECK_STATE='" + state + "' exec '" + exe + "' -test.run='^TestFakeAdeckHelper$' -- \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, state
}

func fakeAdeckPresses(t *testing.T, state string) int {
	t.Helper()
	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func TestFakeAdeckHelper(t *testing.T) {
	if os.Getenv("FAKE_ADECK") != "1" {
		t.Skip("helper process for the handler tests")
	}
	state := os.Getenv("FAKE_ADECK_STATE")
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	joined := " " + strings.Join(args, " ") + " "
	b, _ := os.ReadFile(state)
	k, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	switch {
	case strings.Contains(joined, " output "):
		s := newAskSim(fourQuestionAsk())
		for i := 0; i < k; i++ {
			s.pressDigit(1)
		}
		fmt.Print(s.render())
	case strings.Contains(joined, " send-keys "):
		time.Sleep(120 * time.Millisecond)
		_ = os.WriteFile(state, []byte(strconv.Itoa(k+1)), 0o644)
	default:
		os.Exit(1)
	}
	os.Exit(0)
}

// answerHandlerEnv wires a server whose session s1 is parked on the four-question menu.
func answerHandlerEnv(t *testing.T) (s *server, state string, body []byte) {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	dir := filepath.Join(cfgDir, "projects", "-work-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ask := fourQuestionAsk()
	input := map[string]any{"questions": ask.Questions}
	rec := map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "tool_use", "id": ask.ToolUseID, "name": "AskUserQuestion", "input": input},
	}}}
	line, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, "cid-4.jsonl"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, state := fakeAdeckBinary(t)
	s = testServer(t, config{token: "tok", bin: bin, homeDir: t.TempDir()})
	se := sessionInfo{ID: "s1", Title: "four", Tool: "claude", Path: "/work/proj", ClaudeSessionID: "cid-4"}
	s.lists.mu.Lock()
	s.lists.m["default"] = listCacheEntry{sessions: []sessionInfo{se}, at: time.Now().Add(time.Hour)}
	s.lists.mu.Unlock()
	var answers []attentionAnswer
	for i, q := range ask.Questions {
		answers = append(answers, attentionAnswer{ID: fmt.Sprintf("q%d", i), Answer: q.Options[0].Label})
	}
	body, _ = json.Marshal(attentionResponseRequest{
		SessionID: "s1", AttentionID: "claude-askq-" + ask.ToolUseID, Action: "answer", Answers: answers,
	})
	return s, state, body
}

// 2026-10-31: the phone gave up after 12 s and the request context died between two tabs. The walk
// belongs to the host, not to the connection: it still finishes all four tabs and submits.
func TestAnswerWalkSurvivesCancelledRequest(t *testing.T) {
	s, state, body := answerHandlerEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/rc/attention/respond", bytes.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	time.AfterFunc(350*time.Millisecond, cancel) // mid-walk: about the second or third key
	s.handleAttentionRespond(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"responded":true`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if n := fakeAdeckPresses(t, state); n != 5 { // four answers + Submit
		t.Errorf("menu got %d key presses, want 5", n)
	}
	if ctx.Err() == nil {
		t.Error("the request was supposed to be cancelled mid-walk")
	}
}

// A second tap while a walk runs is refused for that tap only: no attention-result fan-out.
func TestBusyRefusalIsNotBroadcast(t *testing.T) {
	s, _, body := answerHandlerEnv(t)
	events := s.hub.subscribe()
	defer s.hub.unsubscribe(events)
	askRunning.Store("s1", true)
	defer askRunning.Delete("s1")
	rec := httptest.NewRecorder()
	s.handleAttentionRespond(rec, httptest.NewRequest("POST", "/api/rc/attention/respond", bytes.NewReader(body)))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already being sent") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	select {
	case ev := <-events:
		t.Fatalf("busy refusal was broadcast: %s", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
