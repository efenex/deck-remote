package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// queryInt reads a clamped integer query param.
func queryInt(r *http.Request, key string, def, lo, hi int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < lo {
		n = lo
	}
	if n > hi {
		n = hi
	}
	return n
}

// Conversation history from the Claude transcript JSONL. The transcript holds
// the FULL back-and-forth (not just the last reply the deck shows), so this lets
// the PWA populate the thread with prior messages and scroll back through them.
//
// We parse the transcript directly (located by the session's claude UUID) and
// keep only genuine conversation: assistant text blocks, and human user
// messages — filtering out the synthetic user entries Claude Code injects
// (command wrappers, tool results, system reminders, meta).

type histMsg struct {
	Role    string `json:"role"` // "user" | "reply" | "commentary"
	Content string `json:"content"`
	Ts      int64  `json:"ts"` // unix seconds, 0 if unknown
	// Role "bash" (operator's `!` shell line): Content is the command; Stdout and
	// Stderr are the captured output when the output entry was merged in.
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	TurnID string `json:"-"`
	Seq    int64  `json:"-"`

	// recall rows/v2-shaped fields (session-store design §2, slice F0); all
	// additive, the pre-F0 client ignores them.
	//   ID     native row id: Claude line uuid, pi entry id, opencode message id,
	//          `queue:<ts>:<hash>` for a prompt typed while the agent worked (the
	//          attachment carries the absorb time, recall keys on the enqueue
	//          time, so this is convention-compatible, not byte-identical).
	//          Empty where the harness gives no id in hand (codex).
	//   Kind   user | assistant | bash. Both reply and commentary are
	//          "assistant"; Role still tells them apart.
	//   Queued typed while the agent was working (Claude queued_command).
	//   Origin human | peer:<from-id> on user rows, empty on the rest.
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Queued bool   `json:"queued,omitempty"`
	Origin string `json:"origin,omitempty"`
	// Sender is a peer row's display name when the envelope names one; the
	// client otherwise resolves the id from its session list.
	Sender string `json:"sender,omitempty"`
}

var peerPrefixRE = regexp.MustCompile(`^\s*\[agent-deck from:([^\]\s]*)\]`)

// annotateRows returns a copy of msgs with Kind and Origin filled in from Role
// and content, so every adapter gets them without touching its own parser.
func annotateRows(msgs []histMsg) []histMsg {
	out := make([]histMsg, len(msgs))
	for i, m := range msgs {
		if m.Kind == "" {
			switch m.Role {
			case "user", "bash":
				m.Kind = m.Role
			case "reply", "commentary":
				m.Kind = "assistant"
			}
		}
		if m.Origin == "" && m.Role == "user" {
			m.Origin = "human"
			if pm := peerPrefixRE.FindStringSubmatch(m.Content); pm != nil {
				m.Origin = "peer:" + pm[1]
			}
		}
		out[i] = m
	}
	return out
}

