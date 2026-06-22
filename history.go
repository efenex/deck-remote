package main

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	Role    string `json:"role"` // "user" | "reply"
	Content string `json:"content"`
	Ts      int64  `json:"ts"` // unix seconds, 0 if unknown
}

type jsonlLine struct {
	Type      string `json:"type"`
	IsMeta    bool   `json:"isMeta"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
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
func findTranscript(claudeSessionID, projectPath string) string {
	if claudeSessionID == "" {
		return ""
	}
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

// parseTranscript reads the whole transcript into ordered conversation messages
// (oldest first). Cheap enough to run per request for typical transcripts.
func parseTranscript(path string) ([]histMsg, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var msgs []histMsg
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024) // transcripts have long lines
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l jsonlLine
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		if l.IsMeta {
			continue
		}
		switch l.Type {
		case "assistant":
			if txt := assistantText(l.Message.Content); txt != "" {
				msgs = append(msgs, histMsg{Role: "reply", Content: txt, Ts: tsUnix(l.Timestamp)})
			}
		case "user":
			if txt := humanText(l.Message.Content); txt != "" {
				msgs = append(msgs, histMsg{Role: "user", Content: txt, Ts: tsUnix(l.Timestamp)})
			}
		}
	}
	return msgs, sc.Err()
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
	if se.Tool != "claude" {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []histMsg{}, "hasMore": false})
		return
	}
	// Resolve the claude session id (session output --json carries it).
	out, err := s.sessionReply(ctx, se.ID)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	path := findTranscript(out.ClaudeSessionID, se.Path)
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]any{"messages": []histMsg{}, "hasMore": false})
		return
	}
	all, err := parseTranscript(path)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
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
		"messages": all[start:end],
		"hasMore":  start > 0,
		"total":    len(all),
	})
}
