package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "pi", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParsePiTranscript(t *testing.T) {
	snap, tail, err := parsePiTranscript(strings.NewReader(readFixture(t, "session.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if tail.Working {
		t.Fatal("a transcript ending in an aborted turn is not working")
	}
	if snap.State != "interrupted" {
		t.Fatalf("state = %q, want interrupted", snap.State)
	}
	var users, replies []string
	for _, m := range snap.Messages {
		switch m.Role {
		case "user":
			users = append(users, m.Content)
		case "reply":
			replies = append(replies, m.Content)
		}
		if m.Seq == 0 || m.TurnID == "" {
			t.Fatalf("message without seq/turn: %+v", m)
		}
	}
	if len(users) != 5 || users[3] != "After that, say FOLLOWUP" {
		t.Fatalf("users = %q", users)
	}
	if len(replies) != 4 || replies[3] != "FOLLOWUP" || snap.LastReply != "FOLLOWUP" {
		t.Fatalf("replies = %q, last = %q", replies, snap.LastReply)
	}
	if snap.Sequence == 0 || snap.LastActivity == 0 {
		t.Fatalf("sequence/activity unset: %d %d", snap.Sequence, snap.LastActivity)
	}
	// The reply to the follow-up belongs to the follow-up's own turn, which is
	// what request correlation (codexRequestTracker.reconcile) relies on.
	var fuTurn string
	for _, m := range snap.Messages {
		if m.Role == "user" && m.Content == "After that, say FOLLOWUP" {
			fuTurn = m.TurnID
		}
		if m.Role == "reply" && m.Content == "FOLLOWUP" && m.TurnID != fuTurn {
			t.Fatalf("follow-up reply turn %q, want %q", m.TurnID, fuTurn)
		}
	}
}

func TestParsePiTranscriptMidTurnAndBranch(t *testing.T) {
	const doc = `{"type":"session","version":3,"id":"s","timestamp":"2026-10-16T00:00:00Z","cwd":"/x"}
{"type":"message","id":"a1","parentId":null,"timestamp":"2026-10-16T00:00:01Z","message":{"role":"user","content":"first","timestamp":1}}
{"type":"message","id":"a2","parentId":"a1","timestamp":"2026-10-16T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"abandoned"}],"stopReason":"stop"}}
{"type":"message","id":"b1","parentId":null,"timestamp":"2026-10-16T00:00:03Z","message":{"role":"user","content":[{"type":"text","text":"second"}]}}
{"type":"message","id":"b2","parentId":"b1","timestamp":"2026-10-16T00:00:04Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"looking"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls -la"}}],"stopReason":"toolUse"}}
{"type":"message","id":"b3","parentId":"b2","timestamp":"2026-10-1`
	snap, tail, err := parsePiTranscript(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !tail.Working || tail.CurrentTool != "bash(ls -la)" {
		t.Fatalf("tail = %+v", tail)
	}
	if len(snap.Messages) != 2 || snap.Messages[0].Content != "second" || snap.Messages[1].Role != "commentary" {
		t.Fatalf("active branch messages = %+v", snap.Messages)
	}
	if snap.LastReply != "" {
		t.Fatalf("commentary must not become the last reply: %q", snap.LastReply)
	}
}

func TestParsePiTranscriptRejectsForeignFile(t *testing.T) {
	if _, _, err := parsePiTranscript(strings.NewReader(`{"type":"message","id":"x"}`)); err == nil {
		t.Fatal("expected an error without a session header")
	}
}

func TestPiPaneParsing(t *testing.T) {
	idle, draft, working := readFixture(t, "pane-idle.txt"), readFixture(t, "pane-draft.txt"), readFixture(t, "pane-working.txt")
	if d, ok := piEditorDraft(idle); !ok || d != "" {
		t.Fatalf("idle draft = %q, %v", d, ok)
	}
	if d, ok := piEditorDraft(draft); !ok || d != "a draft" {
		t.Fatalf("draft = %q, %v", d, ok)
	}
	if piPaneWorking(idle) || piPaneWorking(draft) || !piPaneWorking(working) {
		t.Fatal("working detection wrong")
	}
	if d, ok := piEditorDraft(working); !ok || d != "" {
		t.Fatalf("working draft = %q, %v", d, ok)
	}
	if piPaneProgressKey(working) == "" {
		t.Fatal("no progress key while working")
	}
	if _, ok := piEditorDraft("some other screen\nwith no editor"); ok {
		t.Fatal("editor found on a screen without one")
	}
}

func TestPiAdapterLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := &server{pi: newPiTranscriptStore(), acts: newActivityCache()}
	a := piAdapter{s: s, root: ".pi"}
	se := sessionInfo{ID: "inst-1", Tool: "pi"}

	snap, _ := a.Snapshot(nil, se, "", false)
	if snap.DegradedReason == "" {
		t.Fatal("missing session dir must degrade to terminal-only")
	}

	dir := filepath.Join(home, ".pi", "agent-deck", "inst-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	snap, _ = a.Snapshot(nil, se, "", false)
	if snap.DegradedReason != "" || snap.State != "idle" {
		t.Fatalf("fresh session: %+v", snap)
	}

	old := filepath.Join(dir, "2026-10-01_old.jsonl")
	if err := os.WriteFile(old, []byte(`{"type":"session","version":3,"id":"o"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-10-16_new.jsonl"), []byte(readFixture(t, "session.jsonl")), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, _ = a.Snapshot(nil, se, readFixture(t, "pane-working.txt"), true)
	if snap.LastReply != "FOLLOWUP" || !snap.Activity.Working || snap.State != "working" {
		t.Fatalf("newest transcript + live pane: reply=%q working=%v state=%q", snap.LastReply, snap.Activity.Working, snap.State)
	}
}

// TestParsePiUserImages: a pi user message with image blocks keeps its text and
// an image count; an image-only message is still a user row.
func TestParsePiUserImages(t *testing.T) {
	const tr = `{"type":"session","version":3,"id":"s1","timestamp":"2026-10-21T05:00:00.000Z","cwd":"/tmp"}
{"type":"message","id":"m1","parentId":null,"timestamp":"2026-10-21T05:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image","data":"AAAA","mimeType":"image/png"}],"timestamp":1792558801000}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-10-21T05:00:02.000Z","message":{"role":"user","content":[{"type":"image","data":"AAAA","mimeType":"image/png"}],"timestamp":1792558802000}}
`
	snap, _, err := parsePiTranscript(strings.NewReader(tr))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 2 || snap.Messages[0].Content != "what is this" || snap.Messages[0].Images != 1 ||
		snap.Messages[1].Content != "" || snap.Messages[1].Images != 1 {
		t.Fatalf("rows = %+v", snap.Messages)
	}
}

// TestPiActivityCacheExpiresWithoutPane: with no pane, the cache is trusted only
// for 3 intervals after the last real pane observation, even though every
// watcher sweep replays the cached value through stepStall (which refreshes
// updatedAt). A dead pi must not read as working forever.
func TestPiActivityCacheExpiresWithoutPane(t *testing.T) {
	const id = "inst-expire"
	for _, tc := range []struct {
		name    string
		sweeps  int // watchInterval-spaced sweeps after the last pane read
		working bool
	}{
		{"first sweep after pane loss", 1, true},
		{"just inside 3 intervals", 2, true},
		{"at 3 intervals", 3, false},
		{"long dead", 40, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{pi: newPiTranscriptStore(), acts: newActivityCache()}
			a := piAdapter{s: s, root: ".pi"}
			se := sessionInfo{ID: id, Tool: "pi"}
			t0 := time.Unix(1_800_000_000, 0)
			// The last real observation: a working pane.
			s.acts.m[id] = stepStall(activityState{}, activityInfo{Working: true, Activity: "Working…", stallKeySet: true, stallKey: "k"}, t0)

			// Transcript tail says idle (nothing to fall back on).
			var got activityInfo
			now := t0
			for i := 0; i < tc.sweeps; i++ {
				now = now.Add(watchInterval)
				got = a.activityAt(se, harnessSnapshot{LastActivity: t0.Unix()}, piTail{}, "", false, now)
				s.acts.m[id] = stepStall(s.acts.m[id], got, now) // the watcher's write-back
			}
			if got.Working != tc.working {
				t.Fatalf("after %d sweeps Working = %v, want %v", tc.sweeps, got.Working, tc.working)
			}
		})
	}
}

// TestPiReplyCorrelatesAfterTreeSwitch: after /tree moves the leaf to a shorter
// branch, a new request's user and reply messages sit at a smaller position on
// the active branch than the baseline taken from the longer one. Correlation
// must follow file order, not branch position.
func TestPiReplyCorrelatesAfterTreeSwitch(t *testing.T) {
	const head = `{"type":"session","version":3,"id":"s","timestamp":"2026-10-16T00:00:00Z","cwd":"/x"}
{"type":"message","id":"r","parentId":null,"timestamp":"2026-10-16T00:00:01Z","message":{"role":"user","content":"root"}}
{"type":"message","id":"a1","parentId":"r","timestamp":"2026-10-16T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"a1"}],"stopReason":"stop"}}
{"type":"message","id":"a2","parentId":"a1","timestamp":"2026-10-16T00:00:03Z","message":{"role":"user","content":"deep"}}
{"type":"message","id":"a3","parentId":"a2","timestamp":"2026-10-16T00:00:04Z","message":{"role":"assistant","content":[{"type":"text","text":"a3"}],"stopReason":"stop"}}
`
	// After /tree back to "r": the new turn forks directly off the root.
	const branch = `{"type":"message","id":"b1","parentId":"r","timestamp":"2026-10-16T00:00:10Z","message":{"role":"user","content":"ask after tree"}}
{"type":"message","id":"b2","parentId":"b1","timestamp":"2026-10-16T00:00:11Z","message":{"role":"assistant","content":[{"type":"text","text":"answer after tree"}],"stopReason":"stop"}}
`
	before, _, err := parsePiTranscript(strings.NewReader(head))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		secondFile string // a /new session instead of a branch in the same file
	}{{"same file", ""}, {"after /new", "new"}} {
		t.Run(tc.name, func(t *testing.T) {
			doc := head + branch
			if tc.secondFile != "" {
				doc = `{"type":"session","version":3,"id":"s2","timestamp":"2026-10-16T00:00:09Z","cwd":"/x"}` + "\n" +
					strings.ReplaceAll(branch, `"parentId":"r"`, `"parentId":null`)
			}
			after, _, err := parsePiTranscript(strings.NewReader(doc))
			if err != nil {
				t.Fatal(err)
			}
			if after.Sequence <= before.Sequence {
				t.Fatalf("sequence %d did not advance past baseline %d", after.Sequence, before.Sequence)
			}
			tr := newCodexRequestTracker()
			tr.add("i", codexPendingRequest{RequestID: "req", Text: "ask after tree", Baseline: before.Sequence})
			got := tr.reconcile("i", after)
			if len(got) != 1 || got[0].Message.Content != "answer after tree" {
				t.Fatalf("reply not correlated: %+v", got)
			}
		})
	}
}

func piLine(id, parent, ts, msg string) string {
	pj := "null"
	if parent != "" {
		pj = `"` + parent + `"`
	}
	return `{"type":"message","id":"` + id + `","parentId":` + pj + `,"timestamp":"` + ts + `","message":` + msg + "}\n"
}

// TestPiLargeTranscriptStillYieldsActivityAndAsk: a transcript past
// maxPiTranscriptBytes (generated here, not committed) is read from its header
// and tail; history is flagged truncated, but the snapshot is not degraded, so
// ask stays allowed, and the mid-turn tail still reads as working.
func TestPiLargeTranscriptStillYieldsActivityAndAsk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".pi", "agent-deck", "inst-big")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-10-26_big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"session","version":3,"id":"big","timestamp":"2026-10-26T00:00:00Z","cwd":"/x"}` + "\n")
	// Old history: a user turn followed by ~70 MiB of tool output.
	f.WriteString(piLine("u0", "", "2026-10-26T00:00:01Z", `{"role":"user","content":"ancient prompt"}`))
	base := time.Now().Add(-time.Minute)
	now := func(s int) string { return base.Add(time.Duration(s) * time.Second).UTC().Format(time.RFC3339Nano) }
	pad := strings.Repeat("x", 1<<20)
	parent := "u0"
	for i := 0; i < 70; i++ {
		id := "t" + strings.Repeat("a", i%5) + string(rune('A'+i%26)) + string(rune('a'+i/26))
		f.WriteString(piLine(id, parent, "2026-10-26T00:00:02Z", `{"role":"toolResult","toolCallId":"c","content":[{"type":"text","text":"`+pad+`"}]}`))
		parent = id
	}
	f.WriteString(piLine("u1", parent, now(0), `{"role":"user","content":"recent prompt"}`))
	f.WriteString(piLine("a1", "u1", now(1), `{"role":"assistant","content":[{"type":"toolCall","id":"c9","name":"bash","arguments":{"command":"make test"}}],"stopReason":"toolUse"}`))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() <= maxPiTranscriptBytes {
		t.Fatalf("fixture only %d bytes, want > %d", st.Size(), maxPiTranscriptBytes)
	}

	s := &server{pi: newPiTranscriptStore(), acts: newActivityCache()}
	a := piAdapter{s: s, root: ".pi"}
	se := sessionInfo{ID: "inst-big", Tool: "pi"}
	snap, err := a.Snapshot(nil, se, "", false)
	if err != nil || snap.DegradedReason != "" {
		t.Fatalf("a large transcript must not block ask: err=%v degraded=%q", err, snap.DegradedReason)
	}
	if !snap.HistoryTruncated {
		t.Fatal("expected HistoryTruncated")
	}
	if snap.Sequence == 0 || len(snap.Messages) == 0 || snap.Messages[len(snap.Messages)-1].Content != "recent prompt" {
		t.Fatalf("tail not read: seq=%d msgs=%+v", snap.Sequence, snap.Messages)
	}
	if !snap.Activity.Working || snap.Activity.CurrentTool != "bash(make test)" {
		t.Fatalf("activity from the tail: %+v", snap.Activity)
	}
	hist, err := a.History(nil, se)
	if err != nil || len(hist) == 0 || !strings.Contains(hist[0].Content, "truncated") {
		t.Fatalf("history must say it is truncated: %v %+v", err, hist)
	}

	// Growth is parsed from the last offset: the reply lands, working clears.
	app, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	app.WriteString(piLine("a2", "a1", now(5), `{"role":"toolResult","toolCallId":"c9","content":"ok"}`))
	app.WriteString(piLine("a3", "a2", now(6), `{"role":"assistant","content":[{"type":"text","text":"all green"}],"stopReason":"stop"}`))
	// A torn final line is left for the next read.
	app.WriteString(`{"type":"message","id":"a4","parentId":"a3","timest`)
	app.Close()
	before := snap.Sequence
	snap2, _ := a.Snapshot(nil, se, "", false)
	if snap2.LastReply != "all green" || snap2.Activity.Working || snap2.Sequence <= before {
		t.Fatalf("incremental read: %+v", snap2)
	}
	off := s.pi.m[path].parser.offset
	app, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	app.WriteString(`amp":"` + now(60) + `","message":{"role":"user","content":"next"}}` + "\n")
	app.Close()
	snap3, _ := a.Snapshot(nil, se, "", false)
	if snap3.Messages[len(snap3.Messages)-1].Content != "next" || s.pi.m[path].parser.offset <= off {
		t.Fatalf("torn line not completed: %+v", snap3.Messages)
	}
}

// TestPiPaneProgressKeyPinsCurrentHeuristic pins what counts as visible
// progress today (finding #5, unverified against a real pi pane): only the
// output above the editor's top border is hashed, so a silent tool call that
// redraws nothing there reads as a frozen turn, while the spinner in the border
// and the footer are ignored. If pi is found to redraw elsewhere during silent
// tool calls, change this test together with the heuristic.
func TestPiPaneProgressKeyPinsCurrentHeuristic(t *testing.T) {
	pane := readFixture(t, "pane-working.txt")
	lines := piPaneLines(pane)
	top, bottom, ok := piEditorBorders(lines)
	if !ok || !piPaneWorking(pane) {
		t.Fatal("fixture should be a working pane")
	}
	key := piPaneProgressKey(pane)
	if key == "" {
		t.Fatal("empty key for a pane with an editor")
	}
	join := func(l []string) string { return strings.Join(l, "\n") }

	spin := append([]string(nil), lines...)
	spin[top] = strings.Replace(spin[top], "Working", "Working.", 1)
	if piPaneProgressKey(join(spin)) != key {
		t.Fatal("the border spinner must not count as progress")
	}
	if bottom+1 < len(lines) {
		foot := append([]string(nil), lines...)
		foot[len(foot)-1] += " 99%"
		if piPaneProgressKey(join(foot)) != key {
			t.Fatal("the footer must not count as progress")
		}
	}
	if top > 0 {
		out := append([]string(nil), lines...)
		out[top-1] += " more output"
		if piPaneProgressKey(join(out)) == key {
			t.Fatal("new output above the editor must count as progress")
		}
	}

	// Unchanged key while working: Stalled at stallThreshold polls, not before.
	defer func(old int) { stallThreshold = old }(stallThreshold)
	for _, thr := range []int{2, 4} {
		stallThreshold = thr
		var st activityState
		now := time.Unix(1_800_000_000, 0)
		for poll := 1; poll <= thr; poll++ {
			now = now.Add(watchInterval)
			st = stepStall(st, activityInfo{Working: true, Activity: "Working…", stallKeySet: true, stallKey: key}, now)
			if want := poll >= thr; st.Stalled != want {
				t.Fatalf("threshold %d poll %d: Stalled = %v, want %v", thr, poll, st.Stalled, want)
			}
		}
	}
}
