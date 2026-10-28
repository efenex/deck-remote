package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Opt-in real TUI check. Use ONLY an isolated server with a session named fixture,
// parked on multiAsk(): Multi / Pick several? / One, Two, Three / multiSelect=true.
// See testdata/claude/askq-multi-semantics.md. Never targets the default tmux server.
func TestMultiSelectLiveFixture(t *testing.T) {
	socket := os.Getenv("DECK_REMOTE_ASK_FIXTURE_TMUX")
	if socket == "" {
		t.Skip("requires an isolated Claude TUI fixture")
	}
	if socket == "default" {
		t.Fatal("refusing the default tmux server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", socket}, args...)...).CombinedOutput()
		return string(out), err
	}
	io := askIO{
		pane: func(context.Context) (string, error) { return run("capture-pane", "-pt", "fixture") },
		text: func(_ context.Context, text string) error {
			t.Logf("text: %q", text)
			_, err := run("send-keys", "-t", "fixture", "-l", text)
			return err
		},
		named: func(_ context.Context, key string) error {
			t.Logf("key: %s", key)
			_, err := run("send-keys", "-t", "fixture", key)
			return err
		},
		wait: 100 * time.Millisecond, polls: 30,
	}
	pane, err := io.pane(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := askMenuInfo(pane, multiAsk())
	if !ok || info.ReadOnly {
		t.Fatal("fixture is not ready")
	}
	io.initialState = info.MenuState
	done, cleared, err := answerAsk(ctx, io, multiAsk(), []askPick{{Options: []int{1, 3}, Text: "phone answer"}})
	if err != nil || done != 1 || !cleared {
		t.Fatalf("done=%d cleared=%v err=%v", done, cleared, err)
	}
}
