package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type codexAdapter struct{ s *server }

func (a codexAdapter) resolution(ctx context.Context, se sessionInfo) (codexResolution, harnessSnapshot, error) {
	res, err := a.s.resolveCodex(ctx, se)
	if err != nil {
		return codexResolution{}, harnessSnapshot{}, err
	}
	snap, err := a.s.codex.Load(res)
	if err != nil {
		return codexResolution{}, harnessSnapshot{}, err
	}
	if snap.DegradedReason != "" {
		return res, snap, errTerminalOnly
	}
	return res, snap, nil
}

func (a codexAdapter) Capabilities(ctx context.Context, se sessionInfo) sessionCapabilities {
	return sessionCapabilities{
		Structured: true, History: true, Replies: true, Activity: true, Ask: true,
		Slash: true, Queue: true, Steer: true, Interrupt: true, Attention: true,
		Approvals: true, Questions: true, SlashCommands: codexSlashCapabilities,
	}
}

func (a codexAdapter) Snapshot(ctx context.Context, se sessionInfo, pane string, paneOK bool) (harnessSnapshot, error) {
	_, snap, err := a.resolution(ctx, se)
	if err != nil {
		if snap.DegradedReason == "" {
			snap.DegradedReason = err.Error()
		}
		snap.State = "idle"
		return snap, nil
	}
	if paneOK {
		// Pane parsing is deliberately limited to attention/draft verification;
		// rollout task events remain the source of working/completed state.
		if dialog, ok := parseCodexApprovalDialog(pane); ok {
			snap.Attention = attentionInfo{Pending: true, Kind: "approval", ID: dialog.ID, Text: dialog.Text,
				Actions: availableCodexApprovalActions(dialog)}
		}
	}
	if cached, ok := a.s.acts.get(se.ID); ok && cached.Working == snap.Activity.Working {
		snap.Activity.Stalled = cached.Stalled
	}
	return snap, nil
}

func (a codexAdapter) History(ctx context.Context, se sessionInfo) ([]histMsg, error) {
	_, snap, err := a.resolution(ctx, se)
	if err != nil {
		return nil, err
	}
	return snap.Messages, nil
}

func (a codexAdapter) Attention(ctx context.Context, se sessionInfo, pane string, paneOK bool) (attentionInfo, error) {
	_, snap, err := a.resolution(ctx, se)
	if err != nil {
		return attentionInfo{Unavailable: true}, err
	}
	if paneOK {
		if dialog, ok := parseCodexApprovalDialog(pane); ok {
			return attentionInfo{Pending: true, Kind: "approval", ID: dialog.ID, Text: dialog.Text,
				Actions: availableCodexApprovalActions(dialog)}, nil
		}
	}
	if snap.Attention.Pending {
		return snap.Attention, nil
	}
	if !paneOK && snap.Activity.Working {
		return attentionInfo{Unavailable: true}, nil
	}
	return attentionInfo{}, nil
}

func availableCodexApprovalActions(dialog codexDialog) []string {
	actions := []string{}
	for _, action := range []string{"approve-once", "approve-prefix", "decline"} {
		if _, err := codexApprovalTarget(dialog, action); err == nil {
			actions = append(actions, action)
		}
	}
	return actions
}

func codexComposerHasDraft(pane string) bool {
	lines := strings.Split(stripANSI(pane), "\n")
	start := len(lines) - 10
	if start < 0 {
		start = 0
	}
	for _, line := range lines[start:] {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "›") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trim, "›"))
		if rest != "" && !strings.HasPrefix(strings.ToLower(rest), "ask codex") {
			return true
		}
	}
	return false
}

