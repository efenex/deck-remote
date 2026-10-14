package main

import (
	"os"
	"strings"
	"testing"
)

const claudeDialog = `╭───────────────────────────────────────────╮
│ Bash command                              │
│                                           │
│   go test ./...                           │
│   Run the tests                           │
│                                           │
│ Do you want to proceed?                   │
│ ❯ 1. Yes                                  │
│   2. Yes, and don't ask again for go test │
│   3. No, and tell Claude what to do       │
│      differently (esc)                    │
╰───────────────────────────────────────────╯
`

const idlePrompt = `╭───────────────────────────────────────────╮
│ >                                         │
╰───────────────────────────────────────────╯
  ? for shortcuts
`

func TestClaudePermissionPromptLiveDialog(t *testing.T) {
	pane := strings.Repeat("earlier output\n", 50) + claudeDialog + "\n\n\n"
	if !isClaudePermissionPrompt(pane) {
		t.Fatal("live dialog at the bottom of the pane not detected")
	}
}

// A dialog that was answered long ago, or transcript text quoting a marker,
// still sits in the 2000-line scrollback. It must not read as pending.
func TestClaudePermissionPromptIgnoresScrollback(t *testing.T) {
	for name, above := range map[string]string{
		"old dialog": claudeDialog,
		"quoted":     "● The CLI asks \"Do you want to proceed?\" before it runs.\n",
	} {
		pane := above + strings.Repeat("⏺ more work\n", 30) + idlePrompt
		if isClaudePermissionPrompt(pane) {
			t.Errorf("%s: scrollback read as a pending permission dialog", name)
		}
	}
}

// A real capture (from agent-deck's pane corpus): the dialog sits above a
// background-task row.
func TestClaudePermissionPromptRealCapture(t *testing.T) {
	b, err := os.ReadFile("testdata/claude/permission-with-background-task.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !isClaudePermissionPrompt(string(b)) {
		t.Fatal("real permission capture not detected")
	}
}

// The marker alone near the bottom (a reply quoting it) is not a dialog.
func TestClaudePermissionPromptNeedsSelector(t *testing.T) {
	pane := "● It will ask \"Do you want to proceed?\" and offer \"Yes, and don't ask again\".\n" + idlePrompt
	if isClaudePermissionPrompt(pane) {
		t.Fatal("quoted marker at an idle prompt read as a dialog")
	}
}
