package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Pi (github.com/earendil-works/pi-mono) and its fork omp. agent-deck launches
// both with `--session-dir ~/.<pi|omp>/agent-deck/<instance-id> --continue`, so
// the session's transcript is the newest *.jsonl in that directory (/new starts
// another file there). Entries form a tree via id/parentId; the active branch
// is the chain from the last appended entry back to the root.
//
// Pi has no permission prompts, so there is no attention/approval surface. The
// live TUI is driven with keystrokes: Enter sends (or steers, while busy),
// Alt+Enter queues a follow-up, Escape aborts the turn.

var piSlashCapabilities = []slashCapability{
	{Command: "/compact", Description: "condense the conversation"},
	{Command: "/new", Description: "start a new session"},
	{Command: "/session", Description: "show session info"},
}

type piAdapter struct {
	s    *server
	root string // ".pi" or ".omp"
}

func (a piAdapter) Capabilities(context.Context, sessionInfo) sessionCapabilities {
	return sessionCapabilities{
		Structured: true, History: true, Replies: true, Activity: true, Ask: true,
		Slash: true, Queue: true, Steer: true, Interrupt: true,
		SlashCommands: piSlashCapabilities,
	}
}

func (a piAdapter) sessionDir(se sessionInfo) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, a.root, "agent-deck", se.ID)
}

// load returns the parsed transcript snapshot (no live pane input applied).
func (a piAdapter) load(se sessionInfo) (harnessSnapshot, piTail, error) {
	dir := a.sessionDir(se)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return harnessSnapshot{}, piTail{}, fmt.Errorf("pi session directory not found (%s)", dir)
	}
	path, err := newestJSONL(dir)
	if err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	if path == "" {
		// Launched, never prompted: a valid, empty session.
		return harnessSnapshot{State: "idle"}, piTail{}, nil
	}
	return a.s.pi.load(path)
}

func newestJSONL(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var best string
	var bestT time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if best == "" || info.ModTime().After(bestT) || (info.ModTime().Equal(bestT) && e.Name() > filepath.Base(best)) {
			best, bestT = filepath.Join(dir, e.Name()), info.ModTime()
		}
	}
	return best, nil
}

func (a piAdapter) Snapshot(ctx context.Context, se sessionInfo, pane string, paneOK bool) (harnessSnapshot, error) {
	snap, tail, err := a.load(se)
	if err != nil {
		return harnessSnapshot{State: "idle", DegradedReason: err.Error()}, nil
	}
	snap.Activity = a.activity(se, snap, tail, pane, paneOK)
	if snap.Activity.Working {
		snap.State = "working"
	}
	return snap, nil
}

// activity decides working/idle. The live pane is the truth when we have it
// (pi writes an assistant message only once it completes, and compaction or
// retries never touch the transcript); otherwise the watcher's recent pane
// read, otherwise the transcript tail with a staleness cutoff so a killed pi
// does not read as working forever.
func (a piAdapter) activity(se sessionInfo, snap harnessSnapshot, tail piTail, pane string, paneOK bool) activityInfo {
	return a.activityAt(se, snap, tail, pane, paneOK, time.Now())
}

func (a piAdapter) activityAt(se sessionInfo, snap harnessSnapshot, tail piTail, pane string, paneOK bool, now time.Time) activityInfo {
	info := activityInfo{CurrentTool: tail.CurrentTool}
	switch {
	case paneOK:
		info.Working = piPaneWorking(pane)
		info.stallKeySet = true
		info.stallKey = piPaneProgressKey(pane)
	default:
		// Trust the cache only while the last real pane observation is recent:
		// updatedAt moves on every watcher sweep (including sweeps that replay
		// this very cache), so it cannot be the freshness clock.
		if cached, ok := a.s.acts.get(se.ID); ok && now.Sub(cached.observedAt) < 3*watchInterval {
			info.Working, info.Stalled = cached.Working, cached.Stalled
			info.replayed = true
		} else {
			info.Working = tail.Working && now.Sub(time.Unix(snap.LastActivity, 0)) < 10*time.Minute
		}
	}
	if !info.Working {
		info.CurrentTool = ""
		return info
	}
	info.Activity = "Working…"
	return info
}

