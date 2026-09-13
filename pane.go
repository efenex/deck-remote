package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Pane capture. The old path shelled `agent-deck session output --pane
// --primary` — a full agent-deck startup per capture, per session, per 6s
// sweep (the bulk of the CLI polling burn). agent-deck itself just runs
// `tmux capture-pane -t <session>:^ -p -e -S -2000` under the hood
// (internal/tmux CapturePrimaryFullHistory), so we run the same tiny tmux
// client directly and keep the CLI only as a fallback for sessions we can't
// target (no tmux_session in the list, or a capture error agent-deck might
// recover from by reviving/resolving the session).

// tmuxCapturePane replicates agent-deck's primary-window capture: the managed
// first window (":^"), printed with ANSI escapes, last 2000 lines of history —
// byte-parity with `session output --pane --primary` so every downstream
// parser (activity, permission dialogs, slash snapshots) sees the same text.
func (s *server) tmuxCapturePane(ctx context.Context, socket, tmuxSession string) (string, error) {
	if tmuxSession == "" {
		return "", fmt.Errorf("no tmux session name")
	}
	bin := s.cfg.tmuxBin
	if bin == "" {
		bin = "tmux"
	}
	args := []string{}
	if socket != "" {
		args = append(args, "-L", socket)
	}
	args = append(args, "capture-pane", "-t", tmuxSession+":^", "-p", "-e", "-S", "-2000")
	s.execTmux.Add(1)
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("tmux capture-pane %s: %w: %s", tmuxSession, err, stderr)
	}
	return string(out), nil
}

// tmuxSessionGone reports whether a capture error means the tmux session
// definitively does not exist (dead session / no server on the socket). Most
// tracked sessions are stopped at any given time; falling back to the CLI for
// them would re-spawn agent-deck per dead session per sweep — the exact churn
// this file removes — and the CLI cannot conjure a pane tmux says is gone.
func tmuxSessionGone(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"can't find session", "session not found", "no server running", "error connecting to"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// paneFor captures the pane for an already-resolved session: tmux directly,
// agent-deck CLI as fallback for non-definitive failures only.
func (s *server) paneFor(ctx context.Context, se sessionInfo) (string, error) {
	out, err := s.tmuxCapturePane(ctx, se.TmuxSocket, se.TmuxSession)
	if err == nil {
		return out, nil
	}
	if tmuxSessionGone(err) {
		return "", err
	}
	return s.cliSessionPane(ctx, se.ID)
}
