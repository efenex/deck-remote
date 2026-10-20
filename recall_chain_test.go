package main

import (
	"context"
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
	tailChain := `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n2"},"turns":[
{"id":"boundary:n2","kind":"system","raw_type":"chain/boundary","title":"Conversation cleared","seg":"n2","meta":{"boundary":"clear","seg":"n2","prev_seg":"n1"}},
{"id":"u2","kind":"user","seg":"n2","ts":"2026-10-20T12:00:00.000Z","body":"after clear"}
],"through_cursor":"c2","before_cursor":"seg:n1"}`
	pageChain := `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n2"},"turns":[
{"id":"u1","kind":"user","seg":"n1","ts":"2026-10-20T11:00:00.000Z","body":"before clear"}
],"before_cursor":"seg:n0"}`
	emptyPage := `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n2"},"turns":[],"before_cursor":""}`
	plain := `{"schema":"agent-deck.recall.rows/v2","session":{"id":"s1","native_id":"n2"},"turns":[
{"id":"u2","kind":"user","ts":"2026-10-20T12:00:00.000Z","body":"after clear"}
],"through_cursor":"c2","before_cursor":"b1"}`
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in\n"
	if chain {
		script += "  *--chain*--before\\ seg:n0*) cat <<'EOF'\n" + emptyPage + "\nEOF\n;;\n" +
			"  *--chain*--before*) cat <<'EOF'\n" + pageChain + "\nEOF\n;;\n" +
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
	// scroll back past the /clear: the seg: cursor is passed back with --chain
	msgs, older, ok = ca.historyWindow(context.Background(), se, 3)
	if !ok || contents(msgs) != "before clear|Conversation cleared|after clear" {
		t.Fatalf("paged rows = %q ok=%v", contents(msgs), ok)
	}
	if !older {
		t.Error("still older: seg:n0 not yet read")
	}
	msgs, older, _ = ca.historyWindow(context.Background(), se, 9)
	if older {
		t.Error("an empty page is the chain start")
	}
	argv := readFile(t, log)
	for _, want := range []string{"--chain --tail", "--chain --before seg:n1 --limit", "--chain --before seg:n0 --limit"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv log lacks %q:\n%s", want, argv)
		}
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
