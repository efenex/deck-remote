package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// profileKey scopes a per-request agent-deck profile override on the context.
// The daemon starts with one default cfg.profile, but the CLI-first /api/rc/*
// surface lets each request target a different profile via ?profile=<name> (GET)
// or {profile} (POST). The override is read/write-correct because every adeck()
// invocation is an independent stock-CLI process scoped by the global -p flag;
// nothing in the daemon is bound to one profile for the CLI path.
type profileKey struct{}

// withProfile attaches a profile override to ctx (no-op for the empty string).
func withProfile(ctx context.Context, p string) context.Context {
	if p == "" {
		return ctx
	}
	return context.WithValue(ctx, profileKey{}, p)
}

// profileFrom returns the context's profile override, or def when unset/empty.
func profileFrom(ctx context.Context, def string) string {
	if v, ok := ctx.Value(profileKey{}).(string); ok && v != "" {
		return v
	}
	return def
}

// reqProfile extracts the ?profile= override from a request (trimmed).
func reqProfile(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("profile"))
}

// adeckArgs assembles the agent-deck argv: the global -p <profile> flag (before
// the subcommand) followed by args. Pure helper so the arg ordering is testable
// without exec.
func adeckArgs(profile string, args ...string) []string {
	return append([]string{"-p", profile}, args...)
}

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
// Each invocation is a full Go-binary startup + registry load, so hot polling
// paths must NOT come through here (see adeckweb.go / pane.go / replies.go);
// the exec counter makes any regression visible in the periodic watcher log.
func (s *server) adeck(ctx context.Context, args ...string) ([]byte, error) {
	s.execAdeck.Add(1)
	full := adeckArgs(profileFrom(ctx, s.cfg.profile), args...)
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
	// Optional upstream metadata. Older agent-deck releases omit these fields;
	// the Codex resolver then falls back to the live tmux environment and the
	// hook .sid sidecar (in that order).
	CodexSessionID    string `json:"codex_session_id,omitempty"`
	ResolvedCodexHome string `json:"resolved_codex_home,omitempty"`
	// Populated by the agent-deck web list (adeckweb.go); best-effort from the
	// CLI list. TmuxSocket/SSHHost never reach the PWA wire (internal use).
	ClaudeSessionID string `json:"claude_session_id,omitempty"`
	TmuxSocket      string `json:"-"`
	SSHHost         string `json:"-"`
	// LastReply is a short preview of the session's last assistant message,
	// read from the transcript (reliable regardless of tmux/registry state).
	// Populated best-effort by handleSessions. The PWA is detail-first: it shows
	// this instead of agent-deck's unreliable status.
	LastReply string `json:"lastReply,omitempty"`
	// LastActivity is the unix time (s) of the last assistant reply, from the
	// transcript timestamp. Used by the PWA to sort groups by recency and to
	// fold stale (>1d) groups. 0 when unknown.
	LastActivity int64 `json:"lastActivity,omitempty"`

	// Live activity, parsed best-effort from the pane (handleSessions).
	Working        bool                `json:"working,omitempty"`     // agent is actively processing
	Activity       string              `json:"activity,omitempty"`    // the thinking line, e.g. "Channelling… (1m 12s · ↓ 2.1k tokens)"
	CurrentTool    string              `json:"currentTool,omitempty"` // in-progress tool, e.g. "Bash(…)" / "Task(…)" (Task = subagent)
	Stalled        bool                `json:"stalled,omitempty"`     // working, but the spinner has been frozen across polls (stall detection)
	State          string              `json:"state,omitempty"`       // idle | working | completed | interrupted
	NeedsAttention bool                `json:"needsAttention,omitempty"`
	Capabilities   sessionCapabilities `json:"capabilities"`
	DegradedReason string              `json:"degradedReason,omitempty"`
}

// profileEntry / profilesResp mirror `agent-deck profile list --json`. The
// profile subcommand is global (profile-independent); the prepended -p flag is
// benign and does not filter the returned set (verified against v1.9.68).
type profileEntry struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

type profilesResp struct {
	Profiles       []profileEntry `json:"profiles"`
	DefaultProfile string         `json:"default_profile"`
}

// listProfiles returns the available agent-deck profiles via the stock CLI.
func (s *server) listProfiles(ctx context.Context) (profilesResp, error) {
	var p profilesResp
	err := s.adeckJSON(ctx, &p, "profile", "list", "--json")
	return p, err
}

// replyOutput mirrors `agent-deck session output <id> --json`.
type replyOutput struct {
	ClaudeSessionID string `json:"claude_session_id"`
	Content         string `json:"content"`
	Role            string `json:"role"`
	Timestamp       string `json:"timestamp"`
}

// cliListSessions returns sessions via the agent-deck CLI (`list --json`) — the
// fallback when the agent-deck web server is unreachable or serves a different
// profile (listSessions in adeckweb.go is the front door). We only use
// title/group/tool here; the (possibly stale) status field is ignored — the PWA
// is detail-first and shows the real last reply instead.
func (s *server) cliListSessions(ctx context.Context) ([]sessionInfo, error) {
	raw, err := s.adeck(ctx, "list", "--json")
	if err != nil {
		return nil, err
	}
	// An empty profile prints "No sessions found in profile '<p>'." even under
	// --json (exit 0, through at least agent-deck v1.16.10). That is an empty
	// list, not a decode error; agent-deck's own remote client reads it the
	// same way (internal/session/ssh.go parseRemoteSessions).
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("No sessions found")) {
			return []sessionInfo{}, nil
		}
	}
	var out []sessionInfo
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, fmt.Errorf("decode JSON from %q: %w", "list --json", err)
	}
	return out, nil
}