func (a codexAdapter) Deliver(ctx context.Context, se sessionInfo, text, delivery, pane string, paneOK bool) (deliveryResult, error) {
	_, snap, err := a.resolution(ctx, se)
	if err != nil {
		return deliveryResult{}, err
	}
	if !paneOK {
		return deliveryResult{}, fmt.Errorf("could not verify the live Codex composer")
	}
	if codexComposerHasDraft(pane) {
		return deliveryResult{}, fmt.Errorf("delivery rejected: the live Codex composer contains a desktop draft")
	}
	busy := snap.Activity.Working
	mode := delivery
	if mode == "" || mode == "auto" {
		if busy {
			mode = "queue"
		} else {
			mode = "steer"
		}
	}
	if mode == "queue" && !busy {
		mode = "steer" // Tab is queue-only while Codex is actively working.
	}
	if mode != "queue" && mode != "steer" {
		return deliveryResult{}, fmt.Errorf("delivery must be auto, queue, or steer")
	}
	if err := a.s.sendKeyText(ctx, se.ID, text); err != nil {
		return deliveryResult{}, err
	}
	if mode == "queue" {
		if err := a.s.sendNamedKey(ctx, se.ID, "Tab"); err != nil {
			return deliveryResult{}, err
		}
		return deliveryResult{State: "queued"}, nil
	}
	if err := a.s.sendKeyEnter(ctx, se.ID); err != nil {
		return deliveryResult{}, err
	}
	if busy {
		return deliveryResult{State: "steered"}, nil
	}
	return deliveryResult{State: "sent"}, nil
}

func (a codexAdapter) Interrupt(ctx context.Context, se sessionInfo, pane string, paneOK bool) (bool, error) {
	_, snap, err := a.resolution(ctx, se)
	if err != nil {
		return false, err
	}
	if !snap.Activity.Working {
		return false, nil
	}
	if !paneOK {
		return false, fmt.Errorf("could not verify the live Codex pane")
	}
	if codexComposerHasDraft(pane) {
		return false, fmt.Errorf("interrupt rejected: the live Codex composer contains a draft")
	}
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
		_, next, loadErr := a.resolution(ctx, se)
		if loadErr == nil && !next.Activity.Working && next.State == "interrupted" {
			return true, nil
		}
	}
	// Rollout verification is authoritative. A timeout reports stopping rather
	// than claiming success from a possibly stale pane.
	return false, nil
}

func (a codexAdapter) RespondAttention(ctx context.Context, se sessionInfo, req attentionResponseRequest) (attentionResponseResult, error) {
	pane, err := a.s.sessionPane(ctx, se.ID)
	if err != nil {
		return attentionResponseResult{}, err
	}
	if dialog, ok := parseCodexApprovalDialog(pane); ok && dialog.ID == req.AttentionID {
		return a.respondApproval(ctx, se, req, dialog)
	}
	info, err := a.Attention(ctx, se, pane, true)
	if err != nil || !info.Pending || info.Kind != "question" || info.ID != req.AttentionID {
		return attentionResponseResult{}, fmt.Errorf("attention request changed or cleared")
	}
	if req.Action != "answer" {
		return attentionResponseResult{}, fmt.Errorf("question responses require action=answer")
	}
	return a.respondQuestions(ctx, se, req, info)
}

func (a codexAdapter) respondApproval(ctx context.Context, se sessionInfo, req attentionResponseRequest, dialog codexDialog) (attentionResponseResult, error) {
	target, err := codexApprovalTarget(dialog, req.Action)
	if err != nil {
		return attentionResponseResult{}, err
	}
	// Re-read and compare the full dialog immediately before navigating.
	pane, err := a.s.sessionPane(ctx, se.ID)
	if err != nil {
		return attentionResponseResult{}, err
	}
	cur, ok := parseCodexApprovalDialog(pane)
	if !ok || cur.ID != req.AttentionID || cur.Text != dialog.Text {
		return attentionResponseResult{}, fmt.Errorf("approval dialog changed or cleared")
	}
	target, err = codexApprovalTarget(cur, req.Action)
	if err != nil {
		return attentionResponseResult{}, err
	}
	if err := a.s.moveDialogSelection(ctx, se.ID, cur.SelectedAt, target); err != nil {
		return attentionResponseResult{}, err
	}
	cleared := false
	for i := 0; i < 8; i++ {
		select {
		case <-ctx.Done():
			break
		case <-time.After(250 * time.Millisecond):
		}
		p, readErr := a.s.sessionPane(ctx, se.ID)
		if readErr == nil {
			if next, pending := parseCodexApprovalDialog(p); !pending || next.ID != req.AttentionID {
				cleared = true
				break
			}
		}
	}
	return attentionResponseResult{SessionID: se.ID, AttentionID: req.AttentionID, Responded: true, Cleared: cleared}, nil
}

