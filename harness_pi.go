package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	if snap.HistoryTruncated {
		return append([]histMsg{{Role: "commentary", Content: "Earlier history truncated: this pi session is too large to load whole."}}, snap.Messages...), nil
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
	size   int64
	mtime  time.Time
	parser *piParser
	snap   harnessSnapshot
	tail   piTail
}

type piTranscriptStore struct {
	mu sync.Mutex
	m  map[string]piCacheEntry
}

func newPiTranscriptStore() *piTranscriptStore {
	return &piTranscriptStore{m: map[string]piCacheEntry{}}
}

// maxPiTranscriptBytes bounds what a first read parses. A larger file is read
// from its header plus the last piTailBytes only, and the snapshot is flagged
// HistoryTruncated; growth after that is parsed incrementally from the last
// offset, so a long session never blocks ask and never re-parses from zero.
const (
	maxPiTranscriptBytes = 64 << 20
	piTailBytes          = 16 << 20
	maxPiLineBytes       = 32 << 20 // one entry (an image-laden tool result); longer lines are skipped
)

func (st *piTranscriptStore) load(path string) (harnessSnapshot, piTail, error) {
	info, err := os.Stat(path)
	if err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	prev, cached := st.m[path]
	if cached && prev.size == info.Size() && prev.mtime.Equal(info.ModTime()) {
		return cloneHarnessSnapshot(prev.snap), prev.tail, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	defer f.Close()

	var p *piParser
	if cached && prev.parser.offset <= info.Size() {
		p = prev.parser // append-only growth: parse only the new bytes
		if _, err := f.Seek(p.offset, io.SeekStart); err != nil {
			return harnessSnapshot{}, piTail{}, err
		}
		if err := p.feed(f); err != nil {
			return harnessSnapshot{}, piTail{}, err
		}
	} else {
		p = newPiParser()
		if info.Size() > maxPiTranscriptBytes {
			if err := p.feedTail(f, info.Size()); err != nil {
				return harnessSnapshot{}, piTail{}, err
			}
		} else if err := p.feed(f); err != nil {
			return harnessSnapshot{}, piTail{}, err
		}
	}
	snap, tail, err := p.build()
	if err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	if len(st.m) > 64 {
		st.m = map[string]piCacheEntry{}
	}
	st.m[path] = piCacheEntry{size: info.Size(), mtime: info.ModTime(), parser: p, snap: snap, tail: tail}
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

// piNode is what the parser keeps per entry: the tree links plus the few
// message facts the snapshot needs. Tool output and image data are dropped at
// ingest, so a parser that outlives a read holds text, not the transcript.
type piNode struct {
	id, parent string // parent "" at a root
	rootless   bool   // parentId was an explicit null
	ts         int64  // entry timestamp, unix s
	seq        int64
	isMessage  bool

	role, stop, errMsg, toolCallID string
	msgTs                          int64 // message timestamp, unix s
	text                           string
	images                         int
	calls                          []piToolCall
}

type piToolCall struct{ id, label string }

// piParser accumulates a pi transcript across reads. offset is the number of
// bytes consumed, always ending on a line boundary, so the next read resumes
// there.
type piParser struct {
	nodes     map[string]*piNode
	last      string
	fileSeq   int64
	sawHeader bool
	truncated bool
	offset    int64
}

func newPiParser() *piParser { return &piParser{nodes: map[string]*piNode{}} }

func parsePiTranscript(r io.Reader) (harnessSnapshot, piTail, error) {
	p := newPiParser()
	if err := p.feed(r); err != nil {
		return harnessSnapshot{}, piTail{}, err
	}
	return p.build()
}

// feedTail reads the header line, then only the final piTailBytes of a file too
// large to parse whole, starting at a line boundary.
func (p *piParser) feedTail(f *os.File, size int64) error {
	hdr := bufio.NewReader(f)
	line, _, _, err := readPiLine(hdr)
	if err != nil && err != io.EOF {
		return err
	}
	p.ingest(line)
	start := size - piTailBytes
	if _, err := f.Seek(start-1, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	_, skipped, _, err := readPiLine(br) // the remainder of the line start-1 falls in (or a lone "\n")
	if err != nil && err != io.EOF {
		return err
	}
	p.truncated = true
	p.offset = start - 1 + int64(skipped)
	return p.feedReader(br)
}

func (p *piParser) feed(r io.Reader) error {
	return p.feedReader(bufio.NewReaderSize(r, 1<<20))
}

func (p *piParser) feedReader(br *bufio.Reader) error {
	for {
		line, n, terminated, err := readPiLine(br)
		if n > 0 {
			// A complete line is consumed even when it is not JSON; an
			// unterminated one only once it parses (else pi is mid-write).
			if p.ingest(line) || terminated {
				p.offset += int64(n)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readPiLine returns one line without its '\n', the number of bytes consumed
// from the reader, and whether the line ended in '\n'. A line over
// maxPiLineBytes is consumed but returned empty.
func readPiLine(br *bufio.Reader) (line []byte, consumed int, terminated bool, err error) {
	var buf []byte
	over := false
	for {
		chunk, e := br.ReadSlice('\n')
		consumed += len(chunk)
		if !over {
			if len(buf)+len(chunk) > maxPiLineBytes {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		err = e
		break
	}
	terminated = consumed > 0 && err == nil
	if over {
		return nil, consumed, terminated, err
	}
	return bytes.TrimSuffix(buf, []byte("\n")), consumed, terminated, err
}

// ingest parses one line; it reports whether the line was a complete JSON
// value (a torn final line while pi is writing is not, and is not consumed).
func (p *piParser) ingest(line []byte) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	var e piEntry
	if err := json.Unmarshal(line, &e); err != nil {
		return false
	}
	if e.Type == "session" {
		p.sawHeader = true
		return true
	}
	if e.ID == "" {
		return true
	}
	p.fileSeq = nextPiSeq(p.fileSeq, e.Timestamp)
	n := &piNode{id: e.ID, ts: tsUnix(e.Timestamp), seq: p.fileSeq}
	if e.ParentID != nil {
		n.parent = *e.ParentID
	} else {
		n.rootless = true
	}
	if e.Type == "message" && len(e.Message) > 0 {
		var m piMessage
		if json.Unmarshal(e.Message, &m) == nil {
			n.isMessage = true
			n.role, n.stop, n.errMsg, n.toolCallID = m.Role, m.StopReason, m.ErrorMessage, m.ToolCallID
			if m.Timestamp > 0 {
				n.msgTs = m.Timestamp / 1000
			}
			if m.Role == "user" || m.Role == "assistant" {
				n.text = piText(m.Content)
				for _, b := range piBlocks(m.Content) {
					switch b.Type {
					case "image":
						n.images++
					case "toolCall":
						n.calls = append(n.calls, piToolCall{b.ID, piToolLabel(b)})
					}
				}
			}
		}
	}
	p.nodes[e.ID] = n
	p.last = e.ID
	return true
}

// build derives the snapshot from the accumulated nodes.
func (p *piParser) build() (harnessSnapshot, piTail, error) {
	if !p.sawHeader {
		return harnessSnapshot{}, piTail{}, fmt.Errorf("not a pi session transcript (no session header)")
	}
	// Active branch: leaf → root, then reversed. A parent cut off by tail
	// truncation ends the walk early.
	var path []*piNode
	seen := map[string]bool{}
	for id := p.last; id != "" && !seen[id]; {
		n, ok := p.nodes[id]
		if !ok {
			break
		}
		seen[id] = true
		path = append(path, n)
		if n.rootless {
			break
		}
		id = n.parent
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	snap := harnessSnapshot{State: "idle", HistoryTruncated: p.truncated}
	var tail piTail
	turnID := ""
	lastRole, lastStop := "", ""
	pendingTool := map[string]string{}
	var toolOrder []string
	for _, e := range path {
		if e.ts > 0 {
			snap.LastActivity = e.ts
		}
		if !e.isMessage {
			continue
		}
		ts := e.ts
		if e.msgTs > 0 {
			ts = e.msgTs
		}
		seq := e.seq
		switch e.role {
		case "user":
			turnID = e.id
			pendingTool, toolOrder = map[string]string{}, nil
			if e.text != "" || e.images > 0 {
				snap.Messages = append(snap.Messages, histMsg{Role: "user", Content: e.text, Ts: ts, TurnID: turnID, Seq: seq, ID: e.id, Images: e.images})
			}
		case "assistant":
			text := e.text
			for _, c := range e.calls {
				pendingTool[c.id] = c.label
				toolOrder = append(toolOrder, c.id)
			}
			if e.stop == "error" && e.errMsg != "" && text == "" {
				text = "⚠ " + e.errMsg
			}
			if text != "" {
				role := "reply"
				if e.stop == "toolUse" {
					role = "commentary"
				}
				snap.Messages = append(snap.Messages, histMsg{Role: role, Content: text, Ts: ts, TurnID: turnID, Seq: seq, ID: e.id})
				if role == "reply" {
					snap.LastReply = text
				}
			}
		case "toolResult":
			delete(pendingTool, e.toolCallID)
		default:
			continue
		}
		lastRole, lastStop = e.role, e.stop
	}
	snap.Sequence = p.fileSeq

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
