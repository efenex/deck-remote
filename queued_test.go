package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParseTranscriptQueuedCommand: prompts the operator typed while Claude was
// working exist in the transcript only as attachment.queued_command lines. The
// human ones become queued user rows and peer messages collapsed peer rows;
// task-notifications and unrelated attachments produce nothing.
func TestParseTranscriptQueuedCommand(t *testing.T) {
	msgs, err := parseTranscriptFile("testdata/claude/queued-command.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range msgs {
		got = append(got, m.Role+":"+m.Content)
	}
	want := []string{
		"user:start the kong upgrade",
		"reply:Working on it.",
		"user:ok so the upgrade is done, now back to the vpn issue",
		"peer:[ad-hang → tower] Handoff (reply only).",
		"user:what is needed for the ga-griffin-jobs ?",
		"reply:Both noted.",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q\nwant   %q", got, want)
	}
	if m := msgs[0]; m.ID != "u-0001" || m.Queued {
		t.Errorf("typed row id/queued = %q/%v", m.ID, m.Queued)
	}
	if m := msgs[1]; m.ID != "as-0001" {
		t.Errorf("reply id = %q", m.ID)
	}
	q := msgs[2]
	if !q.Queued || q.Origin != "human" || q.Ts != tsUnix("2026-10-20T11:11:57.687Z") {
		t.Errorf("queued row = %+v", q)
	}
	if want := queuedID("2026-10-20T11:11:57.687Z", "ok so the upgrade is done, now back to the vpn issue"); q.ID != want || !strings.HasPrefix(q.ID, "queue:2026-10-20T11:11:57.687Z:") || len(q.ID) != len("queue:2026-10-20T11:11:57.687Z:")+12 {
		t.Errorf("queued id = %q", q.ID)
	}
}

