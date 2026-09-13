package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

type attentionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type attentionQuestion struct {
	Header   string            `json:"header"`
	ID       string            `json:"id"`
	Question string            `json:"question"`
	Options  []attentionOption `json:"options,omitempty"`
}

type attentionInfo struct {
	Pending     bool                `json:"pending"`
	Kind        string              `json:"kind,omitempty"` // approval | question
	ID          string              `json:"id,omitempty"`
	Text        string              `json:"text,omitempty"`
	Questions   []attentionQuestion `json:"questions,omitempty"`
	Actions     []string            `json:"actions,omitempty"`
	Unavailable bool                `json:"unavailable,omitempty"`
}

type attentionAnswer struct {
	ID     string `json:"id"`
	Answer string `json:"answer"`
}

type attentionResponseRequest struct {
	SessionID   string            `json:"sessionId"`
	Profile     string            `json:"profile"`
	AttentionID string            `json:"attentionId"`
	Action      string            `json:"action"`
	Answers     []attentionAnswer `json:"answers"`
}

type attentionResponseResult struct {
	SessionID   string `json:"sessionId"`
	AttentionID string `json:"attentionId,omitempty"`
	Responded   bool   `json:"responded"`
	Cleared     bool   `json:"cleared,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type attentionCache struct {
	mu sync.RWMutex
	m  map[string]attentionInfo
}

func newAttentionCache() *attentionCache { return &attentionCache{m: map[string]attentionInfo{}} }

func (c *attentionCache) set(id string, info attentionInfo) {
	c.mu.Lock()
	c.m[id] = info
	c.mu.Unlock()
}

func (c *attentionCache) get(id string) (attentionInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.m[id]
	return info, ok
}

func (s *server) handleAttention(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing id")
		return
	}
	ctx, cancel := cliCtx(withProfile(r.Context(), reqProfile(r)), 12*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	pane, paneErr := s.sessionPane(ctx, se.ID)
	info, err := s.adapterFor(se).Attention(ctx, se, pane, paneErr == nil)
	if err != nil {
		writeJSON(w, http.StatusOK, attentionInfo{Unavailable: true})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) handleAttentionRespond(w http.ResponseWriter, r *http.Request) {
	var req attentionResponseRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Profile = strings.TrimSpace(req.Profile)
	req.AttentionID = strings.TrimSpace(req.AttentionID)
	req.Action = strings.TrimSpace(req.Action)
	if req.SessionID == "" || req.AttentionID == "" {
		httpError(w, http.StatusBadRequest, "sessionId and attentionId required")
		return
	}
	ctx, cancel := cliCtx(withProfile(r.Context(), req.Profile), 25*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, req.SessionID)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	result, err := s.adapterFor(se).RespondAttention(ctx, se, req)
	if err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	s.hub.publish(map[string]any{
		"type": "attention-result", "sessionId": se.ID,
		"attentionId": req.AttentionID, "responded": result.Responded,
		"cleared": result.Cleared, "reason": result.Reason, "ts": time.Now().Unix(),
	})
	writeJSON(w, http.StatusOK, result)
}

var codexChoiceRE = regexp.MustCompile(`^\s*([›>•*]?)\s*([0-9]+)[.)]\s+(.+?)\s*$`)

type codexDialogChoice struct {
	Index    int
	Label    string
	Selected bool
}

type codexDialog struct {
	Text       string
	ID         string
	Choices    []codexDialogChoice
	SelectedAt int
}

func parseCodexChoices(pane string) []codexDialogChoice {
	lines := strings.Split(stripANSI(pane), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	choices := []codexDialogChoice{}
	for _, line := range lines {
		m := codexChoiceRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx := 0
		_, _ = fmt.Sscanf(m[2], "%d", &idx)
		choices = append(choices, codexDialogChoice{
			Index: idx, Label: strings.TrimSpace(m[3]), Selected: m[1] != "",
		})
	}
	return choices
}

func parseCodexApprovalDialog(pane string) (codexDialog, bool) {
	clean := stripANSI(pane)
	lines := strings.Split(clean, "\n")
	start := -1
	for i := len(lines) - 1; i >= 0; i-- {
		low := strings.ToLower(lines[i])
		if strings.Contains(low, "would you like to run") || strings.Contains(low, "do you want to approve") ||
			strings.Contains(low, "needs your approval") || strings.Contains(low, "approve this command") {
			start = i
			break
		}
	}
	if start < 0 {
		return codexDialog{}, false
	}
	dialogLines := lines[start:]
	lastChoice := -1
	for i, line := range dialogLines {
		if codexChoiceRE.MatchString(line) {
			lastChoice = i
		}
	}
	if lastChoice < 0 {
		return codexDialog{}, false
	}
	dialogText := strings.Join(dialogLines[:lastChoice+1], "\n")
	choices := parseCodexChoices(dialogText)
	if len(choices) < 2 {
		return codexDialog{}, false
	}
	selected := -1
	for i, c := range choices {
		if c.Selected {
			if selected >= 0 {
				return codexDialog{}, false
			}
			selected = i
		}
	}
	if selected < 0 {
		return codexDialog{}, false
	}
	text := dialogExcerptLines(dialogText, 24)
	return codexDialog{Text: text, ID: fmt.Sprintf("approval-%x", hashStr(text)), Choices: choices, SelectedAt: selected}, true
}

func dialogExcerptLines(pane string, maxLines int) string {
	raw := strings.Split(strings.TrimRight(stripANSI(pane), "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimRight(line, " "))
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func codexApprovalTarget(dialog codexDialog, action string) (int, error) {
	var matches []int
	for i, choice := range dialog.Choices {
		label := strings.ToLower(choice.Label)
		matched := false
		switch action {
		case "approve-once":
			matched = (strings.Contains(label, "yes") || strings.Contains(label, "approve") || strings.Contains(label, "run")) &&
				!strings.Contains(label, "don't ask") && !strings.Contains(label, "prefix") && !strings.Contains(label, "session") && !strings.Contains(label, "remember")
		case "approve-prefix":
			matched = strings.Contains(label, "don't ask") || strings.Contains(label, "prefix") || strings.Contains(label, "remember")
		case "decline":
			matched = strings.HasPrefix(label, "no") || strings.Contains(label, "decline") || strings.Contains(label, "deny") || strings.Contains(label, "cancel")
		default:
			return -1, fmt.Errorf("unknown approval action")
		}
		if matched {
			matches = append(matches, i)
		}
	}
	if len(matches) != 1 {
		return -1, fmt.Errorf("live approval dialog does not unambiguously offer %s", action)
	}
	return matches[0], nil
}

func (s *server) moveDialogSelection(ctx context.Context, id string, from, to int) error {
	key := "Down"
	count := to - from
	if count < 0 {
		key, count = "Up", -count
	}
	for i := 0; i < count; i++ {
		if err := s.sendNamedKey(ctx, id, key); err != nil {
			return err
		}
	}
	return s.sendKeyEnter(ctx, id)
}