func (a piAdapter) History(ctx context.Context, se sessionInfo) ([]histMsg, error) {
	snap, _, err := a.load(se)
	if err != nil {
		return nil, err
	}
	return snap.Messages, nil
}

func (a piAdapter) Attention(context.Context, sessionInfo, string, bool) (attentionInfo, error) {
	return attentionInfo{}, nil
}

func (a piAdapter) Deliver(ctx context.Context, se sessionInfo, text, delivery, pane string, paneOK bool) (deliveryResult, error) {
	if !paneOK {
		return deliveryResult{}, fmt.Errorf("could not verify the live pi editor")
	}
	draft, ok := piEditorDraft(pane)
	if !ok {
		return deliveryResult{}, fmt.Errorf("delivery rejected: the pi editor is not on screen (a menu or dialog may be open)")
	}
	if draft != "" {
		return deliveryResult{}, fmt.Errorf("delivery rejected: the live pi editor contains a desktop draft")
	}
	busy := piPaneWorking(pane)
	mode := delivery
	if mode == "" || mode == "auto" {
		mode = "queue"
		if !busy {
			mode = "send"
		}
	}
	if !busy {
		mode = "send" // nothing to queue behind or steer
	}
	if mode != "queue" && mode != "steer" && mode != "send" {
		return deliveryResult{}, fmt.Errorf("delivery must be auto, queue, or steer")
	}
	if err := a.typeText(ctx, se, text); err != nil {
		return deliveryResult{}, err
	}
	if mode == "queue" {
		// Alt+Enter: follow-up, delivered after pi finishes all current work.
		if err := a.s.sendNamedKey(ctx, se.ID, "M-Enter"); err != nil {
			return deliveryResult{}, err
		}
		return deliveryResult{State: "queued"}, nil
	}
	// Enter: a new turn when idle; a steering message (delivered after the
	// current tool calls) when busy.
	if err := a.s.sendKeyEnter(ctx, se.ID); err != nil {
		return deliveryResult{}, err
	}
	if busy {
		return deliveryResult{State: "steered"}, nil
	}
	return deliveryResult{State: "sent"}, nil
}

// typeText types text into the pi editor. A literal newline would submit, so
// lines are separated with Ctrl+J (pi's other "insert new line" binding; the
// documented Shift+Enter needs tmux extended keys).
func (a piAdapter) typeText(ctx context.Context, se sessionInfo, text string) error {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			if err := a.s.sendNamedKey(ctx, se.ID, "C-j"); err != nil {
				return err
			}
		}
		if line == "" {
			continue
		}
		if err := a.s.sendKeyText(ctx, se.ID, line); err != nil {
			return err
		}
	}
	return nil
}

func (a piAdapter) Interrupt(ctx context.Context, se sessionInfo, pane string, paneOK bool) (bool, error) {
	if !paneOK {
		return false, fmt.Errorf("could not verify the live pi pane")
	}
	if !piPaneWorking(pane) {
		return false, nil
	}
	before, _, _ := a.load(se)
	if err := a.s.sendNamedKey(ctx, se.ID, "Escape"); err != nil {
		return false, err
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		next, _, err := a.load(se)
		if err == nil && next.Sequence > before.Sequence && next.State == "interrupted" {
			return true, nil
		}
		if p, err := a.s.paneFor(ctx, se); err == nil && !piPaneWorking(p) {
			return true, nil
		}
	}
	return false, nil
}

func (a piAdapter) RespondAttention(context.Context, sessionInfo, attentionResponseRequest) (attentionResponseResult, error) {
	return attentionResponseResult{}, fmt.Errorf("pi has no permission prompts")
}

// --- pane parsing ---