// TestQueuedCommandIncrementalTail: a queued_command that lands in the appended
// tail of a cached transcript is picked up by the incremental parse.
func TestQueuedCommandIncrementalTail(t *testing.T) {
	data, err := os.ReadFile("testdata/claude/queued-command.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines[:4], "")), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := parseTranscript(p)
	if err != nil || len(first) != 2 {
		t.Fatalf("first parse: %v %+v", err, first)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(strings.Join(lines[4:], ""))
	f.Close()
	next := time.Now().Add(2 * time.Second)
	os.Chtimes(p, next, next)
	all, err := parseTranscript(p)
	if err != nil || len(all) != 6 || !all[2].Queued || all[2].Content != "ok so the upgrade is done, now back to the vpn issue" {
		t.Fatalf("after append: %v %+v", err, all)
	}
}

func TestSkipLineAttachments(t *testing.T) {
	if !skipLine([]byte(`{"attachment":{"type":"hook_additional_context"},"type":"attachment","uuid":"x"}`)) {
		t.Error("non-queued attachment should be skipped before decoding")
	}
	if skipLine([]byte(`{"attachment":{"type":"queued_command","prompt":"x"},"type":"attachment","uuid":"x"}`)) {
		t.Error("queued_command attachment must be decoded")
	}
}

// TestAnnotateRows: kind and origin are derived centrally, on a copy.
func TestAnnotateRows(t *testing.T) {
	in := []histMsg{
		{Role: "user", Content: "hello"},
		{Role: "user", Content: "[agent-deck from:60b6d3ab-1]\nhi"},
		{Role: "reply", Content: "r"},
		{Role: "commentary", Content: "c"},
		{Role: "bash", Content: "ls"},
		{Role: "user", Content: "q", Queued: true, Origin: "human", Kind: "user"},
	}
	out := annotateRows(in)
	type ko struct{ k, o string }
	want := []ko{{"user", "human"}, {"user", "peer:60b6d3ab-1"}, {"assistant", ""}, {"assistant", ""}, {"bash", ""}, {"user", "human"}}
	for i, w := range want {
		if out[i].Kind != w.k || out[i].Origin != w.o {
			t.Errorf("row %d = kind %q origin %q, want %v", i, out[i].Kind, out[i].Origin, w)
		}
	}
	if in[0].Kind != "" || in[0].Origin != "" {
		t.Error("annotateRows mutated its input")
	}
}

// fakeRecallBin writes a stub agent-deck whose `recall timeline` behaviour is
// chosen by the flag present, logging argv (one line per call).
func fakeRecallBin(t *testing.T, tail, since, before string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "agent-deck"), filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  *--tail*) cat <<'EOF'\n" + tail + "\nEOF\n;;\n" +
		"  *--since*) cat <<'EOF'\n" + since + "\nEOF\n;;\n" +
		"  *--before*) cat <<'EOF'\n" + before + "\nEOF\n;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

const (
	recallTailJSON = `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n1"},"turns":[
{"id":"t1","kind":"tool","ts":"2026-10-20T11:10:30.000Z","body":"big tool output"},
{"id":"u2","kind":"user","ts":"2026-10-20T11:11:00.000Z","body":"typed while idle"},
{"id":"as2","kind":"assistant","ts":"2026-10-20T11:11:30.000Z","body":"Working on it."},
{"id":"a-q","kind":"system","ts":"2026-10-20T11:11:57.687Z","body":"ok so the upgrade is done","raw_type":"attachment/queued_command"},
{"id":"a-peer","kind":"system","ts":"2026-10-20T11:12:00.000Z","body":"[agent-deck from:60b6d3ab-1]\nHandoff","raw_type":"attachment/queued_command"},
{"id":"a-tn","kind":"system","ts":"2026-10-20T11:12:05.000Z","body":"<task-notification>x</task-notification>","raw_type":"attachment/queued_command"},
{"id":"queue:2026-10-20T11:12:08.000Z:aaaaaaaaaaaa","kind":"user","queued":true,"ts":"2026-10-20T11:12:08.000Z","body":"[agent-deck from:60b6d3ab-1]\nsent while busy"},
{"id":"queue:2026-10-20T11:12:09.000Z:bbbbbbbbbbbb","kind":"user","queued":true,"ts":"2026-10-20T11:12:09.000Z","body":"typed while busy, enqueue row"},
{"id":"queue:2026-10-20T11:12:09.500Z:cccccccccccc","kind":"user","queued":true,"ts":"2026-10-20T11:12:09.500Z","body":"echo hi"},
{"id":"u-bash","kind":"user","ts":"2026-10-20T11:12:10.000Z","body":"<bash-input>echo hi</bash-input>"},
{"id":"u-bash-out","kind":"user","ts":"2026-10-20T11:12:11.000Z","body":"<bash-stdout>hi</bash-stdout><bash-stderr></bash-stderr>"},
{"id":"th","kind":"thinking","ts":"2026-10-20T11:12:12.000Z","body":"hmm"}
],"through_cursor":"c1","before_cursor":"b1"}`
	recallSinceJSON = `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n1"},"turns":[
{"id":"as3","kind":"assistant","ts":"2026-10-20T11:13:00.000Z","body":"Both noted."}
],"updates":[{"id":"u2","body":"typed while idle (edited)"}],"removed":["as2"],"through_cursor":"c2","before_cursor":"b1"}`
	recallBeforeJSON = `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n1"},"turns":[
{"id":"u1","kind":"user","ts":"2026-10-20T11:10:00.000Z","body":"start the kong upgrade"}
]}`
)

func TestRecallRowsMapped(t *testing.T) {
	bin, log := fakeRecallBin(t, recallTailJSON, recallSinceJSON, recallBeforeJSON)
	s := testServer(t, config{bin: bin, token: "tok"})
	ca := claudeAdapter{s: s}
	se := sessionInfo{ID: "s1", Tool: "claude"}

	msgs, older, ok := ca.historyWindow(context.Background(), se, 3)
	if !ok {
		t.Fatal("recall path not used")
	}
	var got []string
	for _, m := range annotateRows(msgs) {
		got = append(got, m.Kind+"/"+m.Origin+"/"+m.Role+":"+m.Content)
	}
	// "need" 3 is met without paging; the older cursor is still advertised.
	want := "user/human/user:typed while idle|assistant//reply:Working on it.|user/human/user:ok so the upgrade is done|peer/peer:60b6d3ab-1/peer:Handoff|peer/peer:60b6d3ab-1/peer:sent while busy|user/human/user:typed while busy, enqueue row|bash//bash:echo hi"
	if strings.Join(got, "|") != want {
		t.Fatalf("rows:\n got %q\nwant %q", strings.Join(got, "|"), want)
	}
	if !older {
		t.Error("hasOlder should be true while a before cursor exists")
	}
	if p := msgs[3]; p.Sender != "" || p.Queued != true || p.Content != "Handoff" {
		t.Errorf("peer row = %+v", p)
	}
	if q := msgs[2]; !q.Queued || !strings.HasPrefix(q.ID, "queue:2026-10-20T11:11:57.687Z:") {
		t.Errorf("queued_command row = %+v", q)
	}
	if b := msgs[6]; b.Stdout != "hi" {
		t.Errorf("bash stdout not folded: %+v", b)
	}
	if msgs[0].ID != "u2" || msgs[1].ID != "as2" {
		t.Errorf("native ids not carried: %q %q", msgs[0].ID, msgs[1].ID)
	}

	// Within recallMinInterval the cache answers: no second exec.
	if _, _, ok := ca.historyWindow(context.Background(), se, 3); !ok {
		t.Fatal("cached call failed")
	}
	if n := s.execAdeck.Load(); n != 1 {
		t.Fatalf("execs = %d, want 1", n)
	}

	// Next poll: delta applies new, updated and removed rows; asking for more
	// than is cached pages --before once.
	sv := s.recall.session("default\x00s1")
	sv.lastFetch = time.Now().Add(-time.Minute)
	msgs, older, ok = ca.historyWindow(context.Background(), se, 10)
	if !ok {
		t.Fatal("delta call failed")
	}
	var contents []string
	for _, m := range msgs {
		contents = append(contents, m.Content)
	}
	wantC := "start the kong upgrade|typed while idle (edited)|ok so the upgrade is done|Handoff|sent while busy|typed while busy, enqueue row|echo hi|Both noted."
	if strings.Join(contents, "|") != wantC {
		t.Fatalf("after delta:\n got %q\nwant %q", strings.Join(contents, "|"), wantC)
	}
	if older {
		t.Error("before page had no before_cursor: nothing older should remain")
	}
	argv, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "recall timeline --json --chain --tail 120 s1") ||
		!strings.Contains(lines[1], "recall timeline --json --since c1 s1") ||
		!strings.Contains(lines[2], "recall timeline --json --chain --before b1 --limit 400 s1") {
		t.Errorf("argv log:\n%s", argv)
	}
}

