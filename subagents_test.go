package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var childTestNow = time.Date(2026, 11, 11, 9, 0, 10, 0, time.UTC)

type childFixture struct{ root, parent, dir string }

func childFixtureText(t *testing.T, name string) string {
	t.Helper()
	b, e := os.ReadFile("testdata/subagents/claude-2.1/" + name)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func putChildFile(t *testing.T, path, text string) {
	t.Helper()
	if e := os.WriteFile(path, []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Chtimes(path, childTestNow, childTestNow); e != nil {
		t.Fatal(e)
	}
}
func newChildFixture(t *testing.T) childFixture {
	t.Helper()
	root := t.TempDir()
	f := childFixture{root: root, parent: filepath.Join(root, "parent-session.jsonl"), dir: filepath.Join(root, "parent-session", "subagents")}
	if e := os.MkdirAll(f.dir, 0700); e != nil {
		t.Fatal(e)
	}
	putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl"))
	putChildFile(t, filepath.Join(f.dir, "agent-child-A.jsonl"), childFixtureText(t, "child.jsonl"))
	putChildFile(t, filepath.Join(f.dir, "agent-child-A.meta.json"), childFixtureText(t, "meta.json"))
	return f
}
func (f childFixture) read(now time.Time) subagentList {
	return readClaudeChildren(context.Background(), sessionInfo{ID: "deck-root"}, f.parent, "parent-session", "scope", now)
}
func assertChild(t *testing.T, list subagentList, status string, running int) childRun {
	t.Helper()
	if len(list.Children) != 1 || list.Children[0].Status != status || list.Running != running {
		t.Fatalf("children=%+v running=%d complete=%v", list.Children, list.Running, list.Complete)
	}
	return list.Children[0]
}
func TestSubagentsClaudeJoins(t *testing.T) {
	f := newChildFixture(t)
	list := f.read(childTestNow)
	c := assertChild(t, list, "running", 1)
	if !list.Complete || !c.Linked || c.Label != "Explore" || c.LastAction.Name != "Read" || len(c.Invocations) != 1 || c.Invocations[0].ParentBlock != 1 || c.ThreadAvailable {
		t.Fatalf("bad projection: %+v", list)
	}
	b, _ := json.Marshal(list)
	for _, secret := range []string{"REDACTED", "DO-NOT-READ", "prompt", "stdout", "file_path", "parentUuid"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("leaked %s", secret)
		}
	}
	// Idle parent and fleet footer are not inputs to this provider.
	parent := childFixtureText(t, "parent.jsonl") + `{"type":"assistant","message":{"content":"← 99 agents"}}` + "\n"
	putChildFile(t, f.parent, parent)
	assertChild(t, f.read(childTestNow), "running", 1)
}
func TestSubagentsMetadataBeforeResultAndMissingMetadata(t *testing.T) {
	for _, mode := range []string{"before-result", "missing-metadata", "missing-both", "wrong-source", "wrong-call"} {
		t.Run(mode, func(t *testing.T) {
			f := newChildFixture(t)
			parent := childFixtureText(t, "parent.jsonl")
			if mode == "before-result" || mode == "missing-both" {
				lines := strings.Split(parent, "\n")
				parent = strings.Join(append(lines[:1], lines[2:]...), "\n")
			}
			if mode == "missing-metadata" || mode == "missing-both" || mode == "wrong-source" || mode == "wrong-call" {
				if e := os.Remove(filepath.Join(f.dir, "agent-child-A.meta.json")); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "wrong-source" {
				parent = strings.Replace(parent, `"sourceToolAssistantUUID":"parent-message"`, `"sourceToolAssistantUUID":"other-message"`, 1)
			}
			if mode == "wrong-call" {
				parent = strings.Replace(parent, `"tool_use_id":"launch-call"`, `"tool_use_id":"monitor-call"`, 1)
			}
			putChildFile(t, f.parent, parent)
			if mode == "missing-both" || strings.HasPrefix(mode, "wrong") {
				c := assertChild(t, f.read(childTestNow), "unknown", 0)
				if c.Linked {
					t.Fatal("unverified join")
				}
			} else {
				assertChild(t, f.read(childTestNow), "running", 1)
			}
		})
	}
}
func TestSubagentsTerminalAndStale(t *testing.T) {
	for _, mode := range []string{"stale", "disappeared", "finished", "handback", "failed", "canceled", "duplicate-completion", "prose-completion", "continuation"} {
		t.Run(mode, func(t *testing.T) {
			f := newChildFixture(t)
			now := childTestNow
			status := "unknown"
			running := 0
			switch mode {
			case "stale":
				now = now.Add(time.Minute)
			case "disappeared":
				_ = os.Remove(filepath.Join(f.dir, "agent-child-A.jsonl"))
			case "finished":
				putChildFile(t, filepath.Join(f.dir, "agent-child-A.meta.json"), strings.Replace(childFixtureText(t, "meta.json"), "false", "true", 1))
				status = "finished"
			case "handback":
				putChildFile(t, filepath.Join(f.dir, "agent-child-A.jsonl"), childFixtureText(t, "child.jsonl")+childFixtureText(t, "handback.jsonl"))
				status = "finished"
			case "failed", "canceled", "duplicate-completion":
				notification := childFixtureText(t, "completion.jsonl")
				status = "succeeded"
				if mode != "duplicate-completion" {
					status = mode
					notification = strings.ReplaceAll(notification, "completed", map[string]string{"failed": "failed", "canceled": "killed"}[mode])
				}
				putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+notification+notification)
			case "prose-completion":
				putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+`{"type":"user","message":{"content":"<task-notification><task-id>child-A</task-id><status>completed</status></task-notification>"}}`+"\n")
				status = "running"
				running = 1
			case "continuation":
				continuation := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(childFixtureText(t, "parent.jsonl"), "launch-call", "resume-call"), "parent-message", "resume-message"), "09:00:0", "09:00:1")
				putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+childFixtureText(t, "completion.jsonl")+continuation+childFixtureText(t, "completion.jsonl"))
				now = now.Add(10 * time.Second)
				status = "running"
				running = 1
			}
			c := assertChild(t, f.read(now), status, running)
			if mode == "continuation" && len(c.Invocations) != 2 {
				t.Fatalf("resumed child lost edges: %+v", c)
			}
			if mode == "stale" && c.Freshness != "stale" {
				t.Fatal("stale liveness refreshed by read")
			}
		})
	}
}
func TestSubagentsTaskAndNestedExclusion(t *testing.T) {
	for _, mode := range []string{"Task", "Monitor", "Bash", "nested", "wrong-parent", "wrong-agent"} {
		t.Run(mode, func(t *testing.T) {
			f := newChildFixture(t)
			switch mode {
			case "nested":
				putChildFile(t, filepath.Join(f.dir, "agent-child-A.meta.json"), strings.Replace(childFixtureText(t, "meta.json"), `"spawnDepth":1`, `"spawnDepth":2`, 1))
			case "wrong-parent", "wrong-agent":
				child := childFixtureText(t, "child.jsonl")
				if mode == "wrong-parent" {
					child = strings.ReplaceAll(child, "parent-session", "other-parent")
				} else {
					child = strings.ReplaceAll(child, "child-A", "child-B")
				}
				putChildFile(t, filepath.Join(f.dir, "agent-child-A.jsonl"), child)
			default:
				putChildFile(t, f.parent, strings.Replace(childFixtureText(t, "parent.jsonl"), `"name":"Agent"`, `"name":"`+mode+`"`, 1))
			}
			list := f.read(childTestNow)
			if list.Running != 0 {
				t.Fatalf("%s counted: %+v", mode, list)
			}
		})
	}
}
func TestSubagentsLimitsPartialAndIsolation(t *testing.T) {
	t.Run("partial-line", func(t *testing.T) {
		f := newChildFixture(t)
		putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+`{"type":`)
		list := f.read(childTestNow)
		assertChild(t, list, "running", 1)
		if list.Complete {
			t.Fatal("partial line silently ignored")
		}
	})
	t.Run("parent-tail", func(t *testing.T) {
		f := newChildFixture(t)
		putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+strings.Repeat("{\"type\":\"system\"}\n", childParentBytes/10))
		list := f.read(childTestNow)
		if list.Complete || !list.Truncated || list.Running != 0 {
			t.Fatalf("unbounded/uncorrelated %+v", list)
		}
	})
	for _, mode := range []string{"file-symlink", "directory-symlink", "parent-symlink", "meta-symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newChildFixture(t)
			outside := t.TempDir()
			target := filepath.Join(outside, "outside.jsonl")
			putChildFile(t, target, childFixtureText(t, "child.jsonl"))
			path := filepath.Join(f.dir, "agent-child-A.jsonl")
			if mode == "meta-symlink" {
				path = filepath.Join(f.dir, "agent-child-A.meta.json")
				putChildFile(t, target, strings.Replace(childFixtureText(t, "meta.json"), "false", "true", 1))
			}
			if mode == "parent-symlink" {
				path = f.parent
				putChildFile(t, target, childFixtureText(t, "parent.jsonl"))
			}
			if mode == "directory-symlink" {
				path = f.dir
				target = outside
				if e := os.RemoveAll(path); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
			}
			if e := os.Symlink(target, path); e != nil {
				t.Fatal(e)
			}
			list := f.read(childTestNow)
			if mode == "meta-symlink" {
				assertChild(t, list, "running", 1)
			} else if list.Running != 0 {
				t.Fatal("symlink read escaped")
			}
		})
	}
	t.Run("bounded-discovery", func(t *testing.T) {
		f := newChildFixture(t)
		for i := 0; i < childMaxRuns+5; i++ {
			id := fmt.Sprintf("child-%03d", i)
			putChildFile(t, filepath.Join(f.dir, "agent-"+id+".jsonl"), strings.ReplaceAll(childFixtureText(t, "child.jsonl"), "child-A", id))
		}
		list := f.read(childTestNow)
		if len(list.Children) > childMaxRuns || list.Complete || !list.Truncated {
			t.Fatalf("unbounded discovery: %d complete=%v", len(list.Children), list.Complete)
		}
	})
}
func TestSubagentsPagingEpochAndCache(t *testing.T) {
	f := newChildFixture(t)
	var cache subagentCache
	se := sessionInfo{ID: "root"}
	read := func(profile string, now time.Time) subagentList {
		return cache.read(context.Background(), profile, se, f.parent, "parent-session", now)
	}
	a := read("one", childTestNow)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b := read("one", childTestNow); b.Revision != a.Revision {
				t.Error("phones didn't share snapshot")
			}
		}()
	}
	wg.Wait()
	if b := read("two", childTestNow); b.Segment == a.Segment || b.Children[0].ID == a.Children[0].ID {
		t.Fatal("cross-profile identity")
	}
	a.Children = append(a.Children, a.Children[0])
	a.Children[1].ID = "different"
	first, code := childPage(a, "", "", 1)
	if code != 200 || len(first.Children) != 1 || first.NextCursor == "" {
		t.Fatal("no bounded page")
	}
	second, code := childPage(a, a.Segment, first.NextCursor, 1)
	if code != 200 || len(second.Children) != 1 || second.NextCursor != "" {
		t.Fatal("bad next page")
	}
	for _, args := range [][2]string{{"other", first.NextCursor}, {a.Segment, "invalid"}} {
		reset, code := childPage(a, args[0], args[1], 1)
		if code != 409 || !reset.Reset {
			t.Fatal("missing reset")
		}
	}
	putChildFile(t, f.parent, strings.Replace(childFixtureText(t, "parent.jsonl"), "parent-message", "source-rewrite", -1))
	if err := os.Chtimes(f.parent, childTestNow.Add(childCacheTTL), childTestNow.Add(childCacheTTL)); err != nil {
		t.Fatal(err)
	}
	b := read("one", childTestNow.Add(childCacheTTL))
	if a.Segment == b.Segment {
		t.Fatal("rewrite retained segment")
	}
	var restarted subagentCache
	if c := restarted.read(context.Background(), "one", se, f.parent, "parent-session", childTestNow); c.Segment == b.Segment {
		t.Fatal("restart retained epoch")
	}
}
func TestSubagentsEndpointScopesAndCapabilities(t *testing.T) {
	s := testServer(t, config{token: "fixture-token", homeDir: t.TempDir(), profile: "one"})
	s.lists.m["one"] = listCacheEntry{at: time.Now(), sessions: []sessionInfo{{ID: "root", Tool: "opencode"}}}
	s.lists.m["two"] = listCacheEntry{at: time.Now(), sessions: []sessionInfo{}}
	for _, tc := range []struct {
		url  string
		code int
	}{{"?id=root", 200}, {"?id=root&profile=two", 404}, {"?id=other", 404}, {"?id=root&child=../../secret", 400}, {"?id=root&host=other", 400}, {"?id=root&segment=other", 409}, {"", 400}} {
		w := httptest.NewRecorder()
		s.handleSubagents(w, httptest.NewRequest("GET", "/api/rc/subagents"+tc.url, nil))
		if w.Code != tc.code {
			t.Fatalf("%s => %d: %s", tc.url, w.Code, w.Body)
		}
	}
	cap := childCapabilities(sessionInfo{Tool: "opencode"})
	if cap.Summary || cap.Thread || cap.Live || cap.Provider != "opencode-2" {
		t.Fatalf("2.x evidence gate violated: %+v", cap)
	}
	cap = childCapabilities(sessionInfo{Tool: "claude"})
	if !cap.Summary || cap.Thread || cap.Live {
		t.Fatal("S1 capability incorrect")
	}
	cap = childCapabilities(sessionInfo{Tool: "claude", SSHHost: "remote"})
	if cap.Summary {
		t.Fatal("remote claims local reads")
	}
	w := httptest.NewRecorder()
	s.auth(http.HandlerFunc(s.handleSubagents)).ServeHTTP(w, httptest.NewRequest("GET", "/api/rc/subagents?id=root", nil))
	if w.Code != 401 {
		t.Fatalf("unauthenticated %d", w.Code)
	}
}