func (a codexAdapter) respondQuestions(ctx context.Context, se sessionInfo, req attentionResponseRequest, info attentionInfo) (attentionResponseResult, error) {
	answers := map[string]string{}
	for _, answer := range req.Answers {
		if strings.TrimSpace(answer.ID) == "" || strings.TrimSpace(answer.Answer) == "" {
			return attentionResponseResult{}, fmt.Errorf("every answer requires id and answer")
		}
		answers[answer.ID] = answer.Answer
	}
	if len(answers) != len(info.Questions) {
		return attentionResponseResult{}, fmt.Errorf("all questions must be answered")
	}
	for _, q := range info.Questions {
		answer, ok := answers[q.ID]
		if !ok {
			return attentionResponseResult{}, fmt.Errorf("missing answer for %s", q.ID)
		}
		pane, err := a.s.sessionPane(ctx, se.ID)
		if err != nil {
			return attentionResponseResult{}, err
		}
		// The live form must still identify the expected question. This keeps a
		// delayed phone response from landing in a later prompt or composer.
		low := strings.ToLower(stripANSI(pane))
		if !strings.Contains(low, strings.ToLower(q.Header)) && !strings.Contains(low, strings.ToLower(q.Question)) {
			return attentionResponseResult{}, fmt.Errorf("live question changed before %s", q.ID)
		}
		choices := parseCodexChoices(pane)
		selected := -1
		for i, c := range choices {
			if c.Selected {
				selected = i
			}
		}
		target, freeform, targetErr := codexQuestionTarget(choices, answer)
		if targetErr != nil {
			return attentionResponseResult{}, targetErr
		}
		if selected < 0 {
			return attentionResponseResult{}, fmt.Errorf("live form has no unambiguous selection")
		}
		if !freeform {
			if err := a.s.moveDialogSelection(ctx, se.ID, selected, target); err != nil {
				return attentionResponseResult{}, err
			}
		} else {
			if err := a.s.moveDialogSelection(ctx, se.ID, selected, target); err != nil {
				return attentionResponseResult{}, err
			}
			time.Sleep(100 * time.Millisecond)
			if err := a.s.sendKeyText(ctx, se.ID, answer); err != nil {
				return attentionResponseResult{}, err
			}
			if err := a.s.sendKeyEnter(ctx, se.ID); err != nil {
				return attentionResponseResult{}, err
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	cleared := false
	for i := 0; i < 10; i++ {
		_, snap, loadErr := a.resolution(ctx, se)
		if loadErr == nil && (!snap.Attention.Pending || snap.Attention.ID != req.AttentionID) {
			cleared = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	return attentionResponseResult{SessionID: se.ID, AttentionID: req.AttentionID, Responded: true, Cleared: cleared}, nil
}

func codexQuestionTarget(choices []codexDialogChoice, answer string) (target int, freeform bool, err error) {
	target = -1
	for i, c := range choices {
		if sameChoiceLabel(c.Label, answer) {
			if target >= 0 {
				return -1, false, fmt.Errorf("answer is ambiguous in the live form")
			}
			target = i
		}
	}
	if target >= 0 {
		return target, false, nil
	}
	for i, c := range choices {
		label := strings.ToLower(c.Label)
		if strings.Contains(label, "other") || strings.Contains(label, "own answer") || strings.Contains(label, "custom") {
			if target >= 0 {
				return -1, false, fmt.Errorf("freeform row is ambiguous")
			}
			target = i
		}
	}
	if target < 0 {
		return -1, false, fmt.Errorf("live form does not offer a safe freeform answer")
	}
	return target, true, nil
}

func sameChoiceLabel(display, answer string) bool {
	normalize := func(s string) string {
		s = strings.TrimSpace(strings.ToLower(s))
		if i := strings.Index(s, " — "); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	return normalize(display) == normalize(answer)
}
