package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chainBin is a fake agent-deck. With chain=false it rejects --chain like a
// build without the verb; otherwise `timeline --chain` answers a tail read that
// spans a /clear, then pages back to the chain start.
func chainBin(t *testing.T, chain bool) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "agent-deck"), filepath.Join(dir, "argv.log")
	// Amended contract: before_cursor is always an ordinary opaque cursor ("" at the
	// chain start, never seg:<id>); a boundary row sits right before the first row
	// of its segment and may end the next-older page while that first row is on the
	// newer one (here boundary:n2 is on both pages: dedupe by id).
	tailChain := `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n2"},"turns":[
{"id":"boundary:n2","kind":"system","raw_type":"chain/boundary","title":"Conversation cleared","seg":"n2","meta":{"boundary":"clear","seg":"n2","prev_seg":"n1"}},
{"id":"u2","kind":"user","origin":"human","seg":"n2","ts":"2026-10-20T12:00:00.000Z","body":"after clear"}
],"through_cursor":"c2","before_cursor":"cur-b1"}`
	pageChain := `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n2"},"turns":[
{"id":"line::aaaa","kind":"other","seg":"n1","body":""},
{"id":"line::aaaa","kind":"other","seg":"n1","body":""},
{"id":"u1","kind":"user","origin":"human","seg":"n1","ts":"2026-10-20T11:00:00.000Z","body":"before clear"},
{"id":"boundary:n2","kind":"system","raw_type":"chain/boundary","title":"Conversation cleared","seg":"n2","meta":{"boundary":"clear","seg":"n2","prev_seg":"n1"}}
],"before_cursor":""}`
	plain := `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n2"},"turns":[
{"id":"u2","kind":"user","ts":"2026-10-20T12:00:00.000Z","body":"after clear"}
],"through_cursor":"c2","before_cursor":"b1"}`
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in\n"
	if chain {
		script += "  *--chain*--before*) cat <<'EOF'\n" + pageChain + "\nEOF\n;;\n" +
			"  *--chain*) cat <<'EOF'\n" + tailChain + "\nEOF\n;;\n"
	} else {
		script += "  *--chain*) echo 'flag provided but not defined: -chain' >&2; exit 2;;\n"
	}
	script += "  *--tail*) cat <<'EOF'\n" + plain + "\nEOF\n;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestChainHistorySpansClear(t *testing.T) {
	bin, log := chainBin(t, true)
	s := testServer(t, config{bin: bin, token: "tok"})
	ca := claudeAdapter{s: s}
	se := sessionInfo{ID: "s1", Tool: "claude"}
	msgs, older, ok := ca.historyWindow(context.Background(), se, 2)
	if !ok || !older {
		t.Fatalf("ok=%v older=%v", ok, older)
	}
	if got := contents(msgs); got != "Conversation cleared|after clear" {
		t.Fatalf("tail rows = %q", got)
	}
	if msgs[0].Role != "boundary" || msgs[0].Kind != "boundary" {
		t.Errorf("boundary row = %+v", msgs[0])
	}
	// scroll back past the /clear: the opaque cursor is passed back with --chain;
	// the page that ends with the (duplicate) boundary adds one row and no second
	// divider, and its empty before_cursor ends the chain.
	msgs, older, ok = ca.historyWindow(context.Background(), se, 3)
	if !ok || contents(msgs) != "before clear|Conversation cleared|after clear" {
		t.Fatalf("paged rows = %q ok=%v", contents(msgs), ok)
	}
	if older {
		t.Error(`before_cursor "" is the chain start`)
	}
	argv := readFile(t, log)
	for _, want := range []string{"--chain --tail", "--chain --before cur-b1 --limit"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv log lacks %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "seg:") {
		t.Errorf("seg: cursors are never produced:\n%s", argv)
	}
}

// A boundary that ends the next-older page, with its segment's first row on the
// newer page, lands between them.
func TestChainBoundaryAtPageEnd(t *testing.T) {
	sv := &recallSess{rows: []recallRow{{ID: "u2", Kind: "user", Body: "new seg first"}}}
	page := []recallRow{
		{ID: "u1", Kind: "user", Body: "old seg last"},
		{ID: "boundary:n2", Kind: "system", RawType: "chain/boundary", Title: "Conversation cleared"},
	}
	sv.rows = append(page, sv.rows...)
	if got := contents(recallMessages(sv.rows)); got != "old seg last|Conversation cleared|new seg first" {
		t.Errorf("rows = %q", got)
	}
}

// `!` lines from a fork that emits kind:bash + meta.user_shell: command in
// detail, output on an update (is_error when stderr only).
func TestRecallUserShellRow(t *testing.T) {
	sv := &recallSess{}
	var f1, f2, f3, f4 recallFrame
	_ = json.Unmarshal([]byte(`{"frame":"row","row":{"id":"sh1","kind":"bash","origin":"human","raw_type":"user","detail":"echo hi","title":"echo hi","finished":false,"meta":{"user_shell":true}},"cursor":"c1"}`), &f1)
	_ = json.Unmarshal([]byte(`{"frame":"update","row":{"id":"sh1","body":"hi\n","finished":true,"meta":{"user_shell":true}},"cursor":"c2"}`), &f2)
	_ = json.Unmarshal([]byte(`{"frame":"row","row":{"id":"tool1","kind":"bash","detail":"ls","body":"x"},"cursor":"c3"}`), &f3) // the Bash tool: not a thread row
	_ = json.Unmarshal([]byte(`{"frame":"row","row":{"id":"sh2","kind":"bash","origin":"human","detail":"false","meta":{"user_shell":true}}}`), &f4)
	var f5 recallFrame
	_ = json.Unmarshal([]byte(`{"frame":"update","row":{"id":"sh2","body":"boom","is_error":true,"meta":{"user_shell":true}},"cursor":"c4"}`), &f5)
	for _, f := range []recallFrame{f1, f2, f3, f4, f5} {
		sv.applyFrame(f)
	}
	msgs := recallMessages(sv.rows)
	if len(msgs) != 2 {
		t.Fatalf("msgs = %+v", msgs)
	}
	if m := msgs[0]; m.Role != "bash" || m.Content != "echo hi" || m.Stdout != "hi" || m.Stderr != "" {
		t.Errorf("shell row = %+v", m)
	}
	if m := msgs[1]; m.Content != "false" || m.Stderr != "boom" || m.Stdout != "" {
		t.Errorf("stderr row = %+v", m)
	}
	if sv.through != "c4" {
		t.Errorf("cursor = %q", sv.through)
	}
}

// A build without `timeline --chain` keeps working on the single-segment path,
// and the rejection is remembered (one probe, not one per request).
func TestChainFallsBackWithoutVerb(t *testing.T) {
	bin, log := chainBin(t, false)
	s := testServer(t, config{bin: bin, token: "tok"})
	ca := claudeAdapter{s: s}
	se := sessionInfo{ID: "s1", Tool: "claude"}
	msgs, _, ok := ca.historyWindow(context.Background(), se, 1)
	if !ok || contents(msgs) != "after clear" {
		t.Fatalf("fallback rows = %q ok=%v", contents(msgs), ok)
	}
	if s.recall.disabled() {
		t.Error("an unsupported --chain must not switch recall off")
	}
	se2 := sessionInfo{ID: "s2", Tool: "claude"}
	ca.historyWindow(context.Background(), se2, 1)
	if n := strings.Count(readFile(t, log), "--chain"); n != 1 {
		t.Errorf("--chain probes = %d, want 1", n)
	}
}