func TestSubagentsFreshnessCacheAndSourceChanges(t *testing.T) {
	f := newChildFixture(t)
	var cache subagentCache
	se := sessionInfo{ID: "root"}
	read := func(now time.Time) subagentList {
		return cache.read(context.Background(), "one", se, f.parent, "parent-session", now)
	}
	first := read(childTestNow)
	assertChild(t, first, "running", 1)
	// A stat-only cache refresh cannot extend liveness indefinitely.
	stale := read(childTestNow.Add(time.Minute))
	assertChild(t, stale, "unknown", 0)
	if first.Segment != stale.Segment || first.Children[0].EvidenceAt != stale.Children[0].EvidenceAt {
		t.Fatal("unchanged source renewed evidence")
	}
	// Removal is detected on the next discovery, including when the old result remains.
	if e := os.Remove(filepath.Join(f.dir, "agent-child-A.jsonl")); e != nil {
		t.Fatal(e)
	}
	gone := read(childTestNow.Add(2 * time.Minute))
	assertChild(t, gone, "unknown", 0)
	if gone.Children[0].Availability != "unavailable" {
		t.Fatal("deleted child still readable")
	}
}
func TestSubagentsMetadataOutcomeAndContinuation(t *testing.T) {
	f := newChildFixture(t)
	putChildFile(t, filepath.Join(f.dir, "agent-child-A.meta.json"), strings.Replace(childFixtureText(t, "meta.json"), "false", "true", 1))
	// Metadata is written after notification delivery; its mtime must not erase
	// the more specific outcome for the same invocation.
	putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+childFixtureText(t, "completion.jsonl"))
	assertChild(t, f.read(childTestNow), "succeeded", 0)
	continuation := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(childFixtureText(t, "parent.jsonl"), "launch-call", "resume-call"), "parent-message", "resume-message"), "09:00:0", "09:00:1")
	putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+childFixtureText(t, "completion.jsonl")+continuation)
	assertChild(t, f.read(childTestNow.Add(10*time.Second)), "running", 1)
}
func TestSubagentsCurrentRootBindingAndClear(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	project := filepath.Join(home, "project")
	dir := filepath.Join(home, "projects", convertToClaudeDirName(project))
	mustMkdir(t, dir)
	for _, id := range []string{"native-one", "native-two"} {
		putChildFile(t, filepath.Join(dir, id+".jsonl"), "{\"type\":\"system\"}\n")
	}
	s := testServer(t, config{token: "fixture", homeDir: t.TempDir(), profile: "one"})
	se := sessionInfo{ID: "same-id", Path: project, ClaudeSessionID: "native-one"}
	if got := s.childNative(withProfile(context.Background(), "one"), se, childTestNow); got != "native-one" {
		t.Fatal(got)
	}
	se.ClaudeSessionID = "native-two"
	if got := s.childNative(withProfile(context.Background(), "two"), se, childTestNow); got != "native-two" {
		t.Fatal("cross-profile binding", got)
	}
	if got := s.childNative(withProfile(context.Background(), "one"), se, childTestNow); got != "native-one" {
		t.Fatal("confirmed id lost precedence over list id", got)
	}
	if err := os.Remove(filepath.Join(dir, "native-one.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := s.childNative(withProfile(context.Background(), "one"), se, childTestNow); got != "native-two" {
		t.Fatal("missing confirmed transcript prevented list fallback", got)
	}
}
func TestSubagentsClaudeEndpointAndActivity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	f := newChildFixture(t)
	project := "/synthetic-project"
	dir := filepath.Join(home, "projects", convertToClaudeDirName(project))
	if e := os.MkdirAll(filepath.Dir(dir), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(f.root, dir); e != nil {
		t.Fatal(e)
	}
	s := testServer(t, config{token: "fixture", homeDir: t.TempDir(), profile: "one"})
	se := sessionInfo{ID: "root", Tool: "claude", ClaudeSessionID: "parent-session", Path: project}
	s.lists.m["one"] = listCacheEntry{at: time.Now(), sessions: []sessionInfo{se}}
	w := httptest.NewRecorder()
	s.handleSubagents(w, httptest.NewRequest("GET", "/api/rc/subagents?id=root", nil))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var list subagentList
	if e := json.Unmarshal(w.Body.Bytes(), &list); e != nil {
		t.Fatal(e)
	}
	assertChild(t, list, "unknown", 0) // deliberately historical timestamps
	w = httptest.NewRecorder()
	s.handleActivity(w, httptest.NewRequest("GET", "/api/rc/activity?id=root", nil))
	var activity activityWithAttention
	if e := json.Unmarshal(w.Body.Bytes(), &activity); e != nil {
		t.Fatal(e)
	}
	if activity.Working || activity.Subagents == nil || activity.Subagents.Unknown != 1 {
		t.Fatalf("idle parent lost child state: %+v", activity)
	}
}
func TestSubagentsOpaquePagesBoundBytesAndScope(t *testing.T) {
	list := subagentList{Segment: "segment", Children: []childRun{}, subagentSummary: subagentSummary{Revision: "revision"}}
	for i := 0; i < 100; i++ {
		c := childRun{ID: fmt.Sprint(i), Invocations: []childInvocation{}}
		for n := 0; n < 32; n++ {
			c.Invocations = append(c.Invocations, childInvocation{ID: strings.Repeat("i", 32), ParentRowID: strings.Repeat("r", 128), ParentToolCallID: strings.Repeat("c", 128)})
		}
		list.Children = append(list.Children, c)
	}
	page, code := childPage(list, "", "", 10000)
	if code != 200 || len(page.Children) >= 50 || page.NextCursor == "" {
		t.Fatal("byte limit not enforced")
	}
	b, _ := json.Marshal(page)
	if len(b) > childPageBytes+2048 {
		t.Fatalf("oversized page %d", len(b))
	}
	other := list
	other.Segment = "other-root"
	if _, code := childPage(other, "", page.NextCursor, 50); code != 409 {
		t.Fatal("cross-root cursor accepted")
	}
}
func TestSubagentsUnsupportedResultBodyDoesNotBreakProjection(t *testing.T) {
	f := newChildFixture(t)
	putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+`{"type":"user","toolUseResult":"[REDACTED]","message":{"content":"[REDACTED]"}}`+"\n")
	list := f.read(childTestNow)
	if !list.Complete {
		t.Fatal("ordinary tool result made source incomplete")
	}
	assertChild(t, list, "running", 1)
}

