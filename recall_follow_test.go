package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// followBin is a fake agent-deck: `recall timeline` answers from the canned
// JSON, `recall follow` runs followBody (shell) and logs every argv.
func followBin(t *testing.T, tail, since, followBody string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "agent-deck"), filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  *\"recall follow\"*)\n" + followBody + "\n;;\n" +
		"  *--tail*) cat <<'EOF'\n" + tail + "\nEOF\n;;\n" +
		"  *--since*) cat <<'EOF'\n" + since + "\nEOF\n;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

const followTail = `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n1"},"turns":[
{"id":"u1","kind":"user","ts":"2026-10-20T11:10:00.000Z","body":"hello"}
],"through_cursor":"c1","before_cursor":""}`

func contents(msgs []histMsg) string {
	var c []string
	for _, m := range msgs {
		c = append(c, m.Content)
	}
	return strings.Join(c, "|")
}

// A healthy follower replaces the poll: N history requests cost one tail read
// and one follow process, rows arrive by frame, and no --since is ever run.
func TestFollowReplacesPolling(t *testing.T) {
	frames := `printf '%s\n' '{"frame":"row","row":{"id":"as1","kind":"assistant","ts":"2026-10-20T11:10:05.000Z","body":"live reply"},"cursor":"c2"}'
exec sleep 30`
	bin, log := followBin(t, followTail, followTail, frames)
	s := testServer(t, config{bin: bin, token: "tok"})
	s.recall.follow = true
	defer func() {
		s.recall.mu.Lock()
		for _, sv := range s.recall.sessions {
			sv.mu.Lock()
			sv.stopFollow()
			sv.mu.Unlock()
		}
		s.recall.mu.Unlock()
	}()
	ca := claudeAdapter{s: s}
	se := sessionInfo{ID: "s1", Tool: "claude"}
	ctx := context.Background()

	if _, _, ok := ca.historyWindow(ctx, se, 5); !ok {
		t.Fatal("recall path not used")
	}
	waitFor(t, 5*time.Second, "live row", func() bool {
		msgs, _, _ := ca.historyWindow(ctx, se, 5)
		return contents(msgs) == "hello|live reply"
	})
	for i := 0; i < 20; i++ { // a 3 s-poll sheet for a minute
		ca.historyWindow(ctx, se, 5)
		time.Sleep(5 * time.Millisecond)
	}
	argv, _ := os.ReadFile(log)
	if strings.Contains(string(argv), "--since") {
		t.Errorf("a healthy follower must not poll:\n%s", argv)
	}
	if n := s.execAdeck.Load(); n != 2 {
		t.Errorf("execs = %d, want 2 (tail + follow)\n%s", n, argv)
	}
	if !strings.Contains(string(argv), "recall follow --jsonl --after c1 s1") {
		t.Errorf("follow must resume from the tail cursor:\n%s", argv)
	}
	if n := s.recall.nFollowers.Load(); n != 1 {
		t.Errorf("followers = %d", n)
	}
}

// A follower that dies at once (build without `follow`, recall flipped off)
// hands the session back to polling; nothing goes stale.
func TestFollowFallsBackToPolling(t *testing.T) {
	old := recallFollowBackoff0
	recallFollowBackoff0 = 5 * time.Millisecond
	defer func() { recallFollowBackoff0 = old }()
	since := `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n1"},"turns":[
{"id":"as1","kind":"assistant","ts":"2026-10-20T11:10:05.000Z","body":"polled reply"}],"through_cursor":"c2"}`
	bin, log := followBin(t, followTail, since, "exit 1")
	s := testServer(t, config{bin: bin, token: "tok"})
	s.recall.follow = true
	ca := claudeAdapter{s: s}
	se := sessionInfo{ID: "s1", Tool: "claude"}
	ctx := context.Background()
	ca.historyWindow(ctx, se, 5)
	waitFor(t, 5*time.Second, "follower given up", func() bool { return s.recall.nFollowers.Load() == 0 })
	// recallMinInterval has not elapsed in test time: age the fetch so the poll runs.
	for _, sv := range s.recall.sessions {
		sv.mu.Lock()
		sv.lastFetch = time.Now().Add(-time.Minute)
		sv.mu.Unlock()
	}
	msgs, _, ok := ca.historyWindow(ctx, se, 5)
	if !ok || contents(msgs) != "hello|polled reply" {
		t.Fatalf("fallback poll: ok=%v %q", ok, contents(msgs))
	}
	argv, _ := os.ReadFile(log)
	if n := strings.Count(string(argv), "recall follow"); n != recallFollowFastExits {
		t.Errorf("follow spawns = %d, want %d fast exits then stop\n%s", n, recallFollowFastExits, argv)
	}
}

