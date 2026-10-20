package main

import (
	"encoding/json"
	"testing"
)

func TestParsePeer(t *testing.T) {
	cases := []struct {
		in, from, name, text string
		ok                   bool
	}{
		{"Another Claude session sent a message:\n[agent-deck from:f859-1]\n[tower → x] task\nbody", "f859-1", "", "[tower → x] task\nbody", true},
		{"[agent-deck from:60b6]\nhi", "60b6", "", "hi", true},
		{"<cross-session-message from=\"uds:/tmp/a.sock\" from-name=\"ga-offload\" from-mode=\"prompting\">\nETA correction\nline2</cross-session-message>", "uds:/tmp/a.sock", "ga-offload", "ETA correction\nline2", true},
		{"Another Claude session sent a message:\nno envelope", "", "", "no envelope", true},
		{"just a prompt", "", "", "", false},
		{"[tui] picked suggestion", "", "", "", false},
	}
	for _, c := range cases {
		from, name, text, ok := parsePeer(c.in)
		if from != c.from || name != c.name || text != c.text || ok != c.ok {
			t.Errorf("parsePeer(%q) = %q %q %q %v; want %q %q %q %v", c.in, from, name, text, ok, c.from, c.name, c.text, c.ok)
		}
	}
}

func TestParseSlash(t *testing.T) {
	cases := map[string]string{
		"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>": "/clear",
		"<command-message>tick</command-message>\n<command-name>/tick</command-name>":                                      "/tick",
		"<command-name>/model</command-name><command-args> opus </command-args>":                                           "/model opus",
		"<command-name>review</command-name>":                                                                              "/review",
	}
	for in, want := range cases {
		if got, ok := parseSlash(in); !ok || got != want {
			t.Errorf("parseSlash(%q) = %q %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := parseSlash("<local-command-stdout>x</local-command-stdout>"); ok {
		t.Error("local-command output is not a slash row")
	}
}

func TestUserEntryRowKinds(t *testing.T) {
	enc := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
	if m, ok := userEntryRow(enc("Another Claude session sent a message:\n[agent-deck from:abc]\nhello there"), 5, "u1", false); !ok || m.Role != "peer" || m.Origin != "peer:abc" || m.Content != "hello there" {
		t.Errorf("peer = %+v %v", m, ok)
	}
	if m, ok := userEntryRow(enc("<command-name>/tick</command-name>"), 5, "u2", false); !ok || m.Role != "slash" || m.Content != "/tick" {
		t.Errorf("slash = %+v %v", m, ok)
	}
	if _, ok := userEntryRow(enc("<task-notification>\n<task-id>x</task-id></task-notification>"), 5, "u3", false); ok {
		t.Error("task-notifications stay hidden")
	}
	if _, ok := userEntryRow(enc("<local-command-caveat>c</local-command-caveat>"), 5, "u4", false); ok {
		t.Error("local-command caveat stays hidden")
	}
	if m, ok := userEntryRow(enc("plain"), 5, "u5", true); !ok || m.Role != "user" || !m.Queued {
		t.Errorf("human = %+v %v", m, ok)
	}
}

func TestRecallMessagesPeerSlashOrigin(t *testing.T) {
	rows := []recallRow{
		{ID: "p1", Kind: "system", RawType: "user", Body: "Another Claude session sent a message:\n[agent-deck from:abc]\nhi from tower"},
		{ID: "c1", Kind: "command", RawType: "user", Body: "<command-name>/tick</command-name>\n<command-args>now</command-args>"},
		{ID: "c2", Kind: "command", RawType: "system/local_command", Body: "<local-command-stdout></local-command-stdout>"},
		{ID: "t1", Kind: "system", RawType: "queue-operation", Body: "<task-notification>x</task-notification>"},
		{ID: "o1", Kind: "user", Origin: "task-notification", Body: "whatever"},
		{ID: "o2", Kind: "system", Origin: "peer:zzz", RawType: "user", Body: "plain body from the fork"},
	}
	var kept []recallRow
	for _, r := range rows {
		if recallKeep(r) {
			kept = append(kept, r)
		}
	}
	msgs := recallMessages(kept)
	if len(msgs) != 3 {
		t.Fatalf("msgs = %+v", msgs)
	}
	if m := msgs[0]; m.Role != "peer" || m.Origin != "peer:abc" || m.Content != "hi from tower" {
		t.Errorf("peer = %+v", m)
	}
	if m := msgs[1]; m.Role != "slash" || m.Content != "/tick now" {
		t.Errorf("slash = %+v", m)
	}
	if m := msgs[2]; m.Role != "peer" || m.Origin != "peer:zzz" || m.Content != "plain body from the fork" {
		t.Errorf("origin-field peer = %+v", m)
	}
}
