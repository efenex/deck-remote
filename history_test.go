package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHumanText covers the synthetic-user filter: real human messages pass
// through (trailing system-reminders stripped), while harness-injected user turns
// — command wrappers, system reminders, and background-task notifications — are
// dropped so they never render as messages in the thread.
func TestHumanText(t *testing.T) {
	mk := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
	cases := []struct{ name, in, want string }{
		{"plain message kept", "Build the cube nets.", "Build the cube nets."},
		{"task-notification dropped", "<task-notification>\n<task-id>w1</task-id>\n<result>big blob…</result></task-notification>", ""},
		{"task-prompt dropped", "<task-prompt>do x</task-prompt>", ""},
		{"command wrapper dropped", "<command-name>/clear</command-name>", ""},
		{"system-reminder only dropped", "<system-reminder>be good</system-reminder>", ""},
		{"trailing reminder stripped, human text kept", "real text\n<system-reminder>x</system-reminder>", "real text"},
		{"list content (tool result) dropped", "", ""}, // see below: non-string raw
	}
	for _, c := range cases {
		if c.name == "list content (tool result) dropped" {
			if got := humanText(json.RawMessage(`[{"type":"tool_result","content":"x"}]`)); got != "" {
				t.Errorf("list content = %q, want empty", got)
			}
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			if got := humanText(mk(c.in)); got != c.want {
				t.Errorf("humanText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCleanReplyContent covers the reply-content sanitizer: plain text passes
// through untouched; structured/raw JSON (a serialized tool_use block or a
// content-block array) is reduced to its assistant text (or "" when none);
// malformed JSON is returned unchanged rather than discarded.
func TestCleanReplyContent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "Hello, here is the answer.", "Hello, here is the answer."},
		{"empty", "", ""},
		{"text that mentions json", "use a { in your config", "use a { in your config"},
		{
			"tool_use object -> empty",
			`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}`,
			"",
		},
		{
			"single text block object",
			`{"type":"text","text":"extracted body"}`,
			"extracted body",
		},
		{
			"array of text blocks -> joined",
			`[{"type":"text","text":"first"},{"type":"thinking","text":"ignore"},{"type":"text","text":"second"}]`,
			"first\nsecond",
		},
		{
			"array of tool_use only -> empty",
			`[{"type":"tool_use","name":"Bash","input":{}}]`,
			"",
		},
		{
			"leading/trailing whitespace around json",
			"  {\"type\":\"text\",\"text\":\"trimmed\"}  ",
			"trimmed",
		},
		{
			"malformed json object -> unchanged",
			`{"type":"text","text":"oops"`,
			`{"type":"text","text":"oops"`,
		},
		{
			"malformed json array -> unchanged",
			`[{"type":"text"`,
			`[{"type":"text"`,
		},
		{
			"json string scalar -> no text blocks",
			`"just a quoted string"`,
			`"just a quoted string"`, // does not start with { or [, passes through
		},
		// Genuine JSON the model itself produced must NOT be blanked: it is valid
		// JSON but not an Anthropic content-block payload, so it passes through.
		{"json number array document", `[1, 2, 3]`, `[1, 2, 3]`},
		{"json string array document", `["a", "b"]`, `["a", "b"]`},
		{"plain json object document", `{"foo":"bar","n":1}`, `{"foo":"bar","n":1}`},
		{"empty json array", `[]`, `[]`},
		{"array of untyped objects", `[{"a":1},{"b":2}]`, `[{"a":1},{"b":2}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanReplyContent(c.in); got != c.want {
				t.Errorf("cleanReplyContent(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestBashModeEntries covers the operator's `!` shell lines: the <bash-input>
// entry and its separate <bash-stdout>/<bash-stderr> entry fold into one "bash"
// message (command, stdout, stderr); empty output keeps the command; output
// with nothing to attach to and no content is dropped; content stays verbatim
// (escaping is the renderer's job).
func TestBashModeEntries(t *testing.T) {
	mk := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
	run := func(entries ...string) []histMsg {
		var msgs []histMsg
		for _, e := range entries {
			cmd, so, se, isIn, ok := bashEntry(mk(e))
			if !ok {
				t.Fatalf("bashEntry(%q) not recognised", e)
			}
			msgs = addBash(msgs, cmd, so, se, isIn, 1, "")
		}
		return msgs
	}
	cases := []struct {
		name    string
		in      []string
		cmd     string
		so, se  string
		nothing bool
	}{
		{"stdout", []string{"<bash-input> ls -l</bash-input>", "<bash-stdout>a\nb</bash-stdout><bash-stderr></bash-stderr>"}, "ls -l", "a\nb", "", false},
		{"empty stdout and stderr keeps the command", []string{"<bash-input>true</bash-input>", "<bash-stdout></bash-stdout><bash-stderr></bash-stderr>"}, "true", "", "", false},
		{"stderr only", []string{"<bash-input>ls /nope</bash-input>", "<bash-stdout></bash-stdout><bash-stderr>ls: /nope: No such file</bash-stderr>"}, "ls /nope", "", "ls: /nope: No such file", false},
		{"multi-line stdout and stderr", []string{"<bash-input>x</bash-input>", "<bash-stdout>l1\nl2\n\nl4\n</bash-stdout><bash-stderr>e1\ne2</bash-stderr>"}, "x", "l1\nl2\n\nl4", "e1\ne2", false},
		{"markup in output kept verbatim", []string{"<bash-input>echo '<b>'</bash-input>", "<bash-stdout><b>&</b></bash-stdout><bash-stderr></bash-stderr>"}, "echo '<b>'", "<b>&</b>", "", false},
		{"input only (output pending)", []string{"<bash-input>sleep 1</bash-input>"}, "sleep 1", "", "", false},
		{"empty output, no input: dropped", []string{"<bash-stdout></bash-stdout><bash-stderr></bash-stderr>"}, "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := run(c.in...)
			if c.nothing {
				if len(got) != 0 {
					t.Fatalf("got %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d msgs, want 1: %+v", len(got), got)
			}
			m := got[0]
			if m.Role != "bash" || m.Content != c.cmd || m.Stdout != c.so || m.Stderr != c.se {
				t.Errorf("got %+v, want cmd=%q stdout=%q stderr=%q", m, c.cmd, c.so, c.se)
			}
		})
	}
	if _, _, _, _, ok := bashEntry(mk("just a human message")); ok {
		t.Error("plain message recognised as bash")
	}
	if _, _, _, _, ok := bashEntry(json.RawMessage(`[{"type":"text","text":"<bash-input>x</bash-input>"}]`)); ok {
		t.Error("content-block list recognised as bash")
	}
}

// TestParseTranscriptBashMode runs the full parser over a transcript with a
// bash-mode pair between two human messages.
func TestParseTranscriptBashMode(t *testing.T) {
	lines := []string{
		`{"type":"user","timestamp":"2026-10-20T10:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"user","timestamp":"2026-10-20T10:00:01Z","message":{"role":"user","content":"<bash-input>echo hi</bash-input>"}}`,
		`{"type":"user","timestamp":"2026-10-20T10:00:02Z","message":{"role":"user","content":"<bash-stdout>hi</bash-stdout><bash-stderr></bash-stderr>"}}`,
		`{"type":"user","timestamp":"2026-10-20T10:00:03Z","message":{"role":"user","content":"thanks"}}`,
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := parseTranscriptFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].Role != "user" || msgs[2].Content != "thanks" {
		t.Fatalf("msgs = %+v", msgs)
	}
	if b := msgs[1]; b.Role != "bash" || b.Content != "echo hi" || b.Stdout != "hi" || b.Stderr != "" {
		t.Errorf("bash msg = %+v", b)
	}
}