// resync_required (transcript rewritten) drops the cache and reopens with a tail read.
func TestFollowResyncReopens(t *testing.T) {
	frames := `printf '%s\n' '{"frame":"resync_required","reason":"source_rewritten"}'
exec sleep 30`
	bin, log := followBin(t, followTail, followTail, frames)
	s := testServer(t, config{bin: bin, token: "tok"})
	s.recall.follow = true
	ca := claudeAdapter{s: s}
	se := sessionInfo{ID: "s1", Tool: "claude"}
	ca.historyWindow(context.Background(), se, 5)
	waitFor(t, 5*time.Second, "resync", func() bool {
		for _, sv := range s.recall.sessions {
			sv.mu.Lock()
			done := sv.through == ""
			sv.mu.Unlock()
			return done
		}
		return false
	})
	if msgs, _, ok := ca.historyWindow(context.Background(), se, 5); !ok || contents(msgs) != "hello" {
		t.Fatalf("reopen after resync: %v %q", ok, contents(msgs))
	}
	argv, _ := os.ReadFile(log)
	if n := strings.Count(string(argv), "--tail"); n != 2 {
		t.Errorf("tail reads = %d, want 2\n%s", n, argv)
	}
}

// /clear: the session's claude id no longer matches the cached conversation, so
// the follower (bound to the old file) is replaced by a fresh tail read.
func TestFollowRestartsOnClear(t *testing.T) {
	bin, log := followBin(t, followTail, followTail, "exec sleep 30")
	s := testServer(t, config{bin: bin, token: "tok"})
	s.recall.follow = true
	ca := claudeAdapter{s: s}
	ctx := context.Background()
	ca.historyWindow(ctx, sessionInfo{ID: "s1", Tool: "claude", ClaudeSessionID: "n1"}, 5)
	waitFor(t, 5*time.Second, "first follower", func() bool { return strings.Count(readFile(t, log), "recall follow") == 1 })
	ca.historyWindow(ctx, sessionInfo{ID: "s1", Tool: "claude", ClaudeSessionID: "n2"}, 5)
	argv, _ := os.ReadFile(log)
	if n := strings.Count(string(argv), "--tail"); n != 2 {
		t.Errorf("tail reads after /clear = %d, want 2\n%s", n, argv)
	}
	waitFor(t, 5*time.Second, "second follower", func() bool { return strings.Count(readFile(t, log), "recall follow") == 2 })
	for _, sv := range s.recall.sessions {
		sv.mu.Lock()
		sv.stopFollow()
		sv.mu.Unlock()
	}
}

// The follower count is capped; sessions over the cap keep polling.
func TestFollowCap(t *testing.T) {
	bin, _ := followBin(t, followTail, followTail, "exec sleep 30")
	s := testServer(t, config{bin: bin, token: "tok"})
	s.recall.follow = true
	ca := claudeAdapter{s: s}
	for i := 0; i < recallMaxFollowers+3; i++ {
		ca.historyWindow(context.Background(), sessionInfo{ID: "s" + string(rune('a'+i)), Tool: "claude"}, 5)
	}
	waitFor(t, 5*time.Second, "followers", func() bool { return s.recall.nFollowers.Load() == recallMaxFollowers })
	time.Sleep(50 * time.Millisecond)
	if n := s.recall.nFollowers.Load(); n != recallMaxFollowers {
		t.Errorf("followers = %d, want cap %d", n, recallMaxFollowers)
	}
	s.recall.mu.Lock()
	for _, sv := range s.recall.sessions {
		sv.mu.Lock()
		sv.stopFollow()
		sv.mu.Unlock()
	}
	s.recall.mu.Unlock()
}