func (s *server) sessionReply(ctx context.Context, id string) (replyOutput, error) {
	var out replyOutput
	err := s.adeckJSON(ctx, &out, "session", "output", id, "--json")
	return out, err
}

// sessionPane returns the raw tmux pane text (callers strip ANSI as needed) for
// a session, used to confirm a permission dialog is on screen before approving.
// Resolves the session via the cached list and captures with tmux directly
// (paneFor); the CLI exec survives only as cliSessionPane, the fallback.
func (s *server) sessionPane(ctx context.Context, id string) (string, error) {
	se, err := s.findSession(ctx, id)
	if err != nil {
		return s.cliSessionPane(ctx, id)
	}
	return s.paneFor(ctx, se)
}

// cliSessionPane is the legacy exec path: `agent-deck session output --pane
// --primary`. Fallback only — a full agent-deck startup per capture is the
// polling burn the tmux-direct path exists to avoid.
func (s *server) cliSessionPane(ctx context.Context, id string) (string, error) {
	out, err := s.adeck(ctx, "session", "output", id, "--pane", "--primary")
	return string(out), err
}

// --- `session send` delivery contract (agent-deck v1.11.0+, issue #1793) ---

// sendDeliveryPayload is the JSON `session send --json` emits. As of v1.11.0
// only `delivery: submitted` exits 0; every other delivery state exits 1 with
// code delivery_failed. The payload lands on STDOUT for BOTH outcomes (the CLI
// prints its error object through the same JSON writer), so we parse it
// regardless of exit status. Older agent-deck releases omit delivery/submitted
// — the zero values then leave classification to the exit status alone.
type sendDeliveryPayload struct {
	Success            bool   `json:"success"`
	Error              string `json:"error"`
	Code               string `json:"code"`
	Delivery           string `json:"delivery"`
	Submitted          bool   `json:"submitted"`
	SavedDraft         string `json:"saved_draft"`
	DraftRestoreFailed bool   `json:"draft_restore_failed"`
}

// sendError is a failed `session send`, classified by agent-deck's delivery
// state. The distinction the PWA needs is "never landed" vs "landed but the
// submit was never confirmed" — the latter is worth offering a resend for, but
// must NOT be retried automatically: the text may already be sitting in the
// composer, and a silent resend would double-submit the operator's message.
type sendError struct {
	Delivery   string
	Retryable  bool
	SavedDraft string
	msg        string
}

func (e *sendError) Error() string { return e.msg }

// retryableDelivery reports whether re-sending the same body is worth
// offering. line_too_long is non-retryable by construction (nothing was typed
// and the same body fails identically); send_failed means the dispatch itself
// broke, which a bare retry does not fix.
func retryableDelivery(delivery string) bool {
	switch delivery {
	case "typed", "typed_not_submitted", "no_evidence":
		return true
	default:
		return false
	}
}

// sendErrorFields exposes a send failure's delivery classification for the hub
// events the PWA consumes. Non-send errors yield no extra fields.
func sendErrorFields(err error) map[string]any {
	var se *sendError
	if !errors.As(err, &se) {
		return nil
	}
	f := map[string]any{"retryable": se.Retryable}
	if se.Delivery != "" {
		f["delivery"] = se.Delivery
	}
	if se.SavedDraft != "" {
		f["savedDraft"] = se.SavedDraft
	}
	return f
}

// adeckSend runs `session send` in --json mode and classifies the outcome.
// Returns any bytes trailing the JSON object — on the --wait path agent-deck
// prints the JSON status first and then the raw reply body, so the decoder's
// input offset is exactly the boundary between them.
func (s *server) adeckSend(ctx context.Context, args ...string) (string, error) {
	out, execErr := s.adeck(ctx, append(args, "--json")...)

	var p sendDeliveryPayload
	dec := json.NewDecoder(bytes.NewReader(out))
	trailing := ""
	if decErr := dec.Decode(&p); decErr == nil {
		trailing = string(out[dec.InputOffset():])
	} else if execErr == nil {
		// No parseable payload but the CLI succeeded: an agent-deck too old
		// for the --json status object. Treat the whole stream as the body.
		return strings.TrimSpace(string(out)), nil
	}

	if execErr != nil {
		msg := p.Error
		if msg == "" {
			msg = execErr.Error()
		}
		return "", &sendError{
			Delivery:   p.Delivery,
			Retryable:  retryableDelivery(p.Delivery),
			SavedDraft: p.SavedDraft,
			msg:        msg,
		}
	}

	// Delivered, but an operator draft was cleared to make room and could not
	// be typed back. agent-deck only warns about this on stderr, which we
	// discard on success — surface it or the draft is silently lost.
	if p.DraftRestoreFailed {
		log.Printf("send: operator draft cleared and NOT restored; recover from %s", p.SavedDraft)
	}
	return strings.TrimSpace(trailing), nil
}

// sendNoWait injects a prompt/message without blocking for the reply.
func (s *server) sendNoWait(ctx context.Context, id, message string) error {
	_, err := s.adeckSend(ctx, "session", "send", id, message, "--no-wait")
	return err
}

// sendKeyText / sendKeyEnter deliver raw keystrokes (for guarded approve).
func (s *server) sendKeyText(ctx context.Context, id, text string) error {
	_, err := s.adeck(ctx, "session", "send-keys", id, "--text", text, "--primary")
	return err
}

func (s *server) sendKeyEnter(ctx context.Context, id string) error {
	_, err := s.adeck(ctx, "session", "send-keys", id, "--enter", "--primary")
	return err
}

func (s *server) sendNamedKey(ctx context.Context, id, key string) error {
	_, err := s.adeck(ctx, "session", "send-keys", id, "--named-key", key, "--primary")
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
