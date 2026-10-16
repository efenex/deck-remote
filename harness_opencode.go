package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenCode 2.x. Every TUI is a client of one host-wide background service
// (`opencode service`), whose HTTP API exposes sessions, messages, the
// running set, pending permission requests and forms, prompt delivery
// (steer/queue) and interrupt. The adapter talks to that API only, so it needs
// neither the pane nor keystrokes, and a phone approval clears the TUI dialog
// (the TUI renders service state).
//
// The service publishes {url, password, pid} in
// ~/.local/state/opencode/service.json; auth is HTTP Basic with user
// "opencode". OpenCode 1.x (per-TUI --port servers) is not supported.

var opencodeSlashCapabilities = []slashCapability{
	{Command: "/compact", Description: "condense the conversation"},
}

type opencodeAdapter struct{ s *server }

// --- service client ---

type opencodeService struct {
	URL      string `json:"url"`
	Password string `json:"password"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
}

type opencodeClient struct {
	mu      sync.Mutex
	path    string
	mtime   time.Time
	svc     opencodeService
	http    *http.Client
	binding map[string]ocBinding // agent-deck instance id -> opencode session
}

// ocBinding is a discovered instance → session match. It is re-resolved after
// ocBindingTTL so /new in the TUI (a newer session in the same directory)
// takes over.
type ocBinding struct {
	sessionID string
	at        time.Time
}

const ocBindingTTL = 30 * time.Second

func newOpencodeClient() *opencodeClient {
	home, _ := os.UserHomeDir()
	return &opencodeClient{
		path:    filepath.Join(home, ".local", "state", "opencode", "service.json"),
		http:    &http.Client{Timeout: 5 * time.Second},
		binding: map[string]ocBinding{},
	}
}

func (c *opencodeClient) service() (opencodeService, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, err := os.Stat(c.path)
	if err != nil {
		return opencodeService{}, fmt.Errorf("opencode service not running (no %s)", c.path)
	}
	if !info.ModTime().Equal(c.mtime) || c.svc.URL == "" {
		b, err := os.ReadFile(c.path)
		if err != nil {
			return opencodeService{}, err
		}
		var svc opencodeService
		if err := json.Unmarshal(b, &svc); err != nil || svc.URL == "" {
			return opencodeService{}, fmt.Errorf("unreadable opencode service state: %v", err)
		}
		c.svc, c.mtime = svc, info.ModTime()
	}
	return c.svc, nil
}

func (c *opencodeClient) do(ctx context.Context, method, path string, body, out any) error {
	svc, err := c.service()
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(svc.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", svc.Password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opencode service unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("opencode %s %s: %s %s", method, path, resp.Status, preview(string(data), 200))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode opencode %s: %w", path, err)
	}
	return nil
}

// --- wire shapes (subset of the 2.x OpenAPI) ---

type ocSession struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID"`
	Time     struct {
		Created float64 `json:"created"`
		Updated float64 `json:"updated"`
	} `json:"time"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

type ocMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"` // user | assistant | idle | model-switched | ...
	Time struct {
		Created   float64 `json:"created"`
		Completed float64 `json:"completed"`
	} `json:"time"`
	Text    string `json:"text"`
	Outcome string `json:"outcome"` // idle: succeeded | failed | interrupted
	Finish  string `json:"finish"`  // assistant: stop | tool-calls | error | length
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Content []struct {
		Type  string `json:"type"` // text | tool | reasoning
		Text  string `json:"text"`
		Name  string `json:"name"`
		State struct {
			Status string         `json:"status"`
			Input  map[string]any `json:"input"`
		} `json:"state"`
	} `json:"content"`
}

