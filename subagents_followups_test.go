package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Pause at the parser's first cancellation checkpoint, after source stats.
// This exercises real ingestion without adding production hooks or IO delays.
type childParseGate struct {
	context.Context
	once    sync.Once
	entered chan struct{}
	release <-chan struct{}
}

func (c *childParseGate) Err() error {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Context.Err()
}

type childErrCounter struct {
	context.Context
	calls atomic.Int32
}

func (c *childErrCounter) Err() error {
	c.calls.Add(1)
	return c.Context.Err()
}

func TestSubagentsConcurrentRootsShareOnlySameKey(t *testing.T) {
	f := newChildFixture(t)
	synctest.Test(t, func(t *testing.T) {
		var cache subagentCache
		release := make(chan struct{})
		defer close(release)
		owners := make(chan subagentList, 6)
		gates := make([]*childParseGate, 6)
		for i := range gates {
			gates[i] = &childParseGate{Context: context.Background(), entered: make(chan struct{}), release: release}
			go func() {
				owners <- cache.read(gates[i], "one", sessionInfo{ID: fmt.Sprint(i)}, f.parent, "parent-session", childTestNow)
			}()
		}
		synctest.Wait()
		for i, gate := range gates {
			select {
			case <-gate.entered:
			default:
				t.Fatalf("root %d blocked behind another root's parse", i)
			}
		}
		// Concurrent phones for root 0 must wait and reuse its eventual snapshot.
		waiterCtx := &childErrCounter{Context: context.Background()}
		waiters := make(chan subagentList, 12)
		for range cap(waiters) {
			go func() {
				waiters <- cache.read(waiterCtx, "one", sessionInfo{ID: "0"}, f.parent, "parent-session", childTestNow)
			}()
		}
		ctx, cancel := context.WithCancel(context.Background())
		canceled := make(chan subagentList, 1)
		go func() {
			canceled <- cache.read(ctx, "one", sessionInfo{ID: "0"}, f.parent, "parent-session", childTestNow)
		}()
		synctest.Wait()
		if len(waiters) != 0 || waiterCtx.calls.Load() != 0 {
			t.Fatal("same-key readers bypassed the in-flight owner")
		}
		cancel()
		synctest.Wait()
		if len(canceled) != 1 || (<-canceled).Availability != "unavailable" {
			t.Fatal("canceled waiter did not return promptly")
		}
		// Release each owner; deferred close also unblocks failed assertions.
		for range 6 {
			// Each owner is waiting for a single receive.
			release <- struct{}{}
		}
		synctest.Wait()
		if len(owners) != 6 || len(waiters) != cap(waiters) || waiterCtx.calls.Load() != 0 {
			t.Fatalf("owners=%d waiters=%d redundant parse checkpoints=%d", len(owners), len(waiters), waiterCtx.calls.Load())
		}
		var rootZero subagentList
		for range 6 {
			list := <-owners
			assertChild(t, list, "running", 1)
			if list.Root == "0" {
				rootZero = list
			}
		}
		for range cap(waiters) {
			if got := <-waiters; got.Revision != rootZero.Revision || got.Segment != rootZero.Segment {
				t.Fatal("same-key reader did not share snapshot")
			}
		}
		if len(cache.busy) != 0 || len(cache.entries) != 6 {
			t.Fatal("in-flight keys leaked or roots lost")
		}
	})
}

func TestSubagentsTmuxWaitDoesNotBlockOtherRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	f := newChildFixture(t)
	dir := filepath.Join(home, "projects", convertToClaudeDirName(home))
	mustMkdir(t, dir)
	putChildFile(t, filepath.Join(dir, "parent-session.jsonl"), childFixtureText(t, "parent.jsonl"))
	s := testServer(t, config{tmuxBin: "/usr/bin/false", homeDir: t.TempDir()})
	synctest.Test(t, func(t *testing.T) {
		s.tmux.next = time.Now().Add(10 * time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		se := sessionInfo{ID: "blocked", Path: home, ClaudeSessionID: "parent-session", TmuxSession: "fixture"}
		blocked := make(chan string, 1)
		go func() { blocked <- s.childNative(ctx, se, childTestNow) }()
		synctest.Wait()
		if len(blocked) != 0 {
			t.Fatal("fixture did not wait at tmux pacing gate")
		}
		other := make(chan string, 1)
		go func() {
			otherSession := se
			otherSession.ID, otherSession.TmuxSession = "other", ""
			other <- s.childNative(context.Background(), otherSession, childTestNow)
		}()
		parsed := make(chan subagentList, 1)
		go func() {
			parsed <- s.children.read(context.Background(), "one", sessionInfo{ID: "read"}, f.parent, "parent-session", childTestNow)
		}()
		synctest.Wait()
		if len(other) != 1 || (<-other) != "parent-session" || len(parsed) != 1 {
			t.Fatal("tmux wait serialized an unrelated resolver or parser")
		}
		assertChild(t, <-parsed, "running", 1)
		cancel()
		synctest.Wait()
		if len(blocked) != 1 || s.execTmux.Load() != 0 || len(s.children.bindings) != 1 {
			t.Fatal("canceled tmux wait executed or cached a binding")
		}
	})
}

