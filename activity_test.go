package main

import (
	"strings"
	"testing"
)

// TestParseActivity guards the live-activity parser against the self-referential
// false positive where pane SCROLLBACK that merely mentions the marker strings
// (e.g. this parser's own docs) was read as live work. Detection must require a
// real spinner line (with a numeric token count) in the bottom live region, and a
// "current tool" is only meaningful while working.
func TestParseActivity(t *testing.T) {
	t.Run("doc text mentioning markers is idle, not working", func(t *testing.T) {
		// The exact shape that tripped it: a TODO bullet documenting the markers.
		pane := "some earlier output\n" +
			"21 -- **Activity parsing** (`activity.go`): keys off the spinner line " +
			"(`· ↓ tokens`) + `⏺ Tool(`. Brittle if Claude Code changes its TUI.\n"
		a := parseActivity(pane)
		if a.Working {
			t.Errorf("Working=true on doc text; want idle. Activity=%q Tool=%q", a.Activity, a.CurrentTool)
		}
		if a.CurrentTool != "" {
			t.Errorf("CurrentTool=%q on idle pane; want empty", a.CurrentTool)
		}
	})

	t.Run("real spinner at bottom is working", func(t *testing.T) {
		pane := "⏺ Bash(go build ./...)\n  ⎿ Running…\n✻ Vibing… (12s · ↓ 2.1k tokens)\n"
		a := parseActivity(pane)
		if !a.Working {
			t.Fatalf("Working=false on a real spinner line; want working")
		}
		if a.Activity == "" {
			t.Errorf("Activity empty; want the stripped spinner label")
		}
		if a.CurrentTool != "Bash(go build ./...)" {
			t.Errorf("CurrentTool=%q; want Bash(go build ./...)", a.CurrentTool)
		}
	})

	t.Run("completed tool with no spinner is idle, no current tool", func(t *testing.T) {
		// A finished tool call in scrollback while idle must not report a tool.
		pane := "⏺ Read(history.go)\n  ⎿ Read 257 lines\n\n> \n"
		a := parseActivity(pane)
		if a.Working {
			t.Errorf("Working=true while idle; want idle")
		}
		if a.CurrentTool != "" {
			t.Errorf("CurrentTool=%q while idle; want empty", a.CurrentTool)
		}
	})

	t.Run("spinner without a token count is not working", func(t *testing.T) {
		// "↓ tokens" with no number is documentation, not a live counter.
		pane := "the format is `· ↓ tokens` shown while working\n"
		if parseActivity(pane).Working {
			t.Errorf("Working=true on a numberless token mention; want idle")
		}
	})

	// A completed subagent batch line carries a "↓ N tokens" counter but NOT in
	// parentheses, and freezes once done ("N/N agents done") — the exact false
	// stall observed in the wild. It must not read as a live spinner.
	t.Run("completed subagent batch line is not a spinner", func(t *testing.T) {
		pane := "numberblocks-foldables  Design printable fold-and-build paper-craft cube nets, then verify\n" +
			"  9/9 agents done · 13m 19s · ↓ 397.4k tokens\n"
		if a := parseActivity(pane); a.Working {
			t.Errorf("Working=true on a completed subagent line; want idle. Activity=%q", a.Activity)
		}
	})

	t.Run("subagent line with parens in the description is not a spinner", func(t *testing.T) {
		// A "(read-only)" in the task desc must not satisfy the parenthesized-counter
		// requirement — the counter itself is outside any parens.
		pane := "burndown-iter85  Scout the 8 items (read-only), then gate\n" +
			"  9/9 agents done · 16m 58s · ↓ 731.4k tokens\n"
		if parseActivity(pane).Working {
			t.Errorf("Working=true on a subagent line whose only parens are in the desc; want idle")
		}
	})

	t.Run("wrapped token fragment is not a spinner", func(t *testing.T) {
		if parseActivity("22.4k tokens)\n").Working {
			t.Errorf("Working=true on a wrapped counter fragment; want idle")
		}
	})

	t.Run("real spinner below a subagent summary is picked over it", func(t *testing.T) {
		pane := "numberblocks-foldables  Design …\n" +
			"  9/9 agents done · 13m 19s · ↓ 397.4k tokens\n" +
			"✻ Whisking… (5m 46s · ↓ 15.8k tokens · almost done thinking)\n"
		a := parseActivity(pane)
		if !a.Working {
			t.Fatalf("Working=false with a live spinner present; want working")
		}
		if !strings.HasPrefix(a.Activity, "Whisking…") {
			t.Errorf("Activity=%q; want the live spinner line, not the subagent summary", a.Activity)
		}
	})
}
