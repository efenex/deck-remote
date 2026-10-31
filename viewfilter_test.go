package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApplyView(t *testing.T) {
	now := time.Now()
	since := now.Add(-time.Hour).Format(time.RFC3339)
	recent := now.Add(-time.Minute).Format(time.RFC3339Nano)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	sessions := []sessionInfo{
		{ID: "open-recent", Status: "idle", LastActivityAt: recent},
		{ID: "open-old", Status: "idle", LastActivityAt: old},
		{ID: "stopped-recent", Status: "stopped", LastActivityAt: recent},
		{ID: "archived", Status: "idle", LastActivityAt: recent, Archived: true},
		{ID: "needs-old", Status: "waiting", LastActivityAt: old, NeedsAttention: true},
		{ID: "transcript-only", Status: "running", LastActivity: now.Unix()},
		{ID: "zero-web-time", Status: "running", LastActivityAt: "0001-01-01T00:00:00Z", LastActivity: now.Add(-72 * time.Hour).Unix()},
		{ID: "unknown-time", Status: "running"},
	}
	ids := func(ss []sessionInfo) map[string]bool {
		m := map[string]bool{}
		for _, s := range ss {
			m[s.ID] = true
		}
		return m
	}

	v := &viewFilter{StatusFilter: "active", Statuses: []string{"running", "waiting", "idle", "starting", "queued"},
		TimeFilter: "today", TimeFilterLabel: "Today", ActiveSince: since}
	got, info := applyView(append([]sessionInfo(nil), sessions...), v)
	want := map[string]bool{"open-recent": true, "needs-old": true, "transcript-only": true, "unknown-time": true}
	if g := ids(got); len(g) != len(want) {
		t.Fatalf("kept %v, want %v", g, want)
	} else {
		for id := range want {
			if !g[id] {
				t.Fatalf("kept %v, want %v", g, want)
			}
		}
	}
	if info.Label != "Open · Today" || info.Hidden != 4 {
		t.Fatalf("info = %+v", info)
	}

	got, info = applyView(append([]sessionInfo(nil), sessions...), nil)
	if len(got) != len(sessions) || info != (viewInfo{}) {
		t.Fatalf("nil filter changed the list: %d %+v", len(got), info)
	}

	got, _ = applyView(append([]sessionInfo(nil), sessions...), &viewFilter{StatusFilter: "archived", Statuses: []string{}, Archived: true})
	if g := ids(got); len(g) != 2 || !g["archived"] || !g["needs-old"] {
		t.Fatalf("archived view kept %v", g)
	}
}

// viewTestBin is a fake agent-deck whose `view --json` logs each call, sleeps,
// and either prints a filter or fails.
func viewTestBin(t *testing.T, sleep string, fail bool) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	body := "echo x >> " + calls + "\nsleep " + sleep + "\n"
	if fail {
		body += "exit 1\n"
	} else {
		body += `echo '{"status_filter":"active","statuses":["idle"]}'` + "\n"
	}
	bin = filepath.Join(dir, "agent-deck")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func countLines(t *testing.T, path string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "\n")
}

// Concurrent callers share one slow exec instead of queueing behind views.mu.
func TestViewFilterFor_ConcurrentCallersShareOneExec(t *testing.T) {
	bin, calls := viewTestBin(t, "0.5", false)
	s := askTestServer(t, bin)
	const n = 6
	start := time.Now()
	var wg sync.WaitGroup
	got := make([]*viewFilter, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); got[i] = s.viewFilterFor(context.Background()) }()
	}
	wg.Wait()
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("callers serialised behind the exec: %v", d)
	}
	if c := countLines(t, calls); c != 1 {
		t.Errorf("view exec ran %d times, want 1", c)
	}
	for i, v := range got {
		if v == nil || v.StatusFilter != "active" {
			t.Errorf("caller %d got %+v", i, v)
		}
	}
	// the lock is free while a refresh runs: a cached read never waits on it
	s.views.mu.Lock()
	s.views.mu.Unlock()
}

// A failing exec is cached longer than viewCacheTTL and backs off.
func TestViewFilterFor_FailureBacksOff(t *testing.T) {
	bin, calls := viewTestBin(t, "0", true)
	s := askTestServer(t, bin)
	ctx := context.Background()
	if v := s.viewFilterFor(ctx); v != nil {
		t.Fatalf("failed exec returned %+v", v)
	}
	// age the entry past a plain TTL: a failure must still be cached
	s.views.mu.Lock()
	e := s.views.m[s.cfg.profile]
	if rem := time.Until(e.until); rem <= viewCacheTTL {
		s.views.mu.Unlock()
		t.Fatalf("failure cached for %v, want longer than %v", rem, viewCacheTTL)
	}
	e.until = time.Now().Add(-time.Millisecond)
	s.views.m[s.cfg.profile] = e
	s.views.mu.Unlock()
	s.viewFilterFor(ctx) // expired: retried once, backoff doubles
	s.viewFilterFor(ctx) // cached again
	if c := countLines(t, calls); c != 2 {
		t.Errorf("exec ran %d times, want 2", c)
	}
	s.views.mu.Lock()
	if f := s.views.m[s.cfg.profile].fails; f != 2 {
		t.Errorf("fails = %d, want 2", f)
	}
	s.views.mu.Unlock()
}
