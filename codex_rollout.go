package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var codexUUIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type codexResolution struct {
	SessionID string
	Home      string
	Paths     []string
	Version   string
	Source    string
}

type codexRolloutStore struct {
	mu       sync.Mutex
	sessions map[string]*codexParseState
}

type codexParseState struct {
	paths          []string
	offsets        map[string]int64
	tails          map[string][]byte
	checkpoints    map[string][]byte
	snapshot       harnessSnapshot
	working        bool
	state          string
	turnID         string
	lastComment    string
	toolCalls      map[string]codexToolCall
	attention      *attentionInfo
	seq            int64
	version        string
	supported      bool
	messageIDs     map[string]bool
	messageMirrors map[string]map[string]int
}

type codexToolCall struct {
	ID   string
	Name string
	Seq  int64
}

type codexEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func newCodexRolloutStore() *codexRolloutStore {
	return &codexRolloutStore{sessions: map[string]*codexParseState{}}
}

func newCodexParseState(paths []string) *codexParseState {
	return &codexParseState{
		paths:          append([]string(nil), paths...),
		offsets:        map[string]int64{},
		tails:          map[string][]byte{},
		checkpoints:    map[string][]byte{},
		state:          "idle",
		toolCalls:      map[string]codexToolCall{},
		messageIDs:     map[string]bool{},
		messageMirrors: map[string]map[string]int{},
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func isPrefixStrings(prefix, all []string) bool {
	return len(prefix) <= len(all) && sameStrings(prefix, all[:len(prefix)])
}

// Load incrementally consumes complete JSONL records. A newly appended rollout
// segment continues the same state; truncation, replacement, or reordering
// deterministically rebuilds from the validated segments. An incomplete final
// record is retained until the next read.
func (s *codexRolloutStore) Load(res codexResolution) (harnessSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.sessions[res.SessionID]
	if st == nil || (!sameStrings(st.paths, res.Paths) && !isPrefixStrings(st.paths, res.Paths)) {
		st = newCodexParseState(res.Paths)
		s.sessions[res.SessionID] = st
	} else if !sameStrings(st.paths, res.Paths) {
		st.paths = append([]string(nil), res.Paths...)
	}

	reset := false
	for _, path := range st.paths {
		fi, err := os.Stat(path)
		if err != nil {
			return harnessSnapshot{}, err
		}
		if fi.Size() < st.offsets[path] {
			reset = true
			break
		}
		if st.offsets[path] > 0 && len(st.checkpoints[path]) > 0 {
			f, openErr := os.Open(path)
			if openErr != nil {
				return harnessSnapshot{}, openErr
			}
			got := make([]byte, len(st.checkpoints[path]))
			_, readErr := f.ReadAt(got, st.offsets[path]-int64(len(got)))
			_ = f.Close()
			if readErr != nil || !bytes.Equal(got, st.checkpoints[path]) {
				reset = true
				break
			}
		}
	}
	if reset {
		st = newCodexParseState(res.Paths)
		s.sessions[res.SessionID] = st
	}

	for _, path := range st.paths {
		if err := st.readPath(path); err != nil {
			return harnessSnapshot{}, err
		}
	}
	st.project()
	return cloneHarnessSnapshot(st.snapshot), nil
}

func cloneHarnessSnapshot(in harnessSnapshot) harnessSnapshot {
	out := in
	out.Messages = append([]histMsg(nil), in.Messages...)
	if in.Attention.Questions != nil {
		out.Attention.Questions = append([]attentionQuestion(nil), in.Attention.Questions...)
	}
	return out
}

func (st *codexParseState) readPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(st.offsets[path], io.SeekStart); err != nil {
		return err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	st.offsets[path] += int64(len(b))
	checkpointLen := int64(64)
	if st.offsets[path] < checkpointLen {
		checkpointLen = st.offsets[path]
	}
	if checkpointLen > 0 {
		checkpoint := make([]byte, checkpointLen)
		if _, err := f.ReadAt(checkpoint, st.offsets[path]-checkpointLen); err == nil {
			st.checkpoints[path] = checkpoint
		}
	}
	buf := append(append([]byte(nil), st.tails[path]...), b...)
	lastNL := strings.LastIndexByte(string(buf), '\n')
	if lastNL < 0 {
		st.tails[path] = buf
		return nil
	}
	complete := buf[:lastNL]
	st.tails[path] = append([]byte(nil), buf[lastNL+1:]...)
	for _, line := range strings.Split(string(complete), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env codexEnvelope
		if json.Unmarshal([]byte(line), &env) != nil {
			continue // malformed complete records are isolated; the stream continues
		}
		st.apply(env)
	}
	return nil
}

func (st *codexParseState) apply(env codexEnvelope) {
	st.seq++
	switch env.Type {
	case "session_meta":
		var p struct {
			Version string `json:"cli_version"`
		}
		if json.Unmarshal(env.Payload, &p) == nil {
			st.version = p.Version
			st.supported = codexVersionSupported(p.Version)
		}
	case "event_msg":
		st.applyEvent(env)
	case "response_item":
		st.applyResponseItem(env)
	}
}

func (st *codexParseState) applyEvent(env codexEnvelope) {
	var p struct {
		Type             string           `json:"type"`
		TurnID           string           `json:"turn_id"`
		Message          string           `json:"message"`
		Phase            string           `json:"phase"`
		Item             codexMessageItem `json:"item"`
		LastAgentMessage string           `json:"last_agent_message"`
		// user_message attachments (empty on every local rollout so far).
		Images      []json.RawMessage `json:"images"`
		LocalImages []json.RawMessage `json:"local_images"`
	}
	if json.Unmarshal(env.Payload, &p) != nil {
		return
	}
	switch p.Type {
	case "task_started", "turn_started":
		st.working, st.state, st.turnID = true, "working", p.TurnID
		st.lastComment = ""
		st.toolCalls = map[string]codexToolCall{}
		st.attention = nil
	case "task_complete", "turn_complete":
		if p.LastAgentMessage != "" {
			st.addMessage(histMsg{Role: "reply", Content: p.LastAgentMessage, Ts: tsUnix(env.Timestamp), TurnID: firstNonempty(p.TurnID, st.turnID)}, "task_complete")
		}
		st.working, st.state = false, "completed"
		st.toolCalls = map[string]codexToolCall{}
		st.attention = nil
	case "turn_aborted", "task_aborted":
		st.working, st.state = false, "interrupted"
		st.toolCalls = map[string]codexToolCall{}
		st.attention = nil
	case "user_message":
		st.addMessage(histMsg{Role: "user", Content: p.Message, Ts: tsUnix(env.Timestamp), TurnID: st.turnID, Images: len(p.Images) + len(p.LocalImages)}, "event_msg")
	case "agent_message":
		role := codexMessageRole("assistant", p.Phase)
		if role != "" {
			st.addMessage(histMsg{Role: role, Content: p.Message, Ts: tsUnix(env.Timestamp), TurnID: st.turnID}, "event_msg")
		}
	case "item_completed":
		switch p.Item.Type {
		case "UserMessage":
			p.Item.Role = "user"
		case "AgentMessage":
			p.Item.Role = "assistant"
		default:
			return // reasoning, token usage and tool telemetry are not conversation text
		}
		st.addItemMessage(p.Item, env.Timestamp, firstNonempty(p.TurnID, st.turnID), "item_completed")
	}

}

func (st *codexParseState) applyResponseItem(env codexEnvelope) {
	raw := env.Payload
	var msg codexMessageItem
	if json.Unmarshal(raw, &msg) == nil {
		if msg.Type == "message" {
			st.addItemMessage(msg, env.Timestamp, firstNonempty(msg.Metadata.TurnID, st.turnID), "response_item")
			return
		}
		if msg.Type == "agent_message" {
			text, _ := codexContent(msg.Content, nil)
			st.addMessage(histMsg{ID: msg.ID, Role: "peer", Content: text, Ts: tsUnix(env.Timestamp), TurnID: firstNonempty(msg.Metadata.TurnID, st.turnID), Origin: "peer:" + msg.Author, Sender: msg.Author}, "response_item")
			return
		}
	}
	var p struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		ID        string          `json:"id"`
		Arguments string          `json:"arguments"`
		Input     json.RawMessage `json:"input"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	switch p.Type {
	case "function_call", "custom_tool_call":
		id := p.CallID
		if id == "" {
			id = p.ID
		}
		if id == "" {
			return
		}
		st.toolCalls[id] = codexToolCall{ID: id, Name: p.Name, Seq: st.seq}
		if p.Name == "request_user_input" {
			args := []byte(p.Arguments)
			if len(args) == 0 || string(args) == "null" {
				args = p.Input
				if len(args) > 0 && args[0] == '"' {
					var decoded string
					if json.Unmarshal(args, &decoded) == nil {
						args = []byte(decoded)
					}
				}
			}
			if a, ok := parseCodexQuestionCall(id, args); ok {
				st.attention = &a
			}
		}
	case "function_call_output", "custom_tool_call_output":
		delete(st.toolCalls, p.CallID)
		if st.attention != nil && st.attention.ID == p.CallID {
			st.attention = nil
		}
	}
}

func (st *codexParseState) project() {
	st.snapshot.State = st.state
	st.snapshot.Sequence = st.seq
	st.snapshot.Activity = activityInfo{Working: st.working}
	if st.working {
		st.snapshot.Activity.Activity = st.lastComment
		var latest codexToolCall
		for _, call := range st.toolCalls {
			if call.Seq > latest.Seq {
				latest = call
			}
		}
		if latest.Name != "" && latest.Name != "request_user_input" {
			st.snapshot.Activity.CurrentTool = latest.Name
		}
	}
	st.snapshot.DegradedReason = ""
	st.snapshot.Attention = attentionInfo{}
	if st.attention != nil && st.working {
		st.snapshot.Attention = *st.attention
	}
	if !st.supported {
		if st.version == "" {
			st.snapshot.DegradedReason = "Codex rollout metadata is incomplete"
		} else {
			st.snapshot.DegradedReason = "Unverified Codex rollout version " + st.version + "; showing recognized messages read-only. Update deck-remote for structured controls."
		}
	}
}

// Stable patch releases in these explicitly validated minor families only. A
// newer CLI may append modern records to a resumed 0.143/0.144 header, so record
// parsing is shape-based; the version gate controls interactive capabilities.
var codexKnownVersionRE = regexp.MustCompile(`^0\.(143|144|164)\.(0|[1-9][0-9]*)$`)

func codexVersionSupported(v string) bool { return codexKnownVersionRE.MatchString(v) }

type codexContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type codexMessageItem struct {
	Type     string             `json:"type"`
	ID       string             `json:"id"`
	Role     string             `json:"role"`
	Phase    string             `json:"phase"`
	Author   string             `json:"author"`
	Content  []codexContentPart `json:"content"`
	Metadata struct {
		TurnID string   `json:"turn_id"`
		Kinds  []string `json:"content_item_kinds"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

func codexMessageRole(role, phase string) string {
	if role == "user" {
		return "user"
	}
	if role != "assistant" {
		return ""
	}
	switch phase {
	case "final_answer":
		return "reply"
	case "commentary":
		return "commentary"
	}
	return "" // never mistake unknown phases or developer instructions for a reply
}

func codexContent(parts []codexContentPart, kinds []string) (string, int) {
	var text []string
	images := 0
	for i, p := range parts {
		// 0.164 user messages can mix operator text with AGENTS/environment input.
		if len(kinds) > 0 && (i >= len(kinds) || kinds[i] != "user.text") {
			continue
		}
		switch p.Type {
		case "text", "Text", "input_text", "output_text":
			text = append(text, p.Text)
		case "image", "Image", "input_image":
			images++
		}
	}
	return strings.Join(text, "\n"), images
}

func (st *codexParseState) addItemMessage(p codexMessageItem, timestamp, turnID, source string) {
	role := codexMessageRole(p.Role, p.Phase)
	if role == "" {
		return
	}
	var kinds []string
	if p.Role == "user" {
		kinds = p.Metadata.Kinds
	}
	text, images := codexContent(p.Content, kinds)
	st.addMessage(histMsg{ID: p.ID, Role: role, Content: text, Images: images, Ts: tsUnix(timestamp), TurnID: turnID}, source)
}

// The same message appears in response_item and item_completed (and older
// event_msg). IDs deduplicate native mirrors; per-source occurrence counts also
// pair user mirrors with different IDs without collapsing repeated user text.
func (st *codexParseState) addMessage(m histMsg, source string) {
	m.Content = strings.TrimSpace(m.Content)
	if m.Content == "" && m.Images == 0 {
		return
	}
	if m.ID != "" {
		if st.messageIDs[m.ID] {
			return
		}
		st.messageIDs[m.ID] = true
	}
	key := m.TurnID + "\x00" + m.Role + "\x00" + fmt.Sprint(m.Images) + "\x00" + m.Content
	counts := st.messageMirrors[key]
	if counts == nil {
		counts = map[string]int{}
		st.messageMirrors[key] = counts
	}
	max := 0
	for _, count := range counts {
		if count > max {
			max = count
		}
	}
	counts[source]++
	if counts[source] <= max {
		return
	}
	m.Seq = st.seq
	st.snapshot.Messages = append(st.snapshot.Messages, m)
	if m.Role == "reply" {
		st.snapshot.LastReply = m.Content
		st.snapshot.LastActivity = m.Ts
	}
	if m.Role == "commentary" {
		st.lastComment = preview(m.Content, 180)
	}
}

func parseCodexQuestionCall(id string, raw []byte) (attentionInfo, bool) {
	var req struct {
		Questions []attentionQuestion `json:"questions"`
	}
	if json.Unmarshal(raw, &req) != nil || len(req.Questions) == 0 || len(req.Questions) > 3 {
		return attentionInfo{}, false
	}
	seen := map[string]bool{}
	for i := range req.Questions {
		q := &req.Questions[i]
		q.Header = strings.TrimSpace(q.Header)
		q.ID = strings.TrimSpace(q.ID)
		q.Question = strings.TrimSpace(q.Question)
		if q.Header == "" || q.ID == "" || q.Question == "" || seen[q.ID] {
			return attentionInfo{}, false
		}
		seen[q.ID] = true
	}
	return attentionInfo{Pending: true, Kind: "question", ID: id, Questions: req.Questions}, true
}

func (s *server) resolveCodex(ctx context.Context, se sessionInfo) (codexResolution, error) {
	home := s.cfg.homeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	defaultCodexHome := filepath.Join(home, ".codex")
	var reasons []string
	attempt := func(id, candidateHome, source string) (codexResolution, bool) {
		id = strings.TrimSpace(id)
		if !codexUUIDRE.MatchString(id) {
			reasons = append(reasons, source+" supplied an invalid session id")
			return codexResolution{}, false
		}
		codexHome := cleanCodexHome(candidateHome, home)
		paths, version, err := findValidatedCodexRollouts(codexHome, id)
		if err == nil && len(paths) > 0 {
			return codexResolution{SessionID: id, Home: codexHome, Paths: paths, Version: version, Source: source}, true
		}
		if err != nil {
			reasons = append(reasons, source+": "+err.Error())
		}
		return codexResolution{}, false
	}

	// Prefer additive Agent Deck metadata without touching tmux at all.
	if se.CodexSessionID != "" {
		if res, ok := attempt(se.CodexSessionID, firstNonempty(se.ResolvedCodexHome, defaultCodexHome), "upstream"); ok {
			return res, nil
		}
	}

	tmuxID, tmuxHome := "", ""
	if se.TmuxSession != "" {
		tmuxID, _ = s.tmuxEnvironment(ctx, se.TmuxSession, "CODEX_SESSION_ID")
		tmuxHome, _ = s.tmuxEnvironment(ctx, se.TmuxSession, "CODEX_HOME")
	}
	// If upstream supplied an ID but not a usable home, preserve its ID
	// precedence while retrying under the live tmux CODEX_HOME.
	if se.CodexSessionID != "" && tmuxHome != "" && cleanCodexHome(tmuxHome, home) != cleanCodexHome(firstNonempty(se.ResolvedCodexHome, defaultCodexHome), home) {
		if res, ok := attempt(se.CodexSessionID, tmuxHome, "upstream"); ok {
			return res, nil
		}
	}
	if tmuxID != "" {
		if res, ok := attempt(tmuxID, firstNonempty(tmuxHome, se.ResolvedCodexHome, defaultCodexHome), "tmux"); ok {
			return res, nil
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, ".agent-deck", "hooks", se.ID+".sid")); err == nil {
		if res, ok := attempt(strings.TrimSpace(string(b)), firstNonempty(se.ResolvedCodexHome, tmuxHome, defaultCodexHome), "sidecar"); ok {
			return res, nil
		}
	}
	if len(reasons) == 0 {
		return codexResolution{}, fmt.Errorf("Codex session id unavailable")
	}
	return codexResolution{}, fmt.Errorf("Codex rollout unavailable (%s)", strings.Join(reasons, "; "))
}

func firstNonempty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cleanCodexHome(path, home string) string {
	path = strings.TrimSpace(path)
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(home, path)
	}
	return filepath.Clean(path)
}

func (s *server) tmuxEnvironment(ctx context.Context, target, name string) (string, error) {
	if s.tmux.gone("", target) {
		return "", fs.ErrNotExist // not in the last list-panes: nothing to read
	}
	out, err := s.tmuxExec(ctx, "", "show-environment", "-t", target, name)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(out))
	if strings.HasPrefix(line, "-") {
		return "", fs.ErrNotExist
	}
	prefix := name + "="
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("unexpected tmux environment output")
	}
	return strings.TrimPrefix(line, prefix), nil
}