type jsonlLine struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	IsMeta    bool   `json:"isMeta"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// Attachment is set on type "attachment" lines. Only queued_command is read:
	// what the operator typed while Claude was working (no user line is written
	// for it). Peer and task-notification origins are not human input.
	Attachment struct {
		Type        string `json:"type"`
		Prompt      string `json:"prompt"`
		CommandMode string `json:"commandMode"`
		Timestamp   string `json:"timestamp"`
		Origin      struct {
			Kind string `json:"kind"`
		} `json:"origin"`
	} `json:"attachment"`
}

// queuedID is the row id for a queued_command: recall's `queue:<ts>:<hash>`
// convention, hash = first 12 hex chars of sha256(trimmed prompt).
func queuedID(ts, prompt string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(prompt)))
	return fmt.Sprintf("queue:%s:%x", ts, h[:6])
}

// syntheticUserPrefixes mark user entries that are not human messages. These are
// strings the harness injects as user turns (command wrappers, system reminders,
// and background-task completions) — not something the human typed, so they must
// not render in the thread.
var syntheticUserPrefixes = []string{
	"<local-command", "<command-name", "<command-message", "<command-args",
	"<command-stdout", "<system-reminder", "caveat:", "[request interrupted",
	"<task-notification", "<task-prompt",
}

var (
	bashInputRE  = regexp.MustCompile(`(?s)^\s*<bash-input>(.*)</bash-input>\s*$`)
	bashStdoutRE = regexp.MustCompile(`(?s)<bash-stdout>(.*)</bash-stdout>`)
	bashStderrRE = regexp.MustCompile(`(?s)<bash-stderr>(.*)</bash-stderr>`)
)

// bashEntry recognises the user entries Claude Code writes for a `!` shell line:
// "<bash-input>cmd</bash-input>", then (as its own entry)
// "<bash-stdout>…</bash-stdout><bash-stderr>…</bash-stderr>". It returns the
// command (isInput) or the captured output; ok=false for anything else.
func bashEntry(raw json.RawMessage) (cmd, stdout, stderr string, isInput, ok bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	if m := bashInputRE.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1]), "", "", true, true
	}
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<bash-stdout>") && !strings.HasPrefix(t, "<bash-stderr>") {
		return
	}
	if m := bashStdoutRE.FindStringSubmatch(t); m != nil {
		stdout = strings.Trim(m[1], "\r\n")
	}
	if m := bashStderrRE.FindStringSubmatch(t); m != nil {
		stderr = strings.Trim(m[1], "\r\n")
	}
	return "", stdout, stderr, false, true
}

// addBash folds a bash-mode entry into msgs: an input starts a "bash" message;
// an output fills the immediately preceding input (or stands alone, Content "",
// when the input was parsed in an earlier incremental pass). Empty output with
// no input to attach to is dropped.
func addBash(msgs []histMsg, cmd, stdout, stderr string, isInput bool, ts int64, id string) []histMsg {
	if isInput {
		return append(msgs, histMsg{Role: "bash", Content: cmd, Ts: ts, ID: id})
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == "bash" && msgs[n-1].Content != "" && msgs[n-1].Stdout == "" && msgs[n-1].Stderr == "" {
		msgs[n-1].Stdout, msgs[n-1].Stderr = stdout, stderr
		return msgs
	}
	if stdout == "" && stderr == "" {
		return msgs
	}
	return append(msgs, histMsg{Role: "bash", Stdout: stdout, Stderr: stderr, Ts: ts, ID: id})
}

// queuedHumanMsg turns an attachment.queued_command into a row: what the
// operator typed while Claude was working (user, or `!` bash, or a slash
// command), or a peer's message sent meanwhile. Task-notifications and other
// origins stay out of the thread. The prompt goes through the same gates as an
// idle-typed one.
func queuedHumanMsg(l jsonlLine) (histMsg, bool) {
	a := l.Attachment
	if a.Type != "queued_command" || (a.CommandMode != "" && a.CommandMode != "prompt") {
		return histMsg{}, false
	}
	ts := a.Timestamp
	if ts == "" {
		ts = l.Timestamp
	}
	raw, _ := json.Marshal(a.Prompt)
	id := queuedID(ts, a.Prompt)
	if a.Origin.Kind != "human" {
		if from, name, text, ok := parsePeer(strings.TrimSpace(a.Prompt)); ok && text != "" {
			return peerMsg(from, name, text, tsUnix(ts), id, true), true
		}
		return histMsg{}, false
	}
	if cmd, _, _, isIn, ok := bashEntry(raw); ok && isIn {
		return histMsg{Role: "bash", Content: cmd, Ts: tsUnix(ts), ID: id, Queued: true, Origin: "human"}, true
	}
	m, ok := userEntryRow(raw, tsUnix(ts), id, true)
	if ok && m.Role == "user" {
		m.Origin = "human"
	}
	return m, ok
}

var reminderBlockRE = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// convertToClaudeDirName mirrors agent-deck/Claude: non-alphanumerics -> "-".
var nonAlnumRE = regexp.MustCompile(`[^a-zA-Z0-9]`)

func convertToClaudeDirName(p string) string { return nonAlnumRE.ReplaceAllString(p, "-") }

// findTranscript locates <claudeSessionID>.jsonl. Tries the computed path first
// (configDir/projects/<encoded projectPath>/<id>.jsonl), then falls back to a
// walk under projects/ (robust to path-encoding / per-config-dir differences).
// findTranscript is findTranscriptUncached behind a small cache: the PWA asks
// for history every few seconds while a sheet is open, and a transcript that
// isn't at the expected path costs a walk of all of ~/.claude/projects. Hits
// are kept while the file exists; misses are retried after transcriptMissTTL.
func findTranscript(claudeSessionID, projectPath string) string {
	if claudeSessionID == "" {
		return ""
	}
	key := claudeSessionID + "\x00" + projectPath
	now := time.Now()
	transcripts.mu.Lock()
	e, ok := transcripts.paths[key]
	transcripts.mu.Unlock()
	if ok {
		if e.path == "" && now.Sub(e.at) < transcriptMissTTL {
			return ""
		}
		if e.path != "" {
			if _, err := os.Stat(e.path); err == nil {
				return e.path
			}
		}
	}
	path := findTranscriptUncached(claudeSessionID, projectPath)
	transcripts.mu.Lock()
	if len(transcripts.paths) >= transcriptCacheMax {
		transcripts.paths = map[string]pathEntry{}
	}
	transcripts.paths[key] = pathEntry{path: path, at: now}
	transcripts.mu.Unlock()
	return path
}

const (
	transcriptMissTTL  = 30 * time.Second
	transcriptCacheMax = 256
)

type pathEntry struct {
	path string
	at   time.Time
}

type parsedEntry struct {
	size  int64
	mtime time.Time
	off   int64 // bytes consumed: the end of the last complete line (transcripts are append-only)
	msgs  []histMsg
}

var transcripts = struct {
	mu     sync.Mutex
	paths  map[string]pathEntry
	parsed map[string]parsedEntry
	parses int // full parses done, for tests
}{paths: map[string]pathEntry{}, parsed: map[string]parsedEntry{}}

func findTranscriptUncached(claudeSessionID, projectPath string) string {
	cfg := claudeConfigDir()
	projects := filepath.Join(cfg, "projects")
	if projectPath != "" {
		rp := projectPath
		if r, err := filepath.EvalSymlinks(projectPath); err == nil {
			rp = r
		}
		cand := filepath.Join(projects, convertToClaudeDirName(rp), claudeSessionID+".jsonl")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	target := claudeSessionID + ".jsonl"
	var found string
	_ = filepath.WalkDir(projects, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if !d.IsDir() && d.Name() == target {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func tsUnix(s string) int64 {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}

// parseTranscript returns the transcript's messages (oldest first), re-reading
// the file only when its size or mtime changed since the last parse. Callers
// get their own copy of the slice.
func parseTranscript(path string) ([]histMsg, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	transcripts.mu.Lock()
	e, ok := transcripts.parsed[path]
	transcripts.mu.Unlock()
	if ok && e.size == fi.Size() && e.mtime.Equal(fi.ModTime()) {
		return append([]histMsg(nil), e.msgs...), nil
	}
	// A live session only ever appends: parse just the new tail and extend the
	// cached messages instead of re-reading a (potentially 100 MB) transcript on
	// every turn. A shrunk file was rewritten: start over.
	var base []histMsg
	var from int64
	if ok && fi.Size() > e.size && e.off > 0 && e.off <= fi.Size() {
		base, from = e.msgs, e.off
	}
	add, off, err := parseTranscriptFrom(path, from)
	if err != nil {
		return nil, err
	}
	msgs := append(append([]histMsg(nil), base...), add...)
	transcripts.mu.Lock()
	transcripts.parses++
	if len(transcripts.parsed) >= transcriptCacheMax/8 {
		transcripts.parsed = map[string]parsedEntry{}
	}
	transcripts.parsed[path] = parsedEntry{size: fi.Size(), mtime: fi.ModTime(), off: off, msgs: msgs}
	transcripts.mu.Unlock()
	return append([]histMsg(nil), msgs...), nil
}

// parseTranscriptFile reads the whole transcript into ordered conversation
// messages (oldest first).
func parseTranscriptFile(path string) ([]histMsg, error) {
	msgs, _, err := parseTranscriptFrom(path, 0)
	return msgs, err
}

var (
	contentStrA = []byte(`"content":"`)
	contentStrB = []byte(`"content": "`)
	typeUserA   = []byte(`"type":"user"`)
	typeUserB   = []byte(`"type": "user"`)
	typeAttachA = []byte(`"type":"attachment"`)
	typeAttachB = []byte(`"type": "attachment"`)
	queuedCmd   = []byte(`"queued_command"`)
)

