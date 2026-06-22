package main

import (
	"encoding/json"
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