func findValidatedCodexRollouts(home, id string) ([]string, string, error) {
	if !codexUUIDRE.MatchString(id) {
		return nil, "", fmt.Errorf("invalid Codex session id")
	}
	pattern := filepath.Join(home, "sessions", "*", "*", "*", "rollout-*"+id+"*.jsonl")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, "", err
	}
	var valid []string
	version := ""
	for _, path := range matches {
		metaID, v, err := readCodexRolloutMeta(path)
		if err != nil || metaID != id {
			continue
		}
		if version != "" && v != version {
			return nil, "", fmt.Errorf("rollout segments disagree on Codex version")
		}
		version = v
		valid = append(valid, path)
	}
	if len(valid) == 0 {
		return nil, "", fmt.Errorf("no rollout whose metadata matches %s", id)
	}
	sort.Strings(valid)
	return valid, version, nil
}

func readCodexRolloutMeta(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 4<<20))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		var env codexEnvelope
		if json.Unmarshal(sc.Bytes(), &env) != nil || env.Type != "session_meta" {
			continue
		}
		var p struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
			Version   string `json:"cli_version"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			return "", "", fmt.Errorf("malformed session metadata")
		}
		return firstNonempty(p.SessionID, p.ID), p.Version, nil
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	return "", "", fmt.Errorf("session metadata not found")
}