// skipLine is a cheap pre-filter for the bulk of a transcript: a user entry whose
// content is not a plain string is a tool_result / attachment list, which
// humanText rejects anyway, so don't pay to JSON-decode it.
func skipLine(line []byte) bool {
	// Attachments (hook output, skill listings, up to hundreds of KB each) are
	// ignored unless they are a queued_command.
	if bytes.Contains(line, typeAttachA) || bytes.Contains(line, typeAttachB) {
		return !bytes.Contains(line, queuedCmd)
	}
	if !bytes.Contains(line, typeUserA) && !bytes.Contains(line, typeUserB) {
		return false
	}
	return !bytes.Contains(line, contentStrA) && !bytes.Contains(line, contentStrB)
}

// parseTranscriptFrom parses complete lines starting at byte offset from and
// returns the messages found plus the offset just past the last complete line
// (a trailing partial line, still being written, is left for the next call).
func parseTranscriptFrom(path string, from int64) ([]histMsg, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, from, err
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return nil, from, err
		}
	}
	var msgs []histMsg
	off := from
	rd := bufio.NewReaderSize(f, 1024*1024) // transcripts have long lines
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return msgs, off, err
		}
		if len(line) == 0 {
			break
		}
		if line[len(line)-1] != '\n' && !json.Valid(bytes.TrimSpace(line)) {
			break // a partial last line still being written: leave it for the next call
		}
		off += int64(len(line))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || skipLine(line) {
			continue
		}
		var l jsonlLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		if l.IsMeta {
			// Claude Code files some peer messages as meta; nothing else meta is shown.
			if l.Type == "user" {
				var s string
				if json.Unmarshal(l.Message.Content, &s) == nil {
					if from, name, text, ok := parsePeer(strings.TrimSpace(s)); ok && text != "" {
						msgs = append(msgs, peerMsg(from, name, text, tsUnix(l.Timestamp), l.UUID, false))
					}
				}
			}
			continue
		}
		switch l.Type {
		case "assistant":
			if txt := assistantText(l.Message.Content); txt != "" {
				msgs = append(msgs, histMsg{Role: "reply", Content: txt, Ts: tsUnix(l.Timestamp), ID: l.UUID})
			}
		case "user":
			if cmd, so, se, isIn, ok := bashEntry(l.Message.Content); ok {
				msgs = addBash(msgs, cmd, so, se, isIn, tsUnix(l.Timestamp), l.UUID)
				break
			}
			if m, ok := userEntryRow(l.Message.Content, tsUnix(l.Timestamp), l.UUID, false); ok {
				msgs = append(msgs, m)
			}
		case "attachment":
			if m, ok := queuedHumanMsg(l); ok {
				msgs = append(msgs, m)
			}
		}
		if err == io.EOF {
			break
		}
	}
	return msgs, off, nil
}

