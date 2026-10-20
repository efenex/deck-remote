package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTmuxPacingIsSustainedNotBursty(t *testing.T) {
	b := newTmuxBudget()
	b.interval = 30 * time.Millisecond
	ctx := context.Background()
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = b.acquire(ctx) }()
	}
	wg.Wait()
	// six slots: the first is immediate, five more follow 30 ms apart
	if d := time.Since(start); d < 5*30*time.Millisecond-5*time.Millisecond {
		t.Errorf("6 acquires took %v: not paced", d)
	}
}

func TestTmuxRefusesWhenSlotPastDeadline(t *testing.T) {
	b := newTmuxBudget()
	b.interval = time.Second
	if err := b.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.acquire(ctx); !errors.Is(err, errTmuxThrottled) {
		t.Fatalf("err = %v, want throttled", err)
	}
	if time.Since(start) > 50*time.Millisecond || b.throttled.Load() != 1 {
		t.Errorf("a refusal must be immediate and counted (took %v, throttled=%d)", time.Since(start), b.throttled.Load())
	}
	// the refused caller consumed no slot: the next one is still one interval after the first
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	if err := b.acquire(ctx2); err != nil {
		t.Errorf("refusal must not consume a slot: %v", err)
	}
}

func TestTmuxBackoffDoublesCapsAndResets(t *testing.T) {
	b := newTmuxBudget()
	var got []time.Duration
	for i := 0; i < 7; i++ {
		b.fault()
		got = append(got, b.backoff)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff steps = %v", got)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.acquire(ctx); !errors.Is(err, errTmuxThrottled) {
		t.Errorf("an exec inside the backoff window must be refused, got %v", err)
	}
	b.ok()
	if b.backoff != 0 || !b.until.IsZero() {
		t.Error("success resets the backoff")
	}
	if err := b.acquire(context.Background()); err != nil {
		t.Error(err)
	}
}

// fakeTmux logs argv; list-panes lists "alive"; capture-pane answers "pane text";
// show-environment fails like an unset variable.
func fakeTmux(t *testing.T, body string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "tmux"), filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestDeadSessionsCostNoExec(t *testing.T) {
	bin, log := fakeTmux(t, `case "$*" in
  *list-panes*) printf 'alive\nalive\nother\n';;
  *capture-pane*) echo "pane text";;
  *) exit 1;;
esac`)
	s := testServer(t, config{token: "tok", tmuxBin: bin, bin: "/nonexistent"})
	sessions := []sessionInfo{
		{ID: "a", Tool: "claude", TmuxSession: "alive"},
		{ID: "d1", Tool: "claude", TmuxSession: "dead1"},
		{ID: "d2", Tool: "claude", TmuxSession: "dead2"},
	}
	ctx := context.Background()
	s.refreshLive(ctx, sessions)
	for _, se := range sessions {
		_, _ = s.paneFor(ctx, se)
		_, _ = s.tmuxEnvironment(ctx, se.TmuxSession, "CLAUDE_SESSION_ID")
	}
	argv := readFile(t, log)
	if n := strings.Count(argv, "list-panes"); n != 1 {
		t.Errorf("list-panes = %d, want one per socket\n%s", n, argv)
	}
	if n := strings.Count(argv, "capture-pane"); n != 1 || strings.Contains(argv, "dead") {
		t.Errorf("only the live session is captured:\n%s", argv)
	}
	if n := strings.Count(argv, "show-environment"); n != 1 {
		t.Errorf("show-environment only for the live session:\n%s", argv)
	}
	if s.execAdeck.Load() != 0 {
		t.Error("a session known to be gone must not fall back to the agent-deck CLI")
	}
	if got := s.execTmux.Load(); got != 3 {
		t.Errorf("tmux execs = %d, want 3", got)
	}
}

func TestOneListPanesPerSocket(t *testing.T) {
	bin, log := fakeTmux(t, `printf 's\n'`)
	s := testServer(t, config{token: "tok", tmuxBin: bin})
	s.refreshLive(context.Background(), []sessionInfo{
		{TmuxSession: "a"}, {TmuxSession: "b"}, {TmuxSession: "c", TmuxSocket: "x"}, {TmuxSession: "d", TmuxSocket: "x"}, {ID: "nonmux"},
	})
	argv := readFile(t, log)
	if strings.Count(argv, "list-panes") != 2 || !strings.Contains(argv, "-L x list-panes") {
		t.Errorf("want one list-panes on default and one on -L x:\n%s", argv)
	}
}

func TestNoServerMeansGoneAndBacksOff(t *testing.T) {
	bin, log := fakeTmux(t, `echo "no server running on /tmp/tmux-501/default" >&2; exit 1`)
	s := testServer(t, config{token: "tok", tmuxBin: bin, bin: "/nonexistent"})
	se := sessionInfo{ID: "a", Tool: "claude", TmuxSession: "alive"}
	s.refreshLive(context.Background(), []sessionInfo{se})
	if !s.tmux.gone("", "alive") {
		t.Error("no tmux server: every session on the socket is gone")
	}
	if s.tmux.faults.Load() != 1 || s.tmux.backoff != time.Second {
		t.Errorf("faults=%d backoff=%v", s.tmux.faults.Load(), s.tmux.backoff)
	}
	// inside the backoff window another exec is refused without running tmux
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.refreshLive(ctx, []sessionInfo{se})
	if n := strings.Count(readFile(t, log), "list-panes"); n != 1 {
		t.Errorf("list-panes ran %d times; the second must be throttled", n)
	}
	if s.tmux.throttled.Load() != 1 {
		t.Errorf("throttled = %d", s.tmux.throttled.Load())
	}
}

func TestThrottledCaptureDoesNotFallBackToCLI(t *testing.T) {
	bin, _ := fakeTmux(t, `echo pane`)
	s := testServer(t, config{token: "tok", tmuxBin: bin, bin: "/nonexistent"})
	s.tmux.fault()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.paneFor(ctx, sessionInfo{ID: "a", Tool: "claude", TmuxSession: "x"})
	if !errors.Is(err, errTmuxThrottled) || s.execAdeck.Load() != 0 {
		t.Errorf("err=%v adeck execs=%d: a throttled capture must not spend an agent-deck exec", err, s.execAdeck.Load())
	}
}

func TestSweepOrderHotThenOldest(t *testing.T) {
	b := newTmuxBudget()
	b.paneAt["old"] = time.Now().Add(-time.Minute)
	b.paneAt["new"] = time.Now()
	ss := []sessionInfo{{ID: "new"}, {ID: "old"}, {ID: "never"}, {ID: "run", Status: "running"}}
	b.paneAt["run"] = time.Now()
	b.order(ss)
	var ids []string
	for _, s := range ss {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "run,never,old,new" {
		t.Errorf("order = %v", ids)
	}
}
