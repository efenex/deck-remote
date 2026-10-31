package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

type claudeAdapter struct{ s *server }

func (a claudeAdapter) Capabilities(context.Context, sessionInfo) sessionCapabilities {
	return sessionCapabilities{
		Structured: true, History: true, Replies: true, Activity: true, Ask: true,
		Slash: true, Queue: true, Attention: true, Approvals: true,
		SlashCommands: claudeSlashCapabilities,
	}
}

func (a claudeAdapter) Snapshot(ctx context.Context, se sessionInfo, pane string, paneOK bool) (harnessSnapshot, error) {
	snap := harnessSnapshot{State: "idle"}
	out, err := a.s.claudeReply(ctx, se)
	if err == nil {
		snap.LastReply = cleanReplyContent(out.Content)
		if t, parseErr := parseRFC3339(out.Timestamp); parseErr == nil {
			snap.LastActivity = t
		}
	}
	if paneOK {
		snap.Activity = a.s.acts.update(se.ID, parseActivity(pane)).info()
	} else {
		snap.Activity = a.s.liveActivity(ctx, se.ID)
	}
	if snap.Activity.Working {
		snap.State = "working"
	}
	snap.Attention, _ = a.Attention(ctx, se, pane, paneOK)
	return snap, nil
}

func parseRFC3339(s string) (int64, error) {
	t := tsUnix(s)
	if t == 0 {
		return 0, fmt.Errorf("invalid timestamp")
	}
	return t, nil
}

func (a claudeAdapter) History(ctx context.Context, se sessionInfo) ([]histMsg, error) {
	out, err := a.s.claudeReply(ctx, se)
	if err != nil {
		return nil, err
	}
	path := findTranscript(out.ClaudeSessionID, se.Path)
	if path == "" {
		return nil, nil
	}
	return parseTranscript(path)
}

func (a claudeAdapter) Attention(_ context.Context, se sessionInfo, pane string, paneOK bool) (attentionInfo, error) {
	if !paneOK {
		return attentionInfo{Unavailable: true}, nil
	}
	if !isClaudePermissionPrompt(pane) {
		// A tabbed multi-question menu: the card is the transcript's pending tool_use.
		if _, tabbed := parseAskPane(pane); tabbed {
			if ask, ok := a.pendingAsk(se); ok {
				if info, ok := askMenuInfo(pane, ask); ok {
					return info, nil
				}
			}
		}
		if info, ok := claudeQuestionInfo(pane); ok {
			return info, nil
		}
		return attentionInfo{}, nil
	}
	// The "will automatically block this request in M:SS" line ticks every second; it is
	// not part of the dialog's identity (the fingerprint must survive the tick) and goes to
	// the card as a deadline instead of text.
	stable, remaining := stripClaudeCountdown(pane)
	text := dialogExcerpt(stable)
	info := attentionInfo{
		Pending: true, Kind: "approval", ID: fmt.Sprintf("claude-approval-%x", hashStr(text)),
		Text: text, Actions: []string{"approve-once"},
	}
	if remaining >= 0 {
		info.ExpiresAt = claudeNow().Add(remaining).Unix()
	}
	return info, nil
}

func (a claudeAdapter) Deliver(context.Context, sessionInfo, string, string, string, bool) (deliveryResult, error) {
	return deliveryResult{}, fmt.Errorf("Claude delivery uses the compatibility turn runner")
}

func (a claudeAdapter) Interrupt(context.Context, sessionInfo, string, bool) (bool, error) {
	return false, fmt.Errorf("structured interruption is unavailable for Claude")
}

func (a claudeAdapter) RespondAttention(ctx context.Context, se sessionInfo, req attentionResponseRequest) (attentionResponseResult, error) {
	if req.Action == "answer" {
		return a.answerQuestion(ctx, se, req)
	}
	if req.Action != "approve-once" {
		return attentionResponseResult{}, fmt.Errorf("Claude supports approve-once and answering a multi-question menu; answer other menus in the terminal")
	}
	pane, err := a.s.sessionPane(ctx, se.ID)
	if err != nil {
		return attentionResponseResult{}, err
	}
	info, _ := a.Attention(ctx, se, pane, true)
	if !info.Pending || info.Kind != "approval" || info.ID != req.AttentionID {
		return attentionResponseResult{}, fmt.Errorf("approval dialog changed or cleared")
	}
	// Revalidate immediately before the keystrokes.
	pane, err = a.s.sessionPane(ctx, se.ID)
	if err != nil || !isClaudePermissionPrompt(pane) {
		return attentionResponseResult{}, fmt.Errorf("approval dialog changed or cleared")
	}
	if cur, _ := a.Attention(ctx, se, pane, true); cur.ID != req.AttentionID {
		return attentionResponseResult{}, fmt.Errorf("approval dialog changed")
	}
	if err := a.s.sendKeyText(ctx, se.ID, "1"); err != nil {
		return attentionResponseResult{}, err
	}
	if err := a.s.sendKeyEnter(ctx, se.ID); err != nil {
		return attentionResponseResult{}, err
	}
	cleared := a.s.pollDialogCleared(ctx, se.ID)
	return attentionResponseResult{SessionID: se.ID, AttentionID: req.AttentionID, Responded: true, Cleared: cleared}, nil
}