// assistantText joins the text blocks of an assistant content array (skips
// thinking / tool_use). Accepts either a JSON array of blocks or a single block
// object, so the same logic serves both the transcript parser and the
// reply-content sanitizer.
func assistantText(raw json.RawMessage) string {
	type block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		// Not an array — try a single block object.
		var one block
		if json.Unmarshal(raw, &one) != nil {
			return ""
		}
		blocks = []block{one}
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// cleanReplyContent sanitizes a reply-content string before it is hashed,
// pushed, or sent to clients. `agent-deck session output --json` can return raw
// structured content (a serialized tool_use block or a literal JSON array of
// content blocks) that would otherwise render verbatim as JSON in the thread.
//
// If the trimmed string is an Anthropic content-block payload (a typed-block
// array, or a single typed block object), we extract the assistant text blocks
// (same logic as the transcript parser) and return the joined text, or "" when
// there is no text (e.g. a tool_use-only turn). Anything else — plain text,
// malformed JSON, AND valid JSON the model itself produced (`[1,2,3]`,
// `{"foo":"bar"}`) — is returned unchanged, so a legitimate JSON answer is
// never silently blanked.
func cleanReplyContent(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	if t[0] != '{' && t[0] != '[' {
		return s
	}
	if !json.Valid([]byte(t)) {
		return s // malformed JSON: leave as-is rather than discard content
	}
	if !looksLikeContentBlocks(t) {
		return s // valid JSON, but a genuine model answer — keep verbatim
	}
	return assistantText(json.RawMessage(t))
}

// looksLikeContentBlocks reports whether the JSON is an Anthropic content-block
// payload: an array of typed blocks, or a single typed block object. Only these
// are safe to reduce to text (and to blank when text-less); a bare JSON document
// the model actually produced is NOT block-shaped and must be left untouched.
func looksLikeContentBlocks(t string) bool {
	raw := json.RawMessage(t)
	if t[0] == '[' {
		var arr []map[string]json.RawMessage
		if json.Unmarshal(raw, &arr) != nil || len(arr) == 0 {
			return false
		}
		for _, el := range arr {
			if _, ok := el["type"]; !ok {
				return false
			}
		}
		return true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return false
	}
	_, ok := obj["type"]
	return ok
}

// humanText returns the human message text for a user entry, or "" if the entry
// is synthetic (command wrapper, tool result list, system reminder only, meta).
func humanText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "" // list content = tool_result/attachments, not a human message
	}
	s = strings.TrimSpace(reminderBlockRE.ReplaceAllString(s, ""))
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	low := strings.ToLower(s)
	for _, p := range syntheticUserPrefixes {
		if strings.HasPrefix(low, p) {
			return ""
		}
	}
	return s
}

