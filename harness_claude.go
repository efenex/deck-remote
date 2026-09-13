package main

import (
	"context"
	"fmt"
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

func (a claudeAdapter) Attention(_ context.Context, _ sessionInfo, pane string, paneOK bool) (attentionInfo, error) {
	if !paneOK {
		return attentionInfo{Unavailable: true}, nil
	}
	if !isClaudePermissionPrompt(pane) {
		return attentionInfo{}, nil
	}
	text := dialogExcerpt(pane)
	return attentionInfo{
		Pending: true, Kind: "approval", ID: fmt.Sprintf("claude-approval-%x", hashStr(text)),
		Text: text, Actions: []string{"approve-once"},
	}, nil
}

func (a claudeAdapter) Deliver(context.Context, sessionInfo, string, string, string, bool) (deliveryResult, error) {
	return deliveryResult{}, fmt.Errorf("Claude delivery uses the compatibility turn runner")
}

func (a claudeAdapter) Interrupt(context.Context, sessionInfo, string, bool) (bool, error) {
	return false, fmt.Errorf("structured interruption is unavailable for Claude")
}

func (a claudeAdapter) RespondAttention(ctx context.Context, se sessionInfo, req attentionResponseRequest) (attentionResponseResult, error) {
	if req.Action != "approve-once" {
		return attentionResponseResult{}, fmt.Errorf("Claude supports approve-once only")
	}
	pane, err := a.s.sessionPane(ctx, se.ID)
	if err != nil {
		return attentionResponseResult{}, err
	}
	info, _ := a.Attention(ctx, se, pane, true)
	if !info.Pending || info.ID != req.AttentionID {
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