type ocPermission struct {
	ID        string   `json:"id"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Save      []string `json:"save"`
	Message   string   `json:"message"`
}

type ocFormField struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Hidden      bool   `json:"hidden"`
	Options     []struct {
		Value       string `json:"value"`
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

type ocForm struct {
	ID     string        `json:"id"`
	Title  string        `json:"title"`
	Fields []ocFormField `json:"fields"`
}

// --- session binding ---

// sessionID resolves the opencode session behind an agent-deck instance:
// agent-deck's own binding when it has one, else the newest root session in
// the instance's directory created since the instance was (agent-deck's
// 2.x discovery can lag or miss, e.g. through a /tmp → /private/tmp symlink).
// Sessions agent-deck has bound to other instances are never taken.
func (a opencodeAdapter) sessionID(ctx context.Context, se sessionInfo) (string, error) {
	if se.OpenCodeSessionID != "" {
		return se.OpenCodeSessionID, nil
	}
	c := a.s.oc
	c.mu.Lock()
	b, ok := c.binding[se.ID]
	c.mu.Unlock()
	if ok && time.Since(b.at) < ocBindingTTL {
		return b.sessionID, nil
	}

	dir := se.Path
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	var list struct {
		Data []ocSession `json:"data"`
	}
	q := url.Values{"directory": {dir}, "limit": {"50"}}
	if err := c.do(ctx, http.MethodGet, "/api/session?"+q.Encode(), nil, &list); err != nil {
		return "", err
	}
	taken := map[string]bool{}
	sameDir := 0
	if all, err := a.s.listSessions(ctx); err == nil {
		for _, o := range all {
			if o.Tool != "opencode" || o.ID == se.ID {
				continue
			}
			if o.OpenCodeSessionID != "" {
				taken[o.OpenCodeSessionID] = true
			} else if sameDirectory(o.Path, se.Path) {
				sameDir++
			}
		}
	}
	created := instanceCreated(se.ID)
	var cands []ocSession
	for _, s := range list.Data {
		if s.ParentID != "" || taken[s.ID] || !sameDirectory(s.Location.Directory, dir) {
			continue
		}
		if !created.IsZero() && time.UnixMilli(int64(s.Time.Created)).Before(created.Add(-2*time.Minute)) {
			continue
		}
		cands = append(cands, s)
	}
	if len(cands) == 0 {
		return "", nil // launched, nothing sent yet
	}
	if sameDir > 0 && len(cands) > 1 {
		return "", fmt.Errorf("ambiguous opencode session: %d agent-deck instances share %s", sameDir+1, se.Path)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Time.Updated > cands[j].Time.Updated })
	c.mu.Lock()
	c.binding[se.ID] = ocBinding{sessionID: cands[0].ID, at: time.Now()}
	c.mu.Unlock()
	return cands[0].ID, nil
}

func sameDirectory(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// instanceCreated reads the creation time agent-deck encodes in instance ids
// ("<hex>-<unix seconds>"); zero when the id has another shape.
func instanceCreated(id string) time.Time {
	i := strings.LastIndexByte(id, '-')
	if i < 0 {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(id[i+1:], 10, 64)
	if err != nil || sec < 1e9 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// --- snapshot ---

func (a opencodeAdapter) Capabilities(context.Context, sessionInfo) sessionCapabilities {
	return sessionCapabilities{
		Structured: true, History: true, Replies: true, Activity: true, Ask: true,
		Slash: true, Queue: true, Steer: true, Interrupt: true, Attention: true,
		Approvals: true, Questions: true, SlashCommands: opencodeSlashCapabilities,
	}
}

// maxOpencodeMessages bounds one snapshot's message window (newest first).
const maxOpencodeMessages = 400

func (a opencodeAdapter) messages(ctx context.Context, sid string, max int) ([]ocMessage, error) {
	var out []ocMessage
	cursor := ""
	for len(out) < max {
		// The cursor carries the order; the service rejects both together.
		q := url.Values{"limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		} else {
			q.Set("order", "desc")
		}
		var page struct {
			Data   []ocMessage `json:"data"`
			Cursor struct {
				Next string `json:"next"`
			} `json:"cursor"`
		}
		if err := a.s.oc.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sid)+"/message?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		if len(page.Data) < 200 || page.Cursor.Next == "" || page.Cursor.Next == cursor {
			break
		}
		cursor = page.Cursor.Next
	}
	// Oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (a opencodeAdapter) Snapshot(ctx context.Context, se sessionInfo, _ string, _ bool) (harnessSnapshot, error) {
	snap, err := a.snapshot(ctx, se, maxOpencodeMessages)
	if err != nil {
		return harnessSnapshot{State: "idle", DegradedReason: err.Error()}, nil
	}
	return snap, nil
}

func (a opencodeAdapter) snapshot(ctx context.Context, se sessionInfo, max int) (harnessSnapshot, error) {
	snap := harnessSnapshot{State: "idle"}
	sid, err := a.sessionID(ctx, se)
	if err != nil {
		return snap, err
	}
	if sid == "" {
		return snap, nil
	}
	msgs, err := a.messages(ctx, sid, max)
	if err != nil {
		return snap, err
	}
	projectOpencode(&snap, msgs)

	var active struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := a.s.oc.do(ctx, http.MethodGet, "/api/session/active", nil, &active); err == nil {
		_, snap.Activity.Working = active.Data[sid]
	}
	// No stall detection: without the pane there is no progress signal finer
	// than whole messages, and an empty key never reads as stalled.
	snap.Activity.stallKeySet = true
	if snap.Activity.Working {
		snap.State = "working"
		snap.Activity.Activity = "Working…"
		snap.Activity.CurrentTool = runningOpencodeTool(msgs)
	}
	snap.Attention, _ = a.attention(ctx, sid)
	return snap, nil
}

// projectOpencode maps service messages onto deck-remote's thread model. Seq
// is the message creation time (ms): stable across windows, monotonic, which
// request correlation relies on. TurnID is the user message that opened the
// turn.
func projectOpencode(snap *harnessSnapshot, msgs []ocMessage) {
	turn := ""
	for _, m := range msgs {
		seq := int64(m.Time.Created)
		ts := seq / 1000
		if seq > snap.Sequence {
			snap.Sequence = seq
		}
		if ts > snap.LastActivity {
			snap.LastActivity = ts
		}
		switch m.Type {
		case "user":
			turn = m.ID
			if t := strings.TrimSpace(m.Text); t != "" {
				snap.Messages = append(snap.Messages, histMsg{Role: "user", Content: t, Ts: ts, TurnID: turn, Seq: seq})
			}
		case "assistant":
			var parts []string
			for _, c := range m.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					parts = append(parts, strings.TrimSpace(c.Text))
				}
			}
			text := strings.Join(parts, "\n\n")
			if text == "" && m.Error != nil && m.Error.Type != "aborted" && m.Error.Message != "" {
				text = "⚠ " + m.Error.Message
			}
			if text == "" {
				continue
			}
			role := "reply"
			if m.Finish == "tool-calls" {
				role = "commentary"
			}
			snap.Messages = append(snap.Messages, histMsg{Role: role, Content: text, Ts: ts, TurnID: turn, Seq: seq})
			if role == "reply" {
				snap.LastReply = text
			}
		case "idle":
			switch m.Outcome {
			case "interrupted":
				snap.State = "interrupted"
			default:
				snap.State = "completed"
			}
		}
	}
}

func runningOpencodeTool(msgs []ocMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Type == "user" {
			return ""
		}
		for j := len(m.Content) - 1; j >= 0; j-- {
			c := m.Content[j]
			if c.Type == "tool" && (c.State.Status == "running" || c.State.Status == "streaming") {
				for _, k := range []string{"command", "filePath", "path", "pattern", "url"} {
					if v, ok := c.State.Input[k].(string); ok && v != "" {
						return fmt.Sprintf("%s(%s)", c.Name, preview(v, 60))
					}
				}
				return c.Name
			}
		}
	}
	return ""
}

func (a opencodeAdapter) History(ctx context.Context, se sessionInfo) ([]histMsg, error) {
	snap, err := a.snapshot(ctx, se, 4000)
	if err != nil {
		return nil, err
	}
	return snap.Messages, nil
}

// --- attention: permission requests and forms ---

func (a opencodeAdapter) attention(ctx context.Context, sid string) (attentionInfo, error) {
	var perms struct {
		Data []ocPermission `json:"data"`
	}
	if err := a.s.oc.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sid)+"/permission", nil, &perms); err != nil {
		return attentionInfo{Unavailable: true}, err
	}
	if len(perms.Data) > 0 {
		p := perms.Data[0]
		text := strings.TrimSpace(p.Action + ": " + strings.Join(p.Resources, "\n"))
		if p.Message != "" {
			text = p.Message + "\n" + text
		}
		actions := []string{"approve-once", "decline"}
		if len(p.Save) > 0 {
			actions = []string{"approve-once", "approve-prefix", "decline"}
			text += "\n(always allows: " + strings.Join(p.Save, ", ") + ")"
		}
		return attentionInfo{Pending: true, Kind: "approval", ID: p.ID, Text: text, Actions: actions}, nil
	}
	var forms struct {
		Data []ocForm `json:"data"`
	}
	if err := a.s.oc.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sid)+"/form", nil, &forms); err != nil {
		return attentionInfo{Unavailable: true}, err
	}
	if len(forms.Data) > 0 {
		f := forms.Data[0]
		info := attentionInfo{Pending: true, Kind: "question", ID: f.ID, Text: f.Title}
		for _, fl := range f.Fields {
			if fl.Hidden {
				continue
			}
			q := attentionQuestion{ID: fl.Key, Header: firstNonempty(fl.Title, fl.Key), Question: firstNonempty(fl.Description, fl.Title, fl.Key)}
			for _, o := range fl.Options {
				q.Options = append(q.Options, attentionOption{Label: o.Label, Description: o.Description})
			}
			if fl.Type == "boolean" && len(q.Options) == 0 {
				q.Options = []attentionOption{{Label: "Yes"}, {Label: "No"}}
			}
			info.Questions = append(info.Questions, q)
		}
		return info, nil
	}
	return attentionInfo{}, nil
}

func (a opencodeAdapter) Attention(ctx context.Context, se sessionInfo, _ string, _ bool) (attentionInfo, error) {
	sid, err := a.sessionID(ctx, se)
	if err != nil || sid == "" {
		return attentionInfo{}, err
	}
	return a.attention(ctx, sid)
}

func (a opencodeAdapter) RespondAttention(ctx context.Context, se sessionInfo, req attentionResponseRequest) (attentionResponseResult, error) {
	sid, err := a.sessionID(ctx, se)
	if err != nil || sid == "" {
		return attentionResponseResult{}, fmt.Errorf("no opencode session bound: %v", err)
	}
	info, err := a.attention(ctx, sid)
	if err != nil {
		return attentionResponseResult{}, err
	}
	if !info.Pending || info.ID != req.AttentionID {
		return attentionResponseResult{}, fmt.Errorf("request changed or already answered")
	}
	base := "/api/session/" + url.PathEscape(sid)
	switch info.Kind {
	case "approval":
		decision := map[string]string{"approve-once": "once", "approve-prefix": "always", "decline": "reject"}[req.Action]
		if decision == "" {
			return attentionResponseResult{}, fmt.Errorf("unsupported action %q", req.Action)
		}
		if err := a.s.oc.do(ctx, http.MethodPost, base+"/permission/"+url.PathEscape(info.ID)+"/reply", map[string]any{"decision": decision}, nil); err != nil {
			return attentionResponseResult{}, err
		}
	case "question":
		answer, err := a.formAnswer(ctx, sid, info.ID, req.Answers)
		if err != nil {
			return attentionResponseResult{}, err
		}
		if err := a.s.oc.do(ctx, http.MethodPost, base+"/form/"+url.PathEscape(info.ID)+"/reply", map[string]any{"answer": answer}, nil); err != nil {
			return attentionResponseResult{}, err
		}
	default:
		return attentionResponseResult{}, fmt.Errorf("nothing to respond to")
	}
	next, _ := a.attention(ctx, sid)
	return attentionResponseResult{SessionID: se.ID, AttentionID: req.AttentionID, Responded: true, Cleared: !next.Pending || next.ID != req.AttentionID}, nil
}

// formAnswer converts the PWA's label answers to typed form values.
func (a opencodeAdapter) formAnswer(ctx context.Context, sid, formID string, answers []attentionAnswer) (map[string]any, error) {
	var forms struct {
		Data []ocForm `json:"data"`
	}
	if err := a.s.oc.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sid)+"/form", nil, &forms); err != nil {
		return nil, err
	}
	var form *ocForm
	for i := range forms.Data {
		if forms.Data[i].ID == formID {
			form = &forms.Data[i]
		}
	}
	if form == nil {
		return nil, fmt.Errorf("form already answered")
	}
	given := map[string]string{}
	for _, an := range answers {
		given[an.ID] = an.Answer
	}
	out := map[string]any{}
	for _, f := range form.Fields {
		raw, ok := given[f.Key]
		if !ok {
			if f.Hidden {
				continue
			}
			return nil, fmt.Errorf("missing answer for %q", firstNonempty(f.Title, f.Key))
		}
		v, err := opencodeFormValue(f, raw)
		if err != nil {
			return nil, err
		}
		out[f.Key] = v
	}
	return out, nil
}

func opencodeFormValue(f ocFormField, raw string) (any, error) {
	byLabel := func(s string) string {
		s = strings.TrimSpace(s)
		for _, o := range f.Options {
			if strings.EqualFold(o.Label, s) || o.Value == s {
				return o.Value
			}
		}
		return s
	}
	switch f.Type {
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "yes", "true", "y", "1":
			return true, nil
		case "no", "false", "n", "0":
			return false, nil
		}
		return nil, fmt.Errorf("%q expects yes or no", firstNonempty(f.Title, f.Key))
	case "number", "integer":
		n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, fmt.Errorf("%q expects a number", firstNonempty(f.Title, f.Key))
		}
		return n, nil
	case "multiselect":
		var vals []string
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				vals = append(vals, byLabel(p))
			}
		}
		return vals, nil
	case "string":
		return byLabel(raw), nil
	}
	return nil, fmt.Errorf("%q (%s) can only be answered in the terminal", firstNonempty(f.Title, f.Key), f.Type)
}

// --- delivery ---

func (a opencodeAdapter) Deliver(ctx context.Context, se sessionInfo, text, delivery, _ string, _ bool) (deliveryResult, error) {
	sid, err := a.sessionID(ctx, se)
	if err != nil {
		return deliveryResult{}, err
	}
	if sid == "" {
		return deliveryResult{}, fmt.Errorf("no opencode session yet: send the first message in the terminal")
	}
	snap, err := a.snapshot(ctx, se, 1)
	if err != nil {
		return deliveryResult{}, err
	}
	busy := snap.Activity.Working
	mode := delivery
	if mode == "" || mode == "auto" {
		mode = "queue"
	}
	if mode != "queue" && mode != "steer" {
		return deliveryResult{}, fmt.Errorf("delivery must be auto, queue, or steer")
	}
	body := map[string]any{"text": text, "delivery": mode}
	if !busy {
		delete(body, "delivery") // idle: the service starts a turn at once
	}
	if err := a.s.oc.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sid)+"/prompt", body, nil); err != nil {
		return deliveryResult{}, err
	}
	switch {
	case !busy:
		return deliveryResult{State: "sent"}, nil
	case mode == "queue":
		return deliveryResult{State: "queued"}, nil
	default:
		return deliveryResult{State: "steered"}, nil
	}
}

func (a opencodeAdapter) Interrupt(ctx context.Context, se sessionInfo, _ string, _ bool) (bool, error) {
	sid, err := a.sessionID(ctx, se)
	if err != nil || sid == "" {
		return false, err
	}
	var out struct {
		Interrupted bool `json:"interrupted"`
	}
	if err := a.s.oc.do(ctx, http.MethodPost, "/api/session/"+url.PathEscape(sid)+"/interrupt", map[string]any{}, &out); err != nil {
		return false, err
	}
	return out.Interrupted, nil
}