func TestSubagentsCacheTTLAndStatReuse(t *testing.T) {
	f := newChildFixture(t)
	var cache subagentCache
	se := sessionInfo{ID: "root"}
	first := cache.read(context.Background(), "one", se, f.parent, "parent-session", childTestNow)
	counter := &childErrCounter{Context: context.Background()}
	cache.read(counter, "one", se, f.parent, "parent-session", childTestNow.Add(childCacheTTL-time.Nanosecond))
	if counter.calls.Load() != 0 {
		t.Fatal("read before six-second expiry touched sources")
	}
	list := cache.read(counter, "one", se, f.parent, "parent-session", childTestNow.Add(childCacheTTL))
	// Stat-only reuse checks cancellation once before publication; parsing
	// would also visit each parent record and discovered child.
	if counter.calls.Load() != 1 || list.Segment != first.Segment || !list.ObservedAt.Equal(childTestNow.Add(childCacheTTL)) {
		t.Fatal("unchanged source was parsed or observation was not renewed")
	}
}

func TestSubagentsNativeResolverOrderAndGone(t *testing.T) {
	for _, mode := range []string{"env", "confirmed", "list", "none", "gone-default", "gone-socket", "other-socket-live"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", home)
			dir := filepath.Join(home, "projects", convertToClaudeDirName(home))
			mustMkdir(t, dir)
			for _, id := range []string{"env-native", "confirmed-native", "list-native"} {
				putChildFile(t, filepath.Join(dir, id+".jsonl"), `{"type":"assistant","message":{"content":[{"type":"text","text":"[REDACTED]"}]}}`+"\n")
			}
			envFile := filepath.Join(home, "env")
			putChildFile(t, envFile, "CLAUDE_SESSION_ID=confirmed-native\n")
			bin, log := fakeTmux(t, "cat '"+envFile+"'")
			if out, err := exec.Command("sh", "-n", bin).CombinedOutput(); err != nil {
				t.Fatalf("invalid tmux fixture: %v %s", err, out)
			}
			s := testServer(t, config{tmuxBin: bin, homeDir: t.TempDir(), profile: "one"})
			se := sessionInfo{ID: "root", Path: home, ClaudeSessionID: "list-native", TmuxSession: "fixture"}
			if mode == "gone-socket" || mode == "other-socket-live" {
				se.TmuxSocket = "fixture-socket"
			}
			ctx := context.Background()
			if got := s.childNative(ctx, se, childTestNow); got != "confirmed-native" {
				t.Fatal("initial env did not confirm transcript", got)
			}
			want, env := "confirmed-native", "missing-native"
			switch mode {
			case "env", "other-socket-live":
				want, env = "env-native", "env-native"
			case "list", "none":
				if err := os.Remove(filepath.Join(dir, "confirmed-native.jsonl")); err != nil {
					t.Fatal(err)
				}
				want = "list-native"
				if mode == "none" {
					want = ""
					se.ClaudeSessionID = "absent-native"
				}
			case "gone-default", "gone-socket":
				env = "env-native"
				s.tmux.setLive(se.TmuxSocket, map[string]bool{})
			}
			if mode == "other-socket-live" {
				s.tmux.setLive("", map[string]bool{})
			}
			putChildFile(t, envFile, "CLAUDE_SESSION_ID="+env+"\n")
			got := s.childNative(ctx, se, childTestNow.Add(envRecheckEvery))
			if got != want {
				t.Fatalf("resolved %q want %q", got, want)
			}
			calls := s.execTmux.Load()
			wantCalls := int64(2)
			if strings.HasPrefix(mode, "gone-") {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("tmux calls=%d want=%d", calls, wantCalls)
			}
			if se.TmuxSocket != "" {
				argv, err := os.ReadFile(log)
				if err != nil || !strings.Contains(string(argv), "-L fixture-socket show-environment -t fixture CLAUDE_SESSION_ID") {
					t.Fatalf("wrong socket: %s (%v)", argv, err)
				}
			}
			// Within the refresh interval, /clear is deliberately paced.
			s.childNative(ctx, se, childTestNow.Add(envRecheckEvery+time.Second))
			if s.execTmux.Load() != calls {
				t.Fatal("binding refresh ignored cadence")
			}
		})
	}
}

