package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	normalCodexID   = "11111111-1111-4111-8111-111111111111"
	rotationCodexID = "22222222-2222-4222-8222-222222222222"
)

func fixturePath(name string) string { return filepath.Join("testdata", "codex", name) }

func rolloutResolution(id string, paths ...string) codexResolution {
	return codexResolution{SessionID: id, Paths: paths, Version: "0.144.0"}
}

func TestCodexRolloutStructuredSnapshot(t *testing.T) {
	store := newCodexRolloutStore()
	snap, err := store.Load(rolloutResolution(normalCodexID, fixturePath("normal.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Activity.Working || snap.State != "working" {
		t.Fatalf("state = %q working=%v, want working", snap.State, snap.Activity.Working)
	}
	if snap.LastReply != "Service is healthy." || len(snap.Messages) != 5 {
		t.Fatalf("unexpected conversation snapshot: reply=%q messages=%+v", snap.LastReply, snap.Messages)
	}
	if got := snap.Messages[1]; got.Role != "commentary" || got.Content != "Inspecting the service." {
		t.Fatalf("commentary was not preserved in history: %+v", got)
	}
	if !snap.Attention.Pending || snap.Attention.Kind != "question" || snap.Attention.ID != "question-1" {
		t.Fatalf("question attention not surfaced: %+v", snap.Attention)
	}
	if len(snap.Attention.Questions) != 2 || snap.Attention.Questions[0].ID != "target" {
		t.Fatalf("multi-question payload not preserved: %+v", snap.Attention.Questions)
	}
}

func TestCodexRolloutCommentaryAndToolActivity(t *testing.T) {
	lines := mustLines(t, fixturePath("normal.jsonl"))
	path := filepath.Join(t.TempDir(), "active.jsonl")
	mustWrite(t, path, strings.Join(lines[:5], "\n")+"\n")
	store := newCodexRolloutStore()
	snap, err := store.Load(rolloutResolution(normalCodexID, path))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Activity.Activity != "Inspecting the service." || snap.Activity.CurrentTool != "exec" {
		t.Fatalf("activity = %+v", snap.Activity)
	}
	if len(snap.Messages) != 2 || snap.Messages[1].Role != "commentary" {
		t.Fatalf("commentary history = %+v", snap.Messages)
	}
	again, err := store.Load(rolloutResolution(normalCodexID, path))
	if err != nil || len(again.Messages) != 2 {
		t.Fatalf("repeated load duplicated commentary: messages=%+v err=%v", again.Messages, err)
	}
}

func TestCodexRolloutMalformedTailThenAppend(t *testing.T) {
	b, err := os.ReadFile(fixturePath("malformed-tail.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	mustWrite(t, path, strings.TrimSuffix(string(b), "\n"))
	store := newCodexRolloutStore()
	id := "44444444-4444-4444-8444-444444444444"
	first, err := store.Load(rolloutResolution(id, path))
	if err != nil || !first.Activity.Working || first.LastReply != "" {
		t.Fatalf("partial load = %+v err=%v", first, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(",\"phase\":\"final_answer\"}}\n{\"timestamp\":\"2026-07-10T13:00:03Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":\"tail-turn\"}}\n")
	_ = f.Close()
	second, err := store.Load(rolloutResolution(id, path))
	if err != nil || second.LastReply != "partial" || second.State != "completed" {
		t.Fatalf("completed tail = %+v err=%v", second, err)
	}
}

func TestCodexRolloutRotationAndTruncation(t *testing.T) {
	store := newCodexRolloutStore()
	first, err := store.Load(rolloutResolution(rotationCodexID, fixturePath("rotation-1.jsonl")))
	if err != nil || !first.Activity.Working {
		t.Fatalf("first segment = %+v err=%v", first, err)
	}
	rotated, err := store.Load(rolloutResolution(rotationCodexID, fixturePath("rotation-1.jsonl"), fixturePath("rotation-2.jsonl")))
	if err != nil || rotated.State != "completed" || rotated.LastReply != "rotation complete" {
		t.Fatalf("rotated = %+v err=%v", rotated, err)
	}

	path := filepath.Join(t.TempDir(), "truncate.jsonl")
	mustWrite(t, path, mustRead(t, fixturePath("normal.jsonl")))
	truncStore := newCodexRolloutStore()
	_, _ = truncStore.Load(rolloutResolution(normalCodexID, path))
	replacement := fmt.Sprintf("{\"timestamp\":\"2026-07-10T14:00:00Z\",\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cli_version\":\"0.144.0\"}}\n", normalCodexID)
	mustWrite(t, path, replacement)
	reset, err := truncStore.Load(rolloutResolution(normalCodexID, path))
	if err != nil || len(reset.Messages) != 0 || reset.LastReply != "" {
		t.Fatalf("truncated rollout was not reset: %+v err=%v", reset, err)
	}
}

func TestCodexUnknownVersionDegrades(t *testing.T) {
	snap, err := newCodexRolloutStore().Load(rolloutResolution("33333333-3333-4333-8333-333333333333", fixturePath("unknown.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap.DegradedReason, "0.145.0") {
		t.Fatalf("degraded reason = %q", snap.DegradedReason)
	}
}

func TestCodexResumed0143RolloutIsSupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resumed-0143.jsonl")
	mustWrite(t, path, strings.Replace(mustRead(t, fixturePath("normal.jsonl")), "0.144.0", "0.143.0", 1))

	snap, err := newCodexRolloutStore().Load(rolloutResolution(normalCodexID, path))
	if err != nil {
		t.Fatal(err)
	}
	if snap.DegradedReason != "" || snap.LastReply != "Service is healthy." || len(snap.Messages) != 5 {
		t.Fatalf("0.143 resumed rollout was not fully parsed: %+v", snap)
	}
}

func TestCodexResolverPrecedenceAndMetadataValidation(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	upID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	tmuxID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	sideID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	for _, id := range []string{upID, tmuxID, sideID} {
		writeRollout(t, codexHome, id, id)
	}
	mustMkdir(t, filepath.Join(home, ".agent-deck", "hooks"))
	mustWrite(t, filepath.Join(home, ".agent-deck", "hooks", "deck-id.sid"), sideID+"\n")
	tmux := filepath.Join(home, "fake-tmux")
	tmuxLog := filepath.Join(home, "tmux.log")
	t.Setenv("TMUX_LOG", tmuxLog)
	mustWrite(t, tmux, "#!/bin/sh\nprintf x >> \"$TMUX_LOG\"\ncase \"$4\" in\n CODEX_SESSION_ID) echo CODEX_SESSION_ID="+tmuxID+" ;;\n CODEX_HOME) echo CODEX_HOME="+codexHome+" ;;\nesac\n")
	if err := os.Chmod(tmux, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, config{homeDir: home, tmuxBin: tmux})
	se := sessionInfo{ID: "deck-id", Tool: "codex", TmuxSession: "tmux-target", CodexSessionID: upID, ResolvedCodexHome: codexHome}
	res, err := s.resolveCodex(context.Background(), se)
	if err != nil || res.SessionID != upID || res.Source != "upstream" {
		t.Fatalf("upstream resolution = %+v err=%v", res, err)
	}
	if _, err := os.Stat(tmuxLog); !os.IsNotExist(err) {
		t.Fatal("upstream metadata resolution unnecessarily queried tmux")
	}
	se.CodexSessionID = ""
	res, err = s.resolveCodex(context.Background(), se)
	if err != nil || res.SessionID != tmuxID || res.Source != "tmux" {
		t.Fatalf("tmux resolution = %+v err=%v", res, err)
	}
	se.TmuxSession = ""
	res, err = s.resolveCodex(context.Background(), se)
	if err != nil || res.SessionID != sideID || res.Source != "sidecar" {
		t.Fatalf("sidecar resolution = %+v err=%v", res, err)
	}

	badID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	writeRollout(t, codexHome, badID, upID) // filename matches, metadata does not
	se.ID = "no-sidecar"
	se.CodexSessionID, se.ResolvedCodexHome = badID, codexHome
	if _, err := s.resolveCodex(context.Background(), se); err == nil {
		t.Fatal("resolver accepted a filename without matching rollout metadata")
	}
}

func TestCodexApprovalParserAndStaleIdentity(t *testing.T) {
	pane := "Command needs your approval.\n$ deploy staging\n› 1. Yes, proceed\n  2. Yes, and don't ask again for commands that start with deploy\n  3. No, decline\n"
	dialog, ok := parseCodexApprovalDialog(pane)
	if !ok || dialog.SelectedAt != 0 {
		t.Fatalf("dialog not parsed: %+v ok=%v", dialog, ok)
	}
	for action, want := range map[string]int{"approve-once": 0, "approve-prefix": 1, "decline": 2} {
		got, err := codexApprovalTarget(dialog, action)
		if err != nil || got != want {
			t.Errorf("%s target=%d err=%v, want %d", action, got, err, want)
		}
	}
	changed, ok := parseCodexApprovalDialog(strings.Replace(pane, "deploy staging", "deploy production", 1))
	if !ok || changed.ID == dialog.ID {
		t.Fatal("changed live request retained stale attention id")
	}
	ambiguous := pane + "  4. Approve once\n"
	if d, ok := parseCodexApprovalDialog(ambiguous); !ok {
		t.Fatal("ambiguous dialog should parse before action validation")
	} else if _, err := codexApprovalTarget(d, "approve-once"); err == nil {
		t.Fatal("ambiguous approve-once action was accepted")
	}
}

func TestCodexDraftConflict(t *testing.T) {
	if !codexComposerHasDraft("status\n› unsent desktop draft\n  gpt-5\n") {
		t.Fatal("desktop draft was not detected")
	}
	if codexComposerHasDraft("status\n› \n  gpt-5\n") {
		t.Fatal("empty composer was treated as a draft")
	}
}

func TestCodexStallUsesVerifiedPaneProgress(t *testing.T) {
	now := time.Unix(100, 0)
	unknown := activityInfo{Working: true, Activity: "Running tests", stallKeySet: true}
	st := stepStall(activityState{}, unknown, now)
	st = stepStall(st, unknown, now.Add(time.Second))
	if st.Stalled {
		t.Fatal("static commentary without a pane progress marker was called stalled")
	}
	verified := activityInfo{Working: true, Activity: "Running tests", stallKeySet: true, stallKey: "esc to interrupt · 20s"}
	st = stepStall(activityState{}, verified, now)
	st = stepStall(st, verified, now.Add(time.Second))
	if !st.Stalled {
		t.Fatal("unchanged verified pane progress was not called stalled")
	}
	if got := codexPaneProgressKey("log\nworking · 20s · esc to interrupt\nfooter"); !strings.Contains(got, "20s") {
		t.Fatalf("progress key = %q", got)
	}
}

func TestCodexQuestionChoiceAndFreeformTargets(t *testing.T) {
	choices := []codexDialogChoice{
		{Index: 1, Label: "Staging — deploy safely", Selected: true},
		{Index: 2, Label: "Production — deploy live"},
		{Index: 3, Label: "Type your own answer"},
	}
	if target, freeform, err := codexQuestionTarget(choices, "Production"); err != nil || target != 1 || freeform {
		t.Fatalf("option target=%d freeform=%v err=%v", target, freeform, err)
	}
	if target, freeform, err := codexQuestionTarget(choices, "canary"); err != nil || target != 2 || !freeform {
		t.Fatalf("freeform target=%d freeform=%v err=%v", target, freeform, err)
	}
	ambiguous := append(append([]codexDialogChoice(nil), choices...), codexDialogChoice{Index: 4, Label: "Other"})
	if _, _, err := codexQuestionTarget(ambiguous, "canary"); err == nil {
		t.Fatal("ambiguous freeform rows were accepted")
	}
}

func TestApprovalNavigationSendsRelativeKeys(t *testing.T) {
	home := t.TempDir()
	logPath := filepath.Join(home, "keys.log")
	bin := filepath.Join(home, "fake-agent-deck")
	mustWrite(t, bin, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$KEY_LOG\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEY_LOG", logPath)
	s := testServer(t, config{homeDir: home, bin: bin, profile: "default"})
	if err := s.moveDialogSelection(context.Background(), "deck", 0, 2); err != nil {
		t.Fatal(err)
	}
	log := mustRead(t, logPath)
	if strings.Count(log, "--named-key Down") != 2 || !strings.Contains(log, "--enter") || strings.Count(log, "--primary") != 3 {
		t.Fatalf("navigation keys = %q", log)
	}
}

func TestDeckRemoteTargetsManagedPrimaryWindow(t *testing.T) {
	home := t.TempDir()
	logPath := filepath.Join(home, "keys.log")
	bin := filepath.Join(home, "fake-agent-deck")
	mustWrite(t, bin, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$KEY_LOG\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEY_LOG", logPath)
	s := testServer(t, config{homeDir: home, bin: bin, profile: "default"})
	ctx := context.Background()
	_, _ = s.sessionPane(ctx, "deck")
	_ = s.sendKeyText(ctx, "deck", "hello")
	_ = s.sendNamedKey(ctx, "deck", "Tab")
	_ = s.sendKeyEnter(ctx, "deck")

	log := mustRead(t, logPath)
	for _, want := range []string{
		"session output deck --pane --primary",
		"session send-keys deck --text hello --primary",
		"session send-keys deck --named-key Tab --primary",
		"session send-keys deck --enter --primary",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing primary-window command %q in:\n%s", want, log)
		}
	}
}

func TestCodexQueueAndSteerKeys(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	id := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	path := writeRollout(t, codexHome, id, id)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{\"timestamp\":\"2026-07-10T15:00:01Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"busy\"}}\n")
	_ = f.Close()
	logPath := filepath.Join(home, "keys.log")
	bin := filepath.Join(home, "fake-agent-deck")
	mustWrite(t, bin, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$KEY_LOG\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEY_LOG", logPath)
	s := testServer(t, config{homeDir: home, bin: bin, profile: "default"})
	se := sessionInfo{ID: "deck", Tool: "codex", CodexSessionID: id, ResolvedCodexHome: codexHome}
	a := codexAdapter{s: s}
	if got, err := a.Deliver(context.Background(), se, "later", "auto", "›\n", true); err != nil || got.State != "queued" {
		t.Fatalf("queue result=%+v err=%v", got, err)
	}
	if got, err := a.Deliver(context.Background(), se, "now", "steer", "›\n", true); err != nil || got.State != "steered" {
		t.Fatalf("steer result=%+v err=%v", got, err)
	}
	log := mustRead(t, logPath)
	if !strings.Contains(log, "--text later --primary") || !strings.Contains(log, "--named-key Tab --primary") ||
		!strings.Contains(log, "--text now --primary") || !strings.Contains(log, "--enter --primary") || strings.Count(log, "--primary") != 4 {
		t.Fatalf("wrong key sequence:\n%s", log)
	}
	before := log
	if _, err := a.Deliver(context.Background(), se, "merge", "auto", "› laptop draft\n", true); err == nil {
		t.Fatal("draft conflict was accepted")
	}
	if after := mustRead(t, logPath); after != before {
		t.Fatalf("draft conflict emitted keys: %q", strings.TrimPrefix(after, before))
	}
}

func TestCodexRequestCorrelationUsesTaskBoundary(t *testing.T) {
	tracker := newCodexRequestTracker()
	tracker.add("s", codexPendingRequest{RequestID: "r1", Text: "same", Baseline: 2})
	snap := harnessSnapshot{Messages: []histMsg{
		{Role: "user", Content: "same", TurnID: "old", Seq: 1},
		{Role: "user", Content: "same", TurnID: "new", Seq: 3},
		{Role: "commentary", Content: "still working", TurnID: "new", Seq: 4},
		{Role: "reply", Content: "wrong", TurnID: "old", Seq: 5},
		{Role: "reply", Content: "right", TurnID: "new", Seq: 6},
	}}
	replies := tracker.reconcile("s", snap)
	if len(replies) != 1 || replies[0].RequestID != "r1" || replies[0].Message.Content != "right" {
		t.Fatalf("correlation = %+v", replies)
	}
}

func TestAttentionWatcherDeduplicatesAndRearms(t *testing.T) {
	sw := &sessWatch{}
	info := attentionInfo{Pending: true, ID: "q1", Kind: "question"}
	if stepAttentionNotification(sw, true, info) {
		t.Fatal("first-sight attention should be baselined")
	}
	if stepAttentionNotification(sw, false, info) {
		t.Fatal("same attention id notified twice")
	}
	if !stepAttentionNotification(sw, false, attentionInfo{Pending: true, ID: "q2", Kind: "question"}) {
		t.Fatal("new attention id was not notified")
	}
	_ = stepAttentionNotification(sw, false, attentionInfo{})
	if !stepAttentionNotification(sw, false, info) {
		t.Fatal("cleared attention did not rearm notification")
	}
}

func testServer(t *testing.T, cfg config) *server {
	t.Helper()
	if cfg.profile == "" {
		cfg.profile = "default"
	}
	u, _ := url.Parse("http://127.0.0.1:1")
	return newServer(cfg, u)
}

func writeRollout(t *testing.T, home, filenameID, metadataID string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "07", "10")
	mustMkdir(t, dir)
	path := filepath.Join(dir, "rollout-2026-07-10T00-00-00-"+filenameID+".jsonl")
	line := fmt.Sprintf("{\"timestamp\":\"2026-07-10T00:00:00Z\",\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cli_version\":\"0.144.0\"}}\n", metadataID)
	mustWrite(t, path, line)
	return path
}

func mustLines(t *testing.T, path string) []string {
	t.Helper()
	return strings.Split(strings.TrimSpace(mustRead(t, path)), "\n")
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
