package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPhoneLogRecordsAskStates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DECK_REMOTE_STATE_DIR", dir)
	s := testServer(t, config{token: "t", homeDir: t.TempDir()})
	s.hub.tap = newPhoneLog(s.cfg.homeDir).tap
	s.hub.publish(map[string]any{"type": "ask-state", "state": "sent", "requestId": "r1", "sessionId": "s1", "text": "ship it", "slash": false})
	s.hub.publish(map[string]any{"type": "ask-state", "state": "queued", "requestId": "r1", "sessionId": "s1", "sendId": "snd1", "position": 2})
	s.hub.publish(map[string]any{"type": "ask-state", "state": "failed", "requestId": "r1", "sessionId": "s1", "error": "boom", "retryable": true})
	s.hub.publish(map[string]any{"type": "ask-state", "state": "stopping", "sessionId": "s1"}) // no request: not a phone row
	s.hub.publish(map[string]any{"type": "reply", "requestId": "r1"})                          // not an ask-state
	b, err := os.ReadFile(filepath.Join(dir, phoneLogName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d: %s", len(lines), b)
	}
	var first, second, third map[string]any
	for i, dst := range []*map[string]any{&first, &second, &third} {
		if err := json.Unmarshal([]byte(lines[i]), dst); err != nil {
			t.Fatal(err)
		}
	}
	if first["state"] != "sent" || first["text"] != "ship it" || first["requestId"] != "r1" || first["ts"] == nil {
		t.Errorf("first = %v", first)
	}
	if second["text"] != nil || second["state"] != "queued" || second["position"] != float64(2) || second["sendId"] != "snd1" {
		t.Errorf("second = %v (text goes only on the event that carries it)", second)
	}
	if third["state"] != "failed" || third["error"] != "boom" || third["retryable"] != true {
		t.Errorf("third = %v", third)
	}
	if fi, _ := os.Stat(filepath.Join(dir, phoneLogName)); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode())
	}
}

func TestPhoneLogRotatesAndCapsText(t *testing.T) {
	dir := t.TempDir()
	p := &phoneLog{path: filepath.Join(dir, phoneLogName)}
	if err := os.WriteFile(p.path, make([]byte, phoneLogMax), 0o600); err != nil {
		t.Fatal(err)
	}
	p.tap(map[string]any{"type": "ask-state", "state": "sent", "requestId": "r", "text": strings.Repeat("é", phoneLogTextMax)})
	if fi, err := os.Stat(p.path + ".1"); err != nil || fi.Size() != phoneLogMax {
		t.Fatalf("old generation not kept: %v", err)
	}
	b, _ := os.ReadFile(p.path)
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("fresh file: %v %q", err, b)
	}
	if txt := rec["text"].(string); len(txt) > phoneLogTextMax || rec["textTruncated"] != true {
		t.Errorf("text not capped: len %d %v", len(txt), rec["textTruncated"])
	}
}

func TestPhoneLogOffWithoutStateDir(t *testing.T) {
	t.Setenv("DECK_REMOTE_STATE_DIR", "")
	p := newPhoneLog("")
	p.tap(map[string]any{"type": "ask-state", "state": "sent", "requestId": "r"}) // must not write to cwd
	if _, err := os.Stat(".agent-deck"); err == nil {
		t.Fatal("wrote to a relative path")
	}
}

// TestPhoneEchoRealTranscript: two messages the operator sent tower from the
// phone (2026-10-21 10:14-10:17Z) landed in tower's transcript as peer messages
// from "unknown" (Claude Code's preamble + caution footer). With the phone log
// of those sends, each becomes one operator row (Origin "phone"), footer gone;
// without it they stay peer rows.
func TestPhoneEchoRealTranscript(t *testing.T) {
	msgs, err := parseTranscriptFile("testdata/claude/phone-echo.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"one thing dr needs is a context/quota view, ie like pulse in cc",
		"and i see i still have that white space at the bottom, but maybe thats just cause we need a deploy",
	}
	if len(msgs) != 2 {
		t.Fatalf("rows = %+v", msgs)
	}
	for i, m := range msgs {
		if m.Role != "peer" || m.Content != want[i] {
			t.Fatalf("row %d = %+v (peer text without the caution footer expected)", i, m)
		}
	}

	dir := t.TempDir()
	log, _ := os.ReadFile("testdata/claude/phone-echo-rows.jsonl")
	if err := os.WriteFile(filepath.Join(dir, phoneLogName), log, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &phoneLog{path: filepath.Join(dir, phoneLogName)}
	if got := p.claimEchoes("someone-else", msgs); got[0].Role != "peer" || got[1].Role != "peer" {
		t.Fatalf("another session's log claimed rows: %+v", got)
	}
	got := annotateRows(p.claimEchoes("f8593170-1792258124", msgs))
	for i, m := range got {
		if m.Role != "user" || m.Kind != "user" || m.Origin != "phone" || m.Content != want[i] {
			t.Errorf("row %d = %+v, want one operator row from the phone", i, m)
		}
	}
	if msgs[0].Role != "peer" {
		t.Error("claimEchoes changed its input")
	}
}

// TestPhoneEchoRules: only sender-less peer rows (transcript "peer:", recall
// "peer:unknown"), same text modulo whitespace, inside the window, one row per
// send; a live send (tap) counts without reading the file back twice.
func TestPhoneEchoRules(t *testing.T) {
	dir := t.TempDir()
	p := &phoneLog{path: filepath.Join(dir, phoneLogName)}
	now := time.Now().Unix()
	p.tap(map[string]any{"type": "ask-state", "state": "sent", "requestId": "r1", "sessionId": "s1", "text": "ship  it\n"})
	peer := func(origin, sender string, ts int64) histMsg {
		return histMsg{Role: "peer", Kind: "peer", Content: "ship it", Origin: origin, Sender: sender, Ts: ts}
	}
	got := p.claimEchoes("s1", []histMsg{
		peer("peer:abc123", "", now),      // a real session sent the same words
		peer("peer:", "tower", now),       // named sender
		peer("peer:unknown", "", now-600), // landed before it was sent
		peer("peer:unknown", "", now+5),   // the echo
		peer("peer:", "", now+6),          // a second copy: the send is used up
	})
	var roles []string
	for _, m := range got {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "peer,peer,peer,user,peer" {
		t.Errorf("roles = %v", roles)
	}
	if len(p.sent) != 1 {
		t.Errorf("sent = %+v (the tapped line was read back again)", p.sent)
	}
	if got := (&phoneLog{}).claimEchoes("s1", []histMsg{peer("peer:", "", now)}); got[0].Role != "peer" {
		t.Error("disabled log claimed a row")
	}
}
