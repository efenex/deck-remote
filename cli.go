package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ansiRe matches the ANSI escape sequences tmux capture-pane emits (CSI + OSC +
// stray ESC), so detection/excerpts work on clean text.
var ansiRe = regexp.MustCompile("\x1b\\[[0-9;:?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-Z\\-_]")

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// subtleConstantEq is a constant-time string compare for the bearer token.
func subtleConstantEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// adeck runs the stock agent-deck CLI for the configured profile and returns
// stdout. The profile is passed as the global -p flag (before the subcommand).
func (s *server) adeck(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"-p", s.cfg.profile}, args...)
	cmd := exec.CommandContext(ctx, s.cfg.bin, full...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return out, fmt.Errorf("agent-deck %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return out, nil
}

// adeckJSON runs a CLI command expected to emit JSON and unmarshals it.
func (s *server) adeckJSON(ctx context.Context, v any, args ...string) error {
	out, err := s.adeck(ctx, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("decode JSON from %q: %w", strings.Join(args, " "), err)
	}
	return nil
}

// --- typed views over the stock CLI JSON shapes (validated against v1.9.68) ---

// sessionInfo mirrors `agent-deck list --json` array elements.
type sessionInfo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Path        string `json:"path"`
	Group       string `json:"group"`
	Tool        string `json:"tool"`
	Status      string `json:"status"`
	TmuxSession string `json:"tmux_session"`
	Profile     string `json:"profile"`
	Model       string `json:"model"`
	// LastReply is a short preview of the session's last assistant message,
	// read from the transcript (reliable regardless of tmux/registry state).
	// Populated best-effort by handleSessions. The PWA is detail-first: it shows
	// this instead of agent-deck's unreliable status.
	LastReply string `json:"lastReply,omitempty"`

	// Live activity, parsed best-effort from the pane (handleSessions).
	Working     bool   `json:"working,omitempty"`     // agent is actively processing
	Activity    string `json:"activity,omitempty"`    // the thinking line, e.g. "Channelling… (1m 12s · ↓ 2.1k tokens)"
	CurrentTool string `json:"currentTool,omitempty"` // in-progress tool, e.g. "Bash(…)" / "Task(…)" (Task = subagent)
}

// replyOutput mirrors `agent-deck session output <id> --json`.
type replyOutput struct {
	ClaudeSessionID string `json:"claude_session_id"`
	Content         string `json:"content"`
	Role            string `json:"role"`
	Timestamp       string `json:"timestamp"`
}

// listSessions returns sessions via the agent-deck CLI (`list --json`). This is
// CLI-first by design: deck-remote does NOT require a long-running agent-deck
// web server (running one as a second writer caused registry churn). We only use
// title/group/tool here; the (possibly stale) status field is ignored — the PWA
// is detail-first and shows the real last reply instead.
func (s *server) listSessions(ctx context.Context) ([]sessionInfo, error) {
	var out []sessionInfo
	if err := s.adeckJSON(ctx, &out, "list", "--json"); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *server) sessionReply(ctx context.Context, id string) (replyOutput, error) {
	var out replyOutput
	err := s.adeckJSON(ctx, &out, "session", "output", id, "--json")
	return out, err
}

// sessionPane returns the raw tmux pane text (ANSI-stripped) for a session,
// used to confirm a permission dialog is on screen before approving.
func (s *server) sessionPane(ctx context.Context, id string) (string, error) {
	out, err := s.adeck(ctx, "session", "output", id, "--pane")
	return string(out), err
}

// sendNoWait injects a prompt/message without blocking for the reply.
func (s *server) sendNoWait(ctx context.Context, id, message string) error {
	_, err := s.adeck(ctx, "session", "send", id, message, "--no-wait")
	return err
}

// sendKeyText / sendKeyEnter deliver raw keystrokes (for guarded approve).
func (s *server) sendKeyText(ctx context.Context, id, text string) error {
	_, err := s.adeck(ctx, "session", "send-keys", id, "--text", text)
	return err
}

func (s *server) sendKeyEnter(ctx context.Context, id string) error {
	_, err := s.adeck(ctx, "session", "send-keys", id, "--enter")
	return err
}

// findSession resolves an id-or-title to a sessionInfo via the list.
func (s *server) findSession(ctx context.Context, idOrTitle string) (sessionInfo, error) {
	sessions, err := s.listSessions(ctx)
	if err != nil {
		return sessionInfo{}, err
	}
	for _, se := range sessions {
		if se.ID == idOrTitle || se.Title == idOrTitle {
			return se, nil
		}
	}
	return sessionInfo{}, fmt.Errorf("session %q not found", idOrTitle)
}

// cliCtx returns a context with a sane default timeout for quick CLI calls.
func cliCtx(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
