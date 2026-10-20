package main

import (
	"context"
	"errors"
	"fmt"
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
	out, err := s.tmuxExec(ctx, socket, "capture-pane", "-t", tmuxSession+":^", "-p", "-e", "-S", "-2000")
	if err != nil {
		if errors.Is(err, errTmuxThrottled) {
			return "", err
		}
		return "", fmt.Errorf("tmux capture-pane %s: %w", tmuxSession, err)
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
	if s.tmux.gone(se.TmuxSocket, se.TmuxSession) {
		// A recent list-panes did not list it: no exec, no CLI fallback.
		return "", fmt.Errorf("tmux capture-pane %s: can't find session (not in list-panes)", se.TmuxSession)
	}
	out, err := s.tmuxCapturePane(ctx, se.TmuxSocket, se.TmuxSession)
	if err == nil {
		s.tmux.touched(se.ID)
		return out, nil
	}
	if tmuxSessionGone(err) {
		s.tmux.touched(se.ID)
		return "", err
	}
	if errors.Is(err, errTmuxThrottled) {
		return "", err // budget or backoff: not a reason to spend an agent-deck exec instead
	}
	return s.cliSessionPane(ctx, se.ID)
}