// GET /api/rc/history?id=<id>&limit=N&offset=M — conversation history (oldest
// first) for a window ending `offset` messages from the newest. hasMore=true
// when older messages exist before the window (for scroll-up paging).
func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing id")
		return
	}
	limit := queryInt(r, "limit", 25, 1, 200)
	offset := queryInt(r, "offset", 0, 0, 100000)

	ctx, cancel := cliCtx(withProfile(r.Context(), reqProfile(r)), 12*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if !structuredHarness(se.Tool) {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []histMsg{}, "hasMore": false})
		return
	}
	var all []histMsg
	hasOlder, fromRecall := false, false
	if ca, isClaude := s.adapterFor(se).(claudeAdapter); isClaude {
		// recall rows/v2 when agent-deck serves them; otherwise the transcript
		// parser answers.
		all, hasOlder, fromRecall = ca.historyWindow(ctx, se, offset+limit)
	}
	if !fromRecall {
		all, err = s.adapterFor(se).History(ctx, se)
		if err != nil {
			httpError(w, http.StatusNotImplemented, err.Error())
			return
		}
	}
	// Window: the `limit` messages ending `offset` from the newest.
	end := len(all) - offset
	if end < 0 {
		end = 0
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": annotateRows(all[start:end]),
		"hasMore":  start > 0 || hasOlder,
		"total":    len(all),
	})
}