func TestSubagentsIncompleteIdentityIsNotVerifiedZero(t *testing.T) {
	f := newChildFixture(t)
	putChildFile(t, f.parent, "{\"type\":\"system\"}\n")
	putChildFile(t, filepath.Join(f.dir, "agent-child-A.jsonl"), `{"type":"user","isSidechain":true,"agentId":"child-A","sessionId":"parent-session","message":{"content":"`+strings.Repeat("x", childTailBytes)+`"}}`+"\n")
	list := f.read(childTestNow)
	if list.Complete || list.Running != 0 {
		t.Fatal("unreadable identity advertised verified zero")
	}
}
func TestSubagentsCancelledReadIsNotCached(t *testing.T) {
	f := newChildFixture(t)
	var cache subagentCache
	se := sessionInfo{ID: "root"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := cache.read(ctx, "one", se, f.parent, "parent-session", childTestNow); got.Availability != "unavailable" {
		t.Fatalf("cancelled read reported %s", got.Availability)
	}
	// Unchanged files after the TTL must not resurrect the cancelled result.
	got := cache.read(context.Background(), "one", se, f.parent, "parent-session", childTestNow.Add(childCacheTTL+time.Second))
	if got.Availability != "available" || len(got.Children) != 1 {
		t.Fatalf("cancelled read was cached: %+v", got)
	}
}

func TestSubagentsBoundedWindowsKeepFreshCounts(t *testing.T) {
	for _, source := range []string{"child", "parent"} {
		t.Run(source, func(t *testing.T) {
			f := newChildFixture(t)
			if source == "child" {
				child := childFixtureText(t, "child.jsonl")
				putChildFile(t, filepath.Join(f.dir, "agent-child-A.jsonl"), child+strings.Repeat("{\"type\":\"system\"}\n", childTailBytes/10)+child)
			} else {
				putChildFile(t, f.parent, strings.Repeat("{\"type\":\"system\"}\n", childParentBytes/10)+childFixtureText(t, "parent.jsonl"))
			}
			list := f.read(childTestNow)
			c := assertChild(t, list, "running", 1)
			if list.Availability != "available" || !list.FreshUntil.After(childTestNow) || c.Freshness != "fresh" {
				t.Fatalf("bounded window lost fresh evidence: %+v", list)
			}
			if source == "child" && (!list.Complete || list.Truncated) {
				t.Fatal("normal child tail reduced discovery coverage")
			}
			if source == "parent" && (list.Complete || !list.Truncated) {
				t.Fatal("parent window omitted partial coverage hint")
			}
		})
	}
}

func TestSubagentsChildActivityAfterTerminalReopensWithoutLaunch(t *testing.T) {
	for _, outcome := range []string{"completed", "failed", "killed", "handback", "metadata"} {
		t.Run(outcome, func(t *testing.T) {
			f := newChildFixture(t)
			child := childFixtureText(t, "child.jsonl")
			notification := strings.ReplaceAll(childFixtureText(t, "completion.jsonl"), "completed", outcome)
			switch outcome {
			case "handback":
				child += childFixtureText(t, "handback.jsonl")
			case "metadata":
				putChildFile(t, filepath.Join(f.dir, "agent-child-A.meta.json"), strings.Replace(childFixtureText(t, "meta.json"), "false", "true", 1))
			default:
				putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+notification)
			}
			// A later assistant tool use resumes the same sidechain, with no new
			// parent Agent launch/result. Everything is synthetic structural data.
			resumed := `{"type":"assistant","isSidechain":true,"agentId":"child-A","sessionId":"parent-session","timestamp":"2026-11-11T09:00:11Z","message":{"content":[{"type":"tool_use","id":"resume-read","name":"Read"}]}}` + "\n"
			putChildFile(t, filepath.Join(f.dir, "agent-child-A.jsonl"), child+resumed)
			c := assertChild(t, f.read(childTestNow.Add(2*time.Second)), "running", 1)
			if c.StateSource != "child-activity" || len(c.Invocations) != 1 {
				t.Fatalf("resume lost activity or invented invocation: %+v", c)
			}
			assertChild(t, f.read(childTestNow.Add(time.Minute)), "unknown", 0)
			// A subsequent real completion still closes the resumed child, even
			// when an older delivery appears later in the parent file.
			later := strings.ReplaceAll(strings.ReplaceAll(childFixtureText(t, "completion.jsonl"), "09:00:09", "09:00:13"), "delivery-A", "delivery-B")
			putChildFile(t, f.parent, childFixtureText(t, "parent.jsonl")+later+notification)
			assertChild(t, f.read(childTestNow.Add(4*time.Second)), "succeeded", 0)
		})
	}
}

