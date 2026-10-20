package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPhoneLogRecordsAskStates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DECK_REMOTE_STATE_DIR", dir)
	s := testServer(t, config{token: "t", homeDir: t.TempDir()})
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