// piEditorBorders finds pi's editor box: the last line made only of '─' (its
// bottom border) and, above it, the nearest line starting with "──" (its top
// border, which carries the "⠙ Working" status while busy). ok is false when
// the editor is not on screen (selectors and dialogs replace it).
func piEditorBorders(lines []string) (top, bottom int, ok bool) {
	bottom = -1
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-12; i-- {
		t := strings.TrimSpace(lines[i])
		if t != "" && strings.Trim(t, "─") == "" && len([]rune(t)) >= 20 {
			bottom = i
			break
		}
	}
	if bottom < 0 {
		return 0, 0, false
	}
	for i := bottom - 1; i >= 0 && i >= bottom-30; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "──") {
			return i, bottom, true
		}
	}
	return 0, 0, false
}

func piPaneLines(pane string) []string {
	return strings.Split(strings.TrimRight(stripANSI(pane), "\n"), "\n")
}

// piEditorDraft returns the text typed into the editor ("" when empty).
func piEditorDraft(pane string) (string, bool) {
	lines := piPaneLines(pane)
	top, bottom, ok := piEditorBorders(lines)
	if !ok {
		return "", false
	}
	var parts []string
	for _, l := range lines[top+1 : bottom] {
		if t := strings.TrimSpace(l); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n"), true
}

func piPaneWorking(pane string) bool {
	lines := piPaneLines(pane)
	top, _, ok := piEditorBorders(lines)
	if !ok {
		return false
	}
	return strings.Contains(lines[top], "Working")
}

// piPaneProgressKey identifies visible progress: the streamed text above the
// editor. Unchanged across polls while working means a frozen turn.
func piPaneProgressKey(pane string) string {
	lines := piPaneLines(pane)
	top, _, ok := piEditorBorders(lines)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%x", hashStr(strings.Join(lines[:top], "\n")))
}

// --- transcript parsing ---

type piTail struct {
	Working     bool   // the active branch ends mid-turn
	CurrentTool string // the tool call awaiting its result, if any
}

type piCacheEntry struct {
	size  int64
	mtime time.Time
	snap  harnessSnapshot
	tail  piTail
}

type piTranscriptStore struct {
	mu sync.Mutex
	m  map[string]piCacheEntry
}

func newPiTranscriptStore() *piTranscriptStore {
	return &piTranscriptStore{m: map[string]piCacheEntry{}}
}

// maxPiTranscriptBytes bounds a single parse; pi transcripts carry tool output
// and images, but a session past this is far beyond what a phone renders.
const maxPiTranscriptBytes = 64 << 20

func (st *piTranscriptStore) load(path string) (harnessSnapshot, piTail, error) {
	info, err := os.Stat(path)
	if err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if e, ok := st.m[path]; ok && e.size == info.Size() && e.mtime.Equal(info.ModTime()) {
		return cloneHarnessSnapshot(e.snap), e.tail, nil
	}
	if info.Size() > maxPiTranscriptBytes {
		return harnessSnapshot{}, piTail{}, fmt.Errorf("pi transcript too large (%d bytes)", info.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	defer f.Close()
	snap, tail, err := parsePiTranscript(f)
	if err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	if len(st.m) > 64 {
		st.m = map[string]piCacheEntry{}
	}
	st.m[path] = piCacheEntry{size: info.Size(), mtime: info.ModTime(), snap: snap, tail: tail}
	return cloneHarnessSnapshot(snap), tail, nil
}

type piEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Version   int             `json:"version"`
	Message   json.RawMessage `json:"message"`
}

type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Timestamp    int64           `json:"timestamp"` // unix ms
	ToolCallID   string          `json:"toolCallId"`
}

type piBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func parsePiTranscript(r interface{ Read([]byte) (int, error) }) (harnessSnapshot, piTail, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), maxPiTranscriptBytes)
	byID := map[string]piEntry{}
	seqByID := map[string]int64{}
	var last string
	var fileSeq int64
	sawHeader := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e piEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // a torn final line while pi is writing
		}
		if e.Type == "session" {
			sawHeader = true
			continue
		}
		if e.ID == "" {
			continue
		}
		byID[e.ID] = e
		last = e.ID
		fileSeq = nextPiSeq(fileSeq, e.Timestamp)
		seqByID[e.ID] = fileSeq
	}
	if err := sc.Err(); err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	if !sawHeader {
		return harnessSnapshot{}, piTail{}, fmt.Errorf("not a pi session transcript (no session header)")
	}

	// Active branch: leaf → root, then reversed.
	var path []piEntry
	seen := map[string]bool{}
	for id := last; id != "" && !seen[id]; {
		e, ok := byID[id]
		if !ok {
			break
		}
		seen[id] = true
		path = append(path, e)
		if e.ParentID == nil {
			break
		}
		id = *e.ParentID
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	snap := harnessSnapshot{State: "idle"}
	var tail piTail
	turnID := ""
	lastRole, lastStop := "", ""
	pendingTool := map[string]string{}
	var toolOrder []string
	for _, e := range path {
		ts := tsUnix(e.Timestamp)
		if ts > 0 {
			snap.LastActivity = ts
		}
		seq := seqByID[e.ID]
		if e.Type != "message" || len(e.Message) == 0 {
			continue
		}
		var m piMessage
		if json.Unmarshal(e.Message, &m) != nil {
			continue
		}
		if m.Timestamp > 0 {
			ts = m.Timestamp / 1000
		}
		switch m.Role {
		case "user":
			turnID = e.ID
			pendingTool, toolOrder = map[string]string{}, nil
			images := 0
			for _, b := range piBlocks(m.Content) {
				if b.Type == "image" {
					images++
				}
			}
			if text := piText(m.Content); text != "" || images > 0 {
				snap.Messages = append(snap.Messages, histMsg{Role: "user", Content: text, Ts: ts, TurnID: turnID, Seq: seq, ID: e.ID, Images: images})
			}
		case "assistant":
			text := piText(m.Content)
			for _, b := range piBlocks(m.Content) {
				if b.Type == "toolCall" {
					pendingTool[b.ID] = piToolLabel(b)
					toolOrder = append(toolOrder, b.ID)
				}
			}
			if m.StopReason == "error" && m.ErrorMessage != "" && text == "" {
				text = "⚠ " + m.ErrorMessage
			}
			if text != "" {
				role := "reply"
				if m.StopReason == "toolUse" {
					role = "commentary"
				}
				snap.Messages = append(snap.Messages, histMsg{Role: role, Content: text, Ts: ts, TurnID: turnID, Seq: seq, ID: e.ID})
				if role == "reply" {
					snap.LastReply = text
				}
			}
		case "toolResult":
			delete(pendingTool, m.ToolCallID)
		default:
			continue
		}
		lastRole, lastStop = m.Role, m.StopReason
	}
	snap.Sequence = fileSeq

	switch {
	case lastRole == "user" || lastRole == "toolResult" || (lastRole == "assistant" && lastStop == "toolUse"):
		tail.Working = true
		for _, id := range toolOrder {
			if label, ok := pendingTool[id]; ok {
				tail.CurrentTool = label
			}
		}
	case lastRole == "assistant" && lastStop == "aborted":
		snap.State = "interrupted"
	case lastRole == "assistant":
		snap.State = "completed"
	}
	return snap, tail, nil
}

// nextPiSeq is the correlation key of the next entry appended to a file: its
// timestamp in unix ms, forced strictly above the previous entry's. Entries are
// only ever appended, so file order is monotonic across /tree branch switches
// (where the position on the active branch shrinks or jumps), and the
// timestamp floor keeps it increasing across /new, which starts another file.
func nextPiSeq(prev int64, timestamp string) int64 {
	next := prev + 1
	if t, err := time.Parse(time.RFC3339Nano, timestamp); err == nil && t.UnixMilli() > next {
		next = t.UnixMilli()
	}
	return next
}

func piBlocks(raw json.RawMessage) []piBlock {
	var blocks []piBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	return nil
}

// piText joins a message's text content (a string, or text blocks; thinking,
// images and tool calls are left out).
func piText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []string
	for _, b := range piBlocks(raw) {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}

func piToolLabel(b piBlock) string {
	for _, k := range []string{"command", "path", "file_path", "pattern", "url"} {
		if v, ok := b.Arguments[k].(string); ok && v != "" {
			return fmt.Sprintf("%s(%s)", b.Name, preview(v, 60))
		}
	}
	return b.Name
}