func TestSubagentsResumeTaskNotifications(t *testing.T) {
	for _, tc := range []struct{ name, task, at, outcome, want string }{
		{"succeeded", "child-A", "13", "completed", "succeeded"},
		{"failed", "child-A", "13", "failed", "failed"},
		{"canceled", "child-A", "13", "killed", "canceled"},
		{"foreign-task", "foreign-child", "13", "failed", "finished"},
		{"unverified-launch", "child-A", "13", "failed", "failed"},
		{"foreign-launch", "child-A", "13", "failed", "finished"},
		{"older-terminal", "child-A", "11", "failed", "finished"},
		{"equal-terminal", "child-A", "12", "failed", "finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChildFixture(t)
			child := childFixtureText(t, "child.jsonl") + `{"type":"assistant","isSidechain":true,"agentId":"child-A","sessionId":"parent-session","timestamp":"2026-11-11T09:00:11Z","message":{"content":[{"type":"tool_use","id":"resume-read","name":"Read"}]}}` + "\n"
			putChildFile(t, filepath.Join(f.dir, "agent-child-A.jsonl"), child)
			meta := filepath.Join(f.dir, "agent-child-A.meta.json")
			putChildFile(t, meta, strings.Replace(childFixtureText(t, "meta.json"), "false", "true", 1))
			terminalTime := childTestNow.Add(2 * time.Second)
			if err := os.Chtimes(meta, terminalTime, terminalTime); err != nil {
				t.Fatal(err)
			}
			notification := strings.NewReplacer("child-A", tc.task, "launch-call", "resume-call", "delivery-A", "resume-delivery", "09:00:09", "09:00:"+tc.at, "completed", tc.outcome).Replace(childFixtureText(t, "completion.jsonl"))
			parent := childFixtureText(t, "parent.jsonl")
			if tc.name == "foreign-launch" || tc.name == "unverified-launch" {
				parent += `{"type":"assistant","uuid":"foreign-launch","sessionId":"parent-session","timestamp":"2026-11-11T09:00:11Z","message":{"content":[{"type":"tool_use","id":"resume-call","name":"Agent"}]}}` + "\n"
			}
			if tc.name == "foreign-launch" {
				parent += `{"type":"user","sessionId":"parent-session","timestamp":"2026-11-11T09:00:12Z","message":{"content":[{"type":"tool_result","tool_use_id":"resume-call","content":"[REDACTED]"}]},"toolUseResult":{"agentId":"foreign-child","status":"async_launched","isAsync":true}}` + "\n"
			}
			// Deliberately put the older initial notification last in file order.
			putChildFile(t, f.parent, parent+notification+childFixtureText(t, "completion.jsonl"))
			list := f.read(childTestNow.Add(4 * time.Second))
			if tc.name == "foreign-launch" {
				if len(list.Children) != 2 || list.Children[1].ID != childToken("scope", "parent-session", "foreign-child") {
					t.Fatal("fixture did not verify the foreign invocation")
				}
				list.Children = list.Children[:1]
			}
			c := assertChild(t, list, tc.want, 0)
			if len(c.Invocations) != 1 || c.Invocations[0].Outcome != "" {
				t.Fatal("resume notification invented or completed an invocation")
			}
			if tc.want != "finished" && c.StateSource != "task-notification" {
				t.Fatal("resume outcome missing structural source")
			}
		})
	}
}

func TestSubagentsResumeNotificationKeepsNewestTerminal(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprint(reversed), func(t *testing.T) {
			f := newChildFixture(t)
			first := childFixtureText(t, "completion.jsonl")
			later := strings.NewReplacer("launch-call", "resume-call", "delivery-A", "resume-delivery", "09:00:09", "09:00:13", "completed", "failed").Replace(first)
			if reversed {
				first, later = later, first
			}
			putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+first+later)
			c := assertChild(t, f.read(childTestNow.Add(4*time.Second)), "failed", 0)
			if c.Invocations[0].Outcome != "succeeded" {
				t.Fatal("resume outcome overwrote the initial invocation")
			}
		})
	}
}
