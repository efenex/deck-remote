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