func TestSubagentsUnlinkedFinishedMetadataIsNotUnknown(t *testing.T) {
	f := newChildFixture(t)
	putChildFile(t, f.parent, "{\"type\":\"system\"}\n")
	list := f.read(childTestNow)
	assertChild(t, list, "unknown", 0)
	if list.Unknown != 1 {
		t.Fatal("unlinked nonterminal child must remain unknown")
	}
	putChildFile(t, filepath.Join(f.dir, "agent-child-A.meta.json"), strings.Replace(childFixtureText(t, "meta.json"), "false", "true", 1))
	list = f.read(childTestNow)
	c := assertChild(t, list, "finished", 0)
	if c.Linked || c.StateSource != "metadata-finished" || list.Unknown != 0 {
		t.Fatalf("unlinked terminal metadata lost: %+v", list)
	}
	if aged := ageChildList(list, childTestNow.Add(time.Minute)); aged.Unknown != 0 || aged.Children[0].Status != "finished" {
		t.Fatal("terminal metadata became unknown when aged")
	}
}

func TestSubagentsSessionSummaryRequiresCapabilityAndTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	f := newChildFixture(t)
	project := "/synthetic-project"
	dir := filepath.Join(home, "projects", convertToClaudeDirName(project))
	mustMkdir(t, filepath.Dir(dir))
	if err := os.Rename(f.root, dir); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, config{bin: "/usr/bin/false", homeDir: t.TempDir(), profile: "one"})
	sessions := []sessionInfo{
		{ID: "local", Tool: "claude", Path: project, ClaudeSessionID: "parent-session"},
		{ID: "remote", Tool: "claude", Path: project, ClaudeSessionID: "parent-session", SSHHost: "fixture-remote"},
		{ID: "missing", Tool: "claude", Path: project, ClaudeSessionID: "absent-session"},
		{ID: "unbound", Tool: "claude", Path: project},
	}
	s.lists.m["one"] = listCacheEntry{at: time.Now(), sessions: sessions}
	for _, se := range sessions {
		s.acts.update(se.ID, activityInfo{})
		s.replies.entry(se.ID).cliAt = time.Now() // fixture-only: no CLI or tmux access
	}
	body, _, err := s.buildSessions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Sessions []sessionInfo }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != len(sessions) {
		t.Fatalf("missing sessions: %s", body)
	}
	for _, se := range got.Sessions {
		want := se.ID == "local"
		if (se.Subagents != nil) != want {
			t.Errorf("%s session summary present=%v want=%v", se.ID, se.Subagents != nil, want)
		}
		w := httptest.NewRecorder()
		s.handleActivity(w, httptest.NewRequest("GET", "/api/rc/activity?id="+se.ID, nil))
		var a activityWithAttention
		if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		if (a.Subagents != nil) != want {
			t.Errorf("%s activity summary present=%v want=%v", se.ID, a.Subagents != nil, want)
		}
	}
}
