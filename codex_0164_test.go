package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const codex164ID = "55555555-5555-4555-8555-555555555555"

func TestCodex0164SyntheticConversation(t *testing.T) {
	snap, err := newCodexRolloutStore().Load(rolloutResolution(codex164ID, fixturePath("normal-0164.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if snap.DegradedReason != "" || snap.LastReply != "Synthetic check passed." || snap.State != "completed" || snap.Activity.Working || snap.Attention.Pending {
		t.Fatalf("snapshot: %+v", snap)
	}
	want := []string{"user:Inspect the synthetic service.", "commentary:Inspecting the synthetic service.", "commentary:Which synthetic colour? Blue or Green.", "peer:Synthetic peer update.", "reply:Synthetic check passed."}
	if len(snap.Messages) != len(want) {
		t.Fatalf("messages: %+v", snap.Messages)
	}
	for i, m := range snap.Messages {
		if m.Role+":"+m.Content != want[i] || m.TurnID != "synthetic-turn" || m.Seq == 0 || m.Ts == 0 {
			t.Fatalf("message %d: %+v", i, m)
		}
	}
	if snap.Messages[3].Origin != "peer:/root/helper" {
		t.Fatal("peer attribution lost")
	}
}

func TestCodex0164IncrementalToolsAndQuestions(t *testing.T) {
	lines := mustLines(t, fixturePath("normal-0164.jsonl"))
	path := filepath.Join(t.TempDir(), "active.jsonl")
	store := newCodexRolloutStore()
	for i, line := range lines {
		mustWrite(t, path, strings.Join(lines[:i+1], "\n")+"\n")
		snap, err := store.Load(rolloutResolution(codex164ID, path))
		if err != nil {
			t.Fatal(err)
		}
		var env codexEnvelope
		_ = json.Unmarshal([]byte(line), &env)
		var p struct {
			Type, Name string
			CallID     string `json:"call_id"`
		}
		_ = json.Unmarshal(env.Payload, &p)
		if p.Type == "custom_tool_call" && p.Name == "exec" && snap.Activity.CurrentTool != "exec" {
			t.Fatalf("tool: %+v", snap.Activity)
		}
		if p.Type == "custom_tool_call_output" && snap.Activity.CurrentTool != "" {
			t.Fatal("finished tool remains active")
		}
		if p.Type == "function_call" && p.Name == "request_user_input" {
			if !snap.Attention.Pending || snap.Attention.ID != "question-sync" || len(snap.Attention.Questions) != 1 || snap.Attention.Questions[0].ID != "target" {
				t.Fatalf("question: %+v", snap.Attention)
			}
		}
		if p.CallID == "question-async" && snap.Attention.Pending {
			t.Fatal("async question was mistaken for an actionable synchronous form")
		}
		if p.Type == "function_call_output" && p.CallID == "question-sync" && snap.Attention.Pending {
			t.Fatal("answered question remains pending")
		}
	}
	snap, _ := store.Load(rolloutResolution(codex164ID, path))
	if len(snap.Messages) != 5 {
		t.Fatalf("incremental reload duplicated messages: %d", len(snap.Messages))
	}
}

func TestCodex0164EitherMessageRepresentationAndResumedHeader(t *testing.T) {
	for _, source := range []string{"response_only", "items_only", "resumed_0143", "resumed_0144"} {
		t.Run(source, func(t *testing.T) {
			var lines []string
			for _, line := range mustLines(t, fixturePath("normal-0164.jsonl")) {
				var e codexEnvelope
				_ = json.Unmarshal([]byte(line), &e)
				var p struct {
					Type string
					Item struct{ Type string }
				}
				_ = json.Unmarshal(e.Payload, &p)
				if source == "response_only" && p.Type == "item_completed" {
					continue
				}
				if source == "items_only" && e.Type == "response_item" && p.Type == "message" {
					continue
				}
				if source == "resumed_0143" {
					line = strings.ReplaceAll(line, "0.164.0", "0.143.0")
				}
				if source == "resumed_0144" {
					line = strings.ReplaceAll(line, "0.164.0", "0.144.0")
				}
				lines = append(lines, line)
			}
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			mustWrite(t, path, strings.Join(lines, "\n")+"\n")
			snap, err := newCodexRolloutStore().Load(rolloutResolution(codex164ID, path))
			if err != nil || snap.DegradedReason != "" || snap.LastReply != "Synthetic check passed." || snap.Messages[0].Content != "Inspect the synthetic service." {
				t.Fatalf("snapshot=%+v err=%v", snap, err)
			}
		})
	}
}

func TestCodexVersionPolicyStableKnownMinorsOnly(t *testing.T) {
	for _, v := range []string{"0.143.0", "0.144.99", "0.164.0", "0.164.12"} {
		if !codexVersionSupported(v) {
			t.Errorf("rejected known family %s", v)
		}
	}
	for _, v := range []string{"", "0.145.0", "0.158.0", "0.165.0", "1.164.0", "0.164.", "0.164.bad", "0.164.0-alpha", "0.164.00"} {
		if codexVersionSupported(v) {
			t.Errorf("accepted unverified %s", v)
		}
	}
}

func TestCodexUnknownVersionReadableButCannotAct(t *testing.T) {
	home := t.TempDir()
	s := testServer(t, config{homeDir: home})
	path := writeRollout(t, filepath.Join(home, ".codex"), codex164ID, codex164ID)
	mustWrite(t, path, strings.ReplaceAll(mustRead(t, fixturePath("normal-0164.jsonl")), "0.164.0", "0.165.0"))
	se := sessionInfo{ID: "synthetic-session", Tool: "codex", CodexSessionID: codex164ID}
	adapter := codexAdapter{s: s}
	messages, err := adapter.History(context.Background(), se)
	if err != nil || len(messages) != 6 || !strings.Contains(messages[5].Content, "0.165.0") || !strings.Contains(messages[5].Content, "read-only") {
		t.Fatalf("history=%+v err=%v", messages, err)
	}
	snap, err := adapter.Snapshot(context.Background(), se, "", false)
	if err != nil || snap.DegradedReason == "" || snap.Activity.Working || snap.Attention.Pending {
		t.Fatalf("unsafe degraded snapshot: %+v %v", snap, err)
	}
	if _, err := adapter.Deliver(context.Background(), se, "must not send", "auto", "", false); err == nil {
		t.Fatal("unverified rollout allowed delivery")
	}
}

func TestCodexMirrorDedupPreservesRepeatedUserInput(t *testing.T) {
	st := newCodexParseState(nil)
	for i := 0; i < 2; i++ {
		m := histMsg{Role: "user", Content: "repeat", TurnID: "turn"}
		st.addMessage(m, "response_item")
		st.addMessage(m, "item_completed")
	}
	if len(st.snapshot.Messages) != 2 {
		t.Fatalf("repeated input count=%d", len(st.snapshot.Messages))
	}
}

func TestCodexUnknownEmptyRolloutStillExplainsDegradation(t *testing.T) {
	home := t.TempDir()
	s := testServer(t, config{homeDir: home})
	path := writeRollout(t, filepath.Join(home, ".codex"), codex164ID, codex164ID)
	mustWrite(t, path, strings.Replace(mustRead(t, path), "0.144.0", "0.165.0", 1))
	messages, err := (codexAdapter{s: s}).History(context.Background(), sessionInfo{ID: "synthetic", Tool: "codex", CodexSessionID: codex164ID})
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Content, "read-only") {
		t.Fatalf("empty unknown rollout: %+v %v", messages, err)
	}
}
