package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countdownDialog is the unattended-session dialog Claude Code draws for a command it could
// not check: the ⚠ line counts down once a second and the harness blocks the request itself
// at 0:00.
func countdownDialog(mmss string) string {
	return "⏺ Bash(sh -c 'rm -rf build')\n\n" +
		" Bash command\n\n   sh -c 'rm -rf build'\n   Remove the build dir\n\n" +
		" This shell -c script runs rm and could not be checked\n" +
		" ⚠ Claude Code will automatically block this request in " + mmss + ", to avoid holding up an unattended session\n\n" +
		" Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n Esc to cancel · Tab to amend\n"
}

func TestCountdownDoesNotChangeApprovalID(t *testing.T) {
	var first attentionInfo
	for i, tick := range []string{"0:30", "0:16", "0:15", "0:01", "0:00"} {
		info, err := claudeAdapter{}.Attention(context.Background(), sessionInfo{}, countdownDialog(tick), true)
		if err != nil || !info.Pending || info.Kind != "approval" {
			t.Fatalf("tick %s: not a pending approval: %+v %v", tick, info, err)
		}
		if strings.Contains(info.Text, "automatically") || strings.Contains(info.Text, tick) {
			t.Errorf("tick %s: countdown leaked into the card text:\n%s", tick, info.Text)
		}
		if !strings.Contains(info.Text, "could not be checked") || !strings.Contains(info.Text, "rm -rf build") {
			t.Errorf("tick %s: dialog body lost:\n%s", tick, info.Text)
		}
		if i == 0 {
			first = info
		} else if info.ID != first.ID || info.Text != first.Text {
			t.Errorf("tick %s: ID %s != %s (the fingerprint follows the countdown)", tick, info.ID, first.ID)
		}
	}
}

func TestCountdownDeadline(t *testing.T) {
	now := time.Date(2026, 10, 31, 16, 52, 40, 0, time.UTC)
	defer func(f func() time.Time) { claudeNow = f }(claudeNow)
	claudeNow = func() time.Time { return now }
	info, _ := claudeAdapter{}.Attention(context.Background(), sessionInfo{}, countdownDialog("0:16"), true)
	if want := now.Add(16 * time.Second).Unix(); info.ExpiresAt != want {
		t.Errorf("ExpiresAt = %d, want %d", info.ExpiresAt, want)
	}
	// A dialog without the timer has no deadline.
	plain, _ := claudeAdapter{}.Attention(context.Background(), sessionInfo{}, claudeDialog, true)
	if plain.ExpiresAt != 0 || !plain.Pending {
		t.Errorf("plain dialog: %+v", plain)
	}
}

// A narrow terminal wraps the timer sentence; the ticking digits must still stay out of the ID.
func TestCountdownWrappedLine(t *testing.T) {
	wrap := func(mmss string) string {
		return strings.Replace(countdownDialog(mmss),
			"automatically block this request in "+mmss+", to avoid holding up an unattended session",
			"automatically block this\n request in\n "+mmss+", to avoid holding up an\n unattended session", 1)
	}
	a, _ := claudeAdapter{}.Attention(context.Background(), sessionInfo{}, wrap("0:20"), true)
	b, _ := claudeAdapter{}.Attention(context.Background(), sessionInfo{}, wrap("0:19"), true)
	if !a.Pending || a.ID != b.ID {
		t.Errorf("wrapped timer changes the ID: %s vs %s\n%s", a.ID, b.ID, a.Text)
	}
}

// A different command is a different dialog even with the same timer.
func TestCountdownKeepsCommandIdentity(t *testing.T) {
	a, _ := claudeAdapter{}.Attention(context.Background(), sessionInfo{}, countdownDialog("0:16"), true)
	other := strings.ReplaceAll(countdownDialog("0:16"), "rm -rf build", "rm -rf /home")
	b, _ := claudeAdapter{}.Attention(context.Background(), sessionInfo{}, other, true)
	if a.ID == b.ID {
		t.Error("two different commands share an approval ID")
	}
}

// End to end: the pane differs on EVERY capture (the timer ticks), the phone approves with the
// ID it fetched earlier, and the keystrokes go out.
func TestApproveWithTickingCountdown(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	for i := 0; i < 40; i++ {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("pane%02d", i)), []byte(countdownDialog(fmt.Sprintf("0:%02d", 40-i))), 0o644)
	}
	bin, _ := fakeTmux(t, `case "$*" in
  *list-panes*) echo tick-sess;;
  *capture-pane*)
    n=$(cat `+counter+` 2>/dev/null || echo 0); echo $((n+1)) > `+counter+`
    cat `+dir+`/pane$(printf %02d $n);;
  *) exit 1;;
esac`)
	adBin, adLog := fakeTmux(t, `exit 0`)
	s := testServer(t, config{token: "tok", tmuxBin: bin, bin: adBin})
	se := sessionInfo{ID: "t1", Title: "tick", Tool: "claude", TmuxSession: "tick-sess"}
	s.lists.mu.Lock()
	s.lists.m["default"] = listCacheEntry{sessions: []sessionInfo{se}, at: time.Now()}
	s.lists.mu.Unlock()

	card, _ := claudeAdapter{s}.Attention(context.Background(), se, countdownDialog("0:41"), true)
	res, err := claudeAdapter{s}.RespondAttention(context.Background(), se, attentionResponseRequest{AttentionID: card.ID, Action: "approve-once"})
	if err != nil || !res.Responded {
		t.Fatalf("approve against a ticking dialog refused: %+v %v", res, err)
	}
	log, _ := os.ReadFile(adLog)
	if !strings.Contains(string(log), "--text 1") || !strings.Contains(string(log), "--enter") {
		t.Errorf("keystrokes not sent:\n%s", log)
	}
}
