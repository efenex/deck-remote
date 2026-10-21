package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func menuFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/claude/question-menu.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An AskUserQuestion menu (captured from a live session parked on one, which
// deck-remote showed as plain idle) is a pending question, not an approval.
func TestClaudeQuestionMenuDetected(t *testing.T) {
	// background-task, shortcut and status rows may sit under the footer
	pane := strings.Repeat("earlier output\n", 40) + menuFixture(t) + "  ⏵⏵ auto mode on (shift+tab to cycle)\n  1 background task\n  ? for shortcuts\n"
	if isClaudePermissionPrompt(pane) {
		t.Fatal("a question menu must not read as a permission dialog")
	}
	info, err := claudeAdapter{}.Attention(context.Background(), sessionInfo{}, pane, true)
	if err != nil || !info.Pending || info.Kind != "question" || len(info.Questions) != 1 {
		t.Fatalf("attention = %+v, %v", info, err)
	}
	q := info.Questions[0]
	if q.Header != "Plan" || !strings.Contains(q.Question, "Which do I follow?") {
		t.Errorf("header/question = %q / %q", q.Header, q.Question)
	}
	if len(q.Options) != 5 || q.Options[0].Label != "Relayed plan (Recommended)" || !strings.Contains(q.Options[0].Description, "fresh context") {
		t.Errorf("options = %+v", q.Options)
	}
	if len(info.Actions) != 0 {
		t.Errorf("a menu is read-only here, got actions %v", info.Actions)
	}
}

// Moving the highlight in the terminal must not mint a new request id.
func TestClaudeQuestionIDIgnoresCursor(t *testing.T) {
	a, _ := claudeQuestionInfo(menuFixture(t))
	moved := strings.Replace(strings.Replace(menuFixture(t), "❯ 1.", "  1.", 1), "  2. Branch", "❯ 2. Branch", 1)
	b, ok := claudeQuestionInfo(moved)
	if !ok || a.ID != b.ID {
		t.Errorf("ids differ across cursor moves: %q vs %q (ok=%v)", a.ID, b.ID, ok)
	}
}

// Neither an idle prompt, a quoted footer in scrollback, nor a permission
// dialog is a question menu.
func TestClaudeQuestionMenuNegatives(t *testing.T) {
	for name, pane := range map[string]string{
		"idle":       idlePrompt,
		"scrollback": menuFixture(t) + strings.Repeat("⏺ more work\n", 30) + idlePrompt,
		"permission": claudeDialog,
	} {
		if isClaudeQuestionMenu(pane) {
			t.Errorf("%s read as a question menu", name)
		}
	}
	if !isClaudePermissionPrompt(claudeDialog) {
		t.Error("permission dialog fixture no longer detected")
	}
}

// The approval path must never act on a question menu: no keys are sent.
func TestClaudeRespondRejectsQuestionMenu(t *testing.T) {
	s := &server{}
	_, err := claudeAdapter{s}.RespondAttention(context.Background(), sessionInfo{}, attentionResponseRequest{Action: "decline"})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("decline on Claude should be refused with a terminal hint, got %v", err)
	}
}

// The 2026-10-21 budget change must not hide a parked menu: a sweep reads it
// into the attention cache (which feeds needsAttention in the session list), and
// a later sweep that runs out of tmux budget keeps that answer instead of
// clearing it; the on-demand endpoint says "unavailable", not "nothing pending".
func TestMenuSurvivesTmuxThrottling(t *testing.T) {
	menu := filepath.Join(t.TempDir(), "menu.txt")
	if err := os.WriteFile(menu, []byte(menuFixture(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, _ := fakeTmux(t, `case "$*" in
  *list-panes*) echo menu-sess;;
  *capture-pane*) cat `+menu+`;;
  *) exit 1;;
esac`)
	s := testServer(t, config{token: "tok", tmuxBin: bin, bin: "/nonexistent"})
	se := sessionInfo{ID: "m1", Title: "menu", Tool: "claude", TmuxSession: "menu-sess", Status: "waiting"}
	s.lists.mu.Lock()
	s.lists.m["default"] = listCacheEntry{sessions: []sessionInfo{se}, at: time.Now()}
	s.lists.mu.Unlock()

	ctx := context.Background()
	s.sweep(ctx, map[string]*sessWatch{}, false)
	info, ok := s.attn.get("m1")
	if !ok || !info.Pending || info.Kind != "question" {
		t.Fatalf("sweep did not surface the menu: %+v ok=%v", info, ok)
	}

	// Out of budget: the next capture is refused without running anything.
	s.tmux.mu.Lock()
	s.tmux.until = time.Now().Add(time.Minute)
	s.tmux.mu.Unlock()
	before := s.execTmux.Load()
	s.sweep(ctx, map[string]*sessWatch{}, false)
	if s.execTmux.Load() != before || s.tmux.throttled.Load() == 0 {
		t.Fatalf("expected a throttled sweep (execs %d→%d, throttled %d)", before, s.execTmux.Load(), s.tmux.throttled.Load())
	}
	if info, _ := s.attn.get("m1"); !info.Pending || info.Kind != "question" {
		t.Errorf("a throttled sweep cleared the cached menu: %+v", info)
	}

	// The on-demand endpoint cannot verify under throttling: it must say so.
	req := httptest.NewRequest("GET", "/api/rc/attention?id=m1", nil)
	rec := httptest.NewRecorder()
	rctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
	defer cancel()
	s.handleAttention(rec, req.WithContext(rctx))
	if !strings.Contains(rec.Body.String(), `"unavailable":true`) {
		t.Errorf("throttled /attention = %s, want unavailable", rec.Body.String())
	}

	// Budget back: the menu comes through the endpoint with its options.
	s.tmux.ok()
	rec = httptest.NewRecorder()
	s.handleAttention(rec, httptest.NewRequest("GET", "/api/rc/attention?id=m1", nil))
	if !strings.Contains(rec.Body.String(), `"kind":"question"`) || !strings.Contains(rec.Body.String(), "Relayed plan (Recommended)") {
		t.Errorf("/attention = %s", rec.Body.String())
	}
}

// A single-question menu stays read-only: "answer" against it is refused before any key (the
// bogus agent-deck binary would fail loudly with a different error if a key were attempted).
func TestSingleQuestionMenuNotAnswerable(t *testing.T) {
	menu := filepath.Join(t.TempDir(), "menu.txt")
	if err := os.WriteFile(menu, []byte(menuFixture(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, _ := fakeTmux(t, `case "$*" in
  *list-panes*) echo menu-sess;;
  *capture-pane*) cat `+menu+`;;
  *) exit 1;;
esac`)
	s := testServer(t, config{token: "tok", tmuxBin: bin, bin: "/nonexistent"})
	se := sessionInfo{ID: "m1", Title: "menu", Tool: "claude", TmuxSession: "menu-sess"}
	s.lists.mu.Lock()
	s.lists.m["default"] = listCacheEntry{sessions: []sessionInfo{se}, at: time.Now()}
	s.lists.mu.Unlock()
	info, _ := claudeAdapter{s}.Attention(context.Background(), se, menuFixture(t), true)
	_, err := claudeAdapter{s}.RespondAttention(context.Background(), se, attentionResponseRequest{AttentionID: info.ID, Action: "answer"})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("answer on a read-only menu = %v", err)
	}
}