// pendingAsk finds the session's unanswered AskUserQuestion in its transcript. The candidate
// Claude ids are the ones claudeReply trusts (live env, last confirmed, the list's).
func (a claudeAdapter) pendingAsk(se sessionInfo) (pendingAsk, bool) {
	if a.s == nil {
		return pendingAsk{}, false
	}
	e := a.s.replies.entry(se.ID)
	e.mu.Lock()
	ids := []string{e.envID, e.claudeID, se.ClaudeSessionID}
	e.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if p := findTranscript(id, se.Path); p != "" {
			if ask, ok := pendingAskFromTranscript(p); ok {
				return ask, true
			}
		}
	}
	return pendingAsk{}, false
}

// askRunning keeps two phones from walking the same menu at once.
var askRunning sync.Map

// errAskBusy: another tap's walk owns the menu. It is that tap's outcome that matters, so this
// refusal is only the second tapper's HTTP reply, not an event for every listener.
var errAskBusy = errors.New("an answer for this session is already being sent")

// answerQuestion answers a tabbed multi-question menu through the gated send-keys path
// (typed.go: act token + operator identity guard the endpoint). The card, the transcript and
// the live pane are re-checked here and again before every key batch (answerAsk).
func (a claudeAdapter) answerQuestion(ctx context.Context, se sessionInfo, req attentionResponseRequest) (attentionResponseResult, error) {
	pane, err := a.s.sessionPane(ctx, se.ID)
	if err != nil {
		return attentionResponseResult{}, err
	}
	info, _ := a.Attention(ctx, se, pane, true)
	if !info.Pending || info.Kind != "question" || info.ID != req.AttentionID {
		return attentionResponseResult{}, fmt.Errorf("question menu changed or cleared")
	}
	if info.ReadOnly || len(info.Actions) == 0 {
		return attentionResponseResult{}, fmt.Errorf("this menu cannot be answered from here; use the terminal")
	}
	if info.MenuState != "" && req.MenuState != info.MenuState {
		return attentionResponseResult{}, askChanged("stale card; refresh the question before answering")
	}
	ask, ok := a.pendingAsk(se)
	if !ok || "claude-askq-"+ask.ToolUseID != req.AttentionID {
		return attentionResponseResult{}, fmt.Errorf("question menu changed or cleared")
	}
	picks, err := askPicks(ask, req.Answers)
	if err != nil {
		return attentionResponseResult{}, err
	}
	if _, busy := askRunning.LoadOrStore(se.ID, true); busy {
		return attentionResponseResult{}, errAskBusy
	}
	defer askRunning.Delete(se.ID)
	var panes, keys int
	t0 := time.Now()
	io := askIO{
		initialState: req.MenuState,
		pane:         func(ctx context.Context) (string, error) { panes++; return a.s.sessionPane(ctx, se.ID) },
		text:         func(ctx context.Context, t string) error { keys++; return a.s.sendKeyText(ctx, se.ID, t) },
		named:        func(ctx context.Context, k string) error { keys++; return a.s.sendNamedKey(ctx, se.ID, k) },
		enter:        func(ctx context.Context) error { keys++; return a.s.sendKeyEnter(ctx, se.ID) },
		// 100 ms polls on the operator lane: a key shows on screen within a frame or two
		wait: 100 * time.Millisecond, polls: 40,
		// a loaded host (load avg 44 on 2026-10-31) can take seconds per capture: wait on the clock too
		stepTimeout: 12 * time.Second,
		progress: func(p askProgress) {
			a.s.hub.publish(map[string]any{
				"type": "attention-progress", "sessionId": se.ID, "attentionId": req.AttentionID,
				"step": p.Step, "n": p.N, "total": p.Total, "ts": time.Now().Unix(),
			})
		},
	}
	done, cleared, err := answerAsk(ctx, io, ask, picks)
	log.Printf("answer walk %s: %d question(s), %d pane read(s), %d key send(s), %s, err=%v", se.ID, len(ask.Questions), panes, keys, time.Since(t0).Round(time.Millisecond), err)
	if err != nil {
		if done > 0 {
			// Not a plain failure: those answers are in the menu, and sending again finishes the rest.
			err = &askPartialError{Done: done, Total: len(ask.Questions), Err: err}
		}
		return attentionResponseResult{}, err
	}
	return attentionResponseResult{SessionID: se.ID, AttentionID: req.AttentionID, Responded: true, Cleared: cleared}, nil
}