// TestRecallOffFallsBack: with recall switched off in agent-deck the call
// reports ok=false (the transcript parser answers) and is not retried on every
// poll.
func TestRecallOffFallsBack(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent-deck")
	script := "#!/bin/sh\necho \"$*\" >> " + filepath.Join(dir, "argv.log") + "\n" +
		"echo '{\"code\":\"INVALID_OPERATION\",\"error\":\"recall is off: set [recall] enabled = true\"}'\nexit 2\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, config{bin: bin, token: "tok"})
	ca := claudeAdapter{s: s}
	se := sessionInfo{ID: "s1", Tool: "claude"}
	for i := 0; i < 3; i++ {
		if _, _, ok := ca.historyWindow(context.Background(), se, 5); ok {
			t.Fatal("recall off must fall back")
		}
	}
	if n := s.execAdeck.Load(); n != 1 {
		t.Errorf("execs = %d, want 1 (off state is cached)", n)
	}
	t.Setenv("DECK_REMOTE_RECALL", "off")
	s2 := testServer(t, config{bin: bin, token: "tok"})
	if _, _, ok := (claudeAdapter{s: s2}).historyWindow(context.Background(), se, 5); ok || s2.execAdeck.Load() != 0 {
		t.Error("DECK_REMOTE_RECALL=off must not exec")
	}
}

