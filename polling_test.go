package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The CLI polling burn regression tests: the hot paths (session list, last
// reply) must be served without exec'ing agent-deck.

func TestMapMenuSession(t *testing.T) {
	m := menuSessionWire{
		ID: "id1", Title: "t", Tool: "claude", Status: "running",
		GroupPath: "g", ProjectPath: "/p", TmuxSession: "agentdeck_t_ab",
		TmuxSocketName: "sock", Model: "opus", ClaudeSessionID: "c-uuid",
		CodexSessionID: "x-uuid", SSHHost: "host",
	}
	se := mapMenuSession(m, "default")
	if se.ID != "id1" || se.Path != "/p" || se.Group != "g" || se.TmuxSession != "agentdeck_t_ab" ||
		se.TmuxSocket != "sock" || se.ClaudeSessionID != "c-uuid" || se.CodexSessionID != "x-uuid" ||
		se.SSHHost != "host" || se.Profile != "default" || se.Status != "running" {
		t.Fatalf("bad mapping: %+v", se)
	}
}

func TestListSessionsHTTPWithCache(t *testing.T) {
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits++
		fmt.Fprint(w, `{"profile":"default","sessions":[{"id":"a","title":"A","tool":"claude","tmuxSession":"tm_a","claudeSessionId":"cid"}]}`)
	}))
	defer ts.Close()

	s := testServer(t, config{agentdeckURL: ts.URL, token: "tok", bin: "/nonexistent-agent-deck"})
	ctx := context.Background()

	got, err := s.listSessions(ctx)
	if err != nil || len(got) != 1 || got[0].ID != "a" || got[0].ClaudeSessionID != "cid" {
		t.Fatalf("listSessions: %v %+v", err, got)
	}
	// Mutating the returned slice must not poison the cache.
	got[0].LastReply = "mutated"
	again, err := s.listSessions(ctx)
	if err != nil || again[0].LastReply != "" {
		t.Fatalf("cache poisoned or errored: %v %+v", err, again)
	}
	if hits != 1 {
		t.Fatalf("expected 1 upstream hit (TTL cache), got %d", hits)
	}

	// A non-matching profile must NOT be served from the web instance; with the
	// CLI unavailable it errors instead of silently serving the wrong profile.
	if _, err := s.listSessions(withProfile(ctx, "other")); err == nil {
		t.Fatal("expected error for profile the web instance does not serve")
	}
	if hits != 2 {
		t.Fatalf("expected the profile miss to consult upstream once, got %d", hits)
	}
}

func TestTmuxSessionGone(t *testing.T) {
	gone := []string{
		"tmux capture-pane x: exit status 1: can't find session: x",
		"tmux capture-pane x: exit status 1: no server running on /private/tmp/tmux-501/default",
		"tmux capture-pane x: error connecting to /private/tmp/tmux-501/default (No such file or directory)",
	}
	for _, m := range gone {
		if !tmuxSessionGone(fmt.Errorf("%s", m)) {
			t.Fatalf("should be definitive-dead: %s", m)
		}
	}
	if tmuxSessionGone(fmt.Errorf("no tmux session name")) || tmuxSessionGone(nil) {
		t.Fatal("non-definitive errors must still allow the CLI fallback")
	}
}

func writeTranscript(t *testing.T, dir, id string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assistantLine(ts, text string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, ts, text)
}

func TestLastReplyFromTranscript(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "sess",
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		assistantLine("2026-07-15T10:00:00Z", "first"),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
		assistantLine("2026-07-15T10:05:00Z", "second"),
		`{"type":"user","message":{"role":"user","content":"ok"}}`,
	)
	out, err := lastReplyFromTranscript(path, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "second" || out.Timestamp != "2026-07-15T10:05:00Z" || out.ClaudeSessionID != "sess" {
		t.Fatalf("bad reply: %+v", out)
	}
}

func TestClaudeReplyTranscriptMtimeGate(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tdir := filepath.Join(cfgDir, "projects", convertToClaudeDirName(project))
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, tdir, "cid-1", assistantLine("2026-07-15T10:00:00Z", "one"))

	s := testServer(t, config{bin: "/nonexistent-agent-deck", token: "tok"})
	se := sessionInfo{ID: "s1", Tool: "claude", Path: project, ClaudeSessionID: "cid-1"}

	out, err := s.claudeReply(context.Background(), se)
	if err != nil || out.Content != "one" {
		t.Fatalf("first read: %v %+v", err, out)
	}

	// Append a newer reply with a changed mtime; the cache must notice.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(assistantLine("2026-07-15T10:06:00Z", "two") + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	out, err = s.claudeReply(context.Background(), se)
	if err != nil || out.Content != "two" {
		t.Fatalf("after append: %v %+v", err, out)
	}

	// No transcript and no CLI -> error (the exec fallback is the last resort).
	if _, err := s.claudeReply(context.Background(), sessionInfo{ID: "s2", Tool: "claude"}); err == nil {
		t.Fatal("expected error when neither transcript nor CLI is available")
	}
}

// A stale list claudeSessionId resolving to an empty transcript must fall back
// to the CLI once, learn the real id from its output, and serve subsequent
// polls from the real transcript with no further execs.
func TestClaudeReplyStaleIDCorrectedViaCLI(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tdir := filepath.Join(cfgDir, "projects", convertToClaudeDirName(project))
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The stale conversation: transcript exists but holds no assistant reply.
	writeTranscript(t, tdir, "cid-stale", `{"type":"user","message":{"role":"user","content":"hi"}}`)
	// The real conversation the CLI knows about.
	writeTranscript(t, tdir, "cid-real", assistantLine("2026-07-15T11:00:00Z", "real reply"))

	fakeBin := filepath.Join(t.TempDir(), "agent-deck")
	script := "#!/bin/sh\necho '{\"claude_session_id\":\"cid-real\",\"content\":\"cli reply\",\"role\":\"assistant\",\"timestamp\":\"2026-07-15T11:00:00Z\"}'\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s := testServer(t, config{bin: fakeBin, token: "tok"})
	se := sessionInfo{ID: "s1", Tool: "claude", Path: project, ClaudeSessionID: "cid-stale"}

	out, err := s.claudeReply(context.Background(), se)
	if err != nil || out.Content != "cli reply" {
		t.Fatalf("stale-id call should serve the CLI reply: %v %+v", err, out)
	}
	if n := s.execAdeck.Load(); n != 1 {
		t.Fatalf("expected exactly 1 CLI exec, got %d", n)
	}
	out, err = s.claudeReply(context.Background(), se)
	if err != nil || out.Content != "real reply" {
		t.Fatalf("second call should serve the corrected transcript: %v %+v", err, out)
	}
	if n := s.execAdeck.Load(); n != 1 {
		t.Fatalf("steady state must be exec-free, got %d execs", n)
	}
}
