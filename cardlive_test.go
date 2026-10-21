package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// task deckremote-card-live: single-question menus, progress, the operator tmux lane, live events.
// Fixtures askq-single-*.txt / askq-multi-single-pane.txt are panes of a throwaway claude session
// (2026-10-21), where a digit was seen to select AND submit a lone question.

func soloAsk() pendingAsk {
	opt := func(l, d string) attentionOption { return attentionOption{Label: l, Description: d} }
	return pendingAsk{ToolUseID: "toolu_solo", Questions: []askQuestion{{
		Header: "Solo", Question: "Pick one?",
		Options: []attentionOption{opt("One", "First option"), opt("Two", "Second option"), opt("Three", "Third option")},
	}}}
}

func claudeFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/claude/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSingleQuestionCardIsAnswerable(t *testing.T) {
	pane := claudeFixture(t, "askq-single-pane.txt")
	ap, ok := parseAskPane(pane)
	if !ok || !ap.Single || len(ap.Tabs) != 1 || ap.Tabs[0].Header != "Solo" || ap.Question != "Pick one?" {
		t.Fatalf("pane = %+v ok=%v", ap, ok)
	}
	ask := soloAsk()
	if ap.focus(ask) != 0 || !ap.optionsMatch(ask.Questions[0]) || ap.Cursor != 0 {
		t.Errorf("focus/options/cursor: %+v", ap)
	}
	info, ok := askMenuInfo(pane, ask)
	if !ok || info.ReadOnly || strings.Join(info.Actions, ",") != "answer" || info.ID != "claude-askq-toolu_solo" || len(info.Questions) != 1 {
		t.Fatalf("card = %+v ok=%v", info, ok)
	}
	// a different transcript question is not this menu
	other := soloAsk()
	other.Questions[0].Header = "Other"
	if _, ok := askMenuInfo(pane, other); ok {
		t.Error("a transcript question with another header must not claim the menu")
	}
	// the typed row of the same menu is still parsed, and says it is being edited
	ed, ok := parseAskPane(claudeFixture(t, "askq-single-editing.txt"))
	if !ok || !ed.Editing || !ed.Single || ed.Cursor != 3 || ed.Options[3] != "hello there" {
		t.Errorf("editing pane = %+v ok=%v", ed, ok)
	}
}

func TestMultiSelectIsReadOnlyAtOnceWithReason(t *testing.T) {
	ask := pendingAsk{ToolUseID: "toolu_m", Questions: []askQuestion{{
		Header: "Multi", Question: "Pick several?", MultiSelect: true,
		Options: []attentionOption{{Label: "One"}, {Label: "Two"}, {Label: "Three"}},
	}}}
	info, ok := askMenuInfo(claudeFixture(t, "askq-multi-single-pane.txt"), ask)
	if !ok || !info.ReadOnly || len(info.Actions) != 0 || !strings.Contains(info.ReadOnlyReason, "several choices") {
		t.Fatalf("card = %+v ok=%v", info, ok)
	}
}

func TestSingleQuestionWalkKeys(t *testing.T) {
	ask := soloAsk()
	run := func(answer string) (*askSim, int, bool, error) {
		picks, err := askPicks(ask, []attentionAnswer{{ID: "q0", Answer: answer}})
		if err != nil {
			t.Fatal(err)
		}
		s := newAskSim(ask)
		s.single = true
		done, cleared, err := answerAsk(context.Background(), s.io(), ask, picks)
		return s, done, cleared, err
	}
	s, done, cleared, err := run("Two")
	if err != nil || done != 1 || !cleared || strings.Join(s.keys, "|") != "text:2" || s.answers[0] != "Two" {
		t.Errorf("option: keys=%q done=%d cleared=%v err=%v", s.keys, done, cleared, err)
	}
	s, done, cleared, err = run("my own words")
	if err != nil || done != 1 || !cleared || strings.Join(s.keys, "|") != "text:4|text:my own words|enter" {
		t.Errorf("free text: keys=%q done=%d cleared=%v err=%v", s.keys, done, cleared, err)
	}
	var steps []string
	for _, p := range s.steps {
		steps = append(steps, p.Step)
	}
	if strings.Join(steps, ",") != "checking,answering,submitting" {
		t.Errorf("progress = %v", steps)
	}
}

// If the single question on screen is not the pending one, nothing is typed.
func TestSingleQuestionWalkRefusesAnotherMenu(t *testing.T) {
	ask := soloAsk()
	s := newAskSim(ask)
	s.single = true
	other := soloAsk()
	other.Questions[0].Question = "A completely different question?"
	picks := []askPick{{Option: 1}}
	_, _, err := answerAsk(context.Background(), s.io(), other, picks)
	if !errors.Is(err, errAskChanged) || len(s.keys) != 0 {
		t.Errorf("err=%v keys=%q", err, s.keys)
	}
}

func TestTabbedWalkReportsEachStep(t *testing.T) {
	ask := askFixtureAsk(t)
	s := newAskSim(ask)
	picks := make([]askPick, len(ask.Questions))
	for i := range picks {
		picks[i] = askPick{Option: 1}
	}
	if _, cleared, err := answerAsk(context.Background(), s.io(), ask, picks); err != nil || !cleared {
		t.Fatalf("cleared=%v err=%v", cleared, err)
	}
	var got []string
	for _, p := range s.steps {
		got = append(got, p.Step)
		if p.Step == "answering" && p.Total != 3 {
			t.Errorf("answering step %+v", p)
		}
	}
	if strings.Join(got, ",") != "checking,answering,answering,answering,verifying,submitting" {
		t.Errorf("steps = %v", got)
	}
}

// The operator lane is not queued behind sweep reservations, and still honours a tmux backoff.
func TestOperatorLaneIsNotQueuedBehindTheSweep(t *testing.T) {
	b := newTmuxBudget()
	b.interval = 200 * time.Millisecond
	ctx := context.Background()
	for i := 0; i < 3; i++ { // the sweep keeps its slots busy
		_ = b.acquire(ctx)
	}
	b.mu.Lock()
	before := b.next
	b.mu.Unlock()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := b.acquireOperator(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("3 operator slots took %v behind a busy sweep", d)
	}
	// and it did not move the sweep's slot
	b.mu.Lock()
	after := b.next
	b.mu.Unlock()
	if !after.Equal(before) {
		t.Errorf("the operator lane moved the sweep's slot: %v -> %v", before, after)
	}
	b.fault()
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := b.acquireOperator(cctx); !errors.Is(err, errTmuxThrottled) {
		t.Errorf("operator lane during backoff: %v, want throttled", err)
	}
}

func TestAttentionEventOnlyOnChange(t *testing.T) {
	s := testServer(t, config{token: "tok"})
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	q := attentionInfo{Pending: true, Kind: "question", ID: "a", Questions: make([]attentionQuestion, 2)}
	s.noteAttention("s1", q)
	s.noteAttention("s1", q) // same request: silent
	s.noteAttention("s1", attentionInfo{})
	s.noteAttention("s1", attentionInfo{}) // still none: silent
	var got []string
	for len(ch) > 0 {
		got = append(got, string(<-ch))
	}
	if len(got) != 2 || !strings.Contains(got[0], `"pending":true`) || !strings.Contains(got[0], `"n":2`) || !strings.Contains(got[1], `"pending":false`) {
		t.Errorf("events = %q", got)
	}
}