// TestHistoryEndpointRowFields: the wire rows carry id/kind/queued/origin next
// to the old role/content/ts, so the current client keeps working.
func TestHistoryEndpointRowFields(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	t.Setenv("DECK_REMOTE_RECALL", "off")
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tdir := filepath.Join(cfgDir, "projects", convertToClaudeDirName(project))
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile("testdata/claude/queued-command.jsonl")
	if err := os.WriteFile(filepath.Join(tdir, "cid-1.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, config{bin: "/nonexistent", token: "tok"})
	s.lists.m["default"] = listCacheEntry{at: time.Now(), sessions: []sessionInfo{
		{ID: "s1", Tool: "claude", Path: project, ClaudeSessionID: "cid-1"},
	}}
	rec := httptest.NewRecorder()
	s.handleHistory(rec, httptest.NewRequest("GET", "/api/rc/history?id=s1&limit=10", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Messages) != 6 {
		t.Fatalf("decode: %v %s", err, rec.Body)
	}
	q := resp.Messages[2]
	if q["role"] != "user" || q["kind"] != "user" || q["queued"] != true || q["origin"] != "human" ||
		!strings.HasPrefix(q["id"].(string), "queue:") || q["content"] == nil || q["ts"] == nil {
		t.Errorf("queued row on the wire = %v", q)
	}
	if r := resp.Messages[1]; r["role"] != "reply" || r["kind"] != "assistant" || r["id"] != "as-0001" || r["queued"] != nil {
		t.Errorf("reply row on the wire = %v", r)
	}
}

// TestParseTranscriptImagePrompts: a prompt with an image has list content (an
// image-only user line; a queued_command whose prompt is [image, text]). Both
// become user rows with an image count and no bytes; a tool_result carrying an
// image stays out.
func TestParseTranscriptImagePrompts(t *testing.T) {
	msgs, err := parseTranscriptFile("testdata/claude/image-prompts.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range msgs {
		got = append(got, fmt.Sprintf("%s:%q:%d", m.Role, m.Content, m.Images))
	}
	want := `user:"look at this":0|user:"":1|reply:"I see it.":0|user:"that's what it looks like now":1`
	if strings.Join(got, "|") != want {
		t.Fatalf("rows:\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
	if q := msgs[3]; !q.Queued || q.Origin != "human" || !strings.HasPrefix(q.ID, "queue:2026-10-21T05:33:40.245Z:") {
		t.Errorf("queued image row = %+v", q)
	}
	if msgs[1].ID != "u-0002" {
		t.Errorf("image-only row id = %q", msgs[1].ID)
	}
	b, _ := json.Marshal(msgs[1])
	if strings.Contains(string(b), "iVBOR") {
		t.Errorf("image bytes leaked into the row: %s", b)
	}
}

// TestSkipLineImages: a prompt's image list is decoded, a tool_result's is not.
func TestSkipLineImages(t *testing.T) {
	data, _ := os.ReadFile("testdata/claude/image-prompts.jsonl")
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i, want := range map[int]bool{2: true, 3: false} {
		if got := skipLine([]byte(lines[i])); got != want {
			t.Errorf("skipLine(line %d) = %v, want %v", i, got, want)
		}
	}
}

// TestRecallImageRow: recall marks an image prompt with meta.images and an empty
// body; the empty enqueue row of a queued prompt (no images) stays dropped.
func TestRecallImageRow(t *testing.T) {
	var rows []recallRow
	if err := json.Unmarshal([]byte(`[
	 {"id":"b3f094e4","kind":"user","ts":"2026-10-20T18:51:43.602Z","origin":"human","meta":{"images":1},"raw_type":"user"},
	 {"id":"queue:2026-10-21T05:33:40.245Z:e3b0c44298fc","kind":"user","ts":"2026-10-21T05:33:40.245Z","queued":true,"origin":"human","meta":{"delivery":"queued"},"raw_type":"queue-operation"},
	 {"id":"queue:2026-10-21T05:34:06.561Z:a1da3ba15f4f","kind":"user","ts":"2026-10-21T05:34:06.561Z","body":"that's what it looks like now","origin":"human","meta":{"delivery":"absorbed"},"raw_type":"queue-operation"}]`), &rows); err != nil {
		t.Fatal(err)
	}
	msgs := recallMessages(rows)
	if len(msgs) != 2 || msgs[0].Images != 1 || msgs[0].Content != "" || msgs[0].Role != "user" || msgs[1].Content != "that's what it looks like now" {
		t.Fatalf("rows = %+v", msgs)
	}
}
