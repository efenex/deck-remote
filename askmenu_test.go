package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Fixtures: testdata/claude/askq-tabbed-pane.txt is the live pane of ga-rdb2 (2026-10-21 15:27Z,
// a 3-question tabbed menu that deck-remote showed no card for); askq-pending.jsonl is its
// transcript record (the AskUserQuestion tool_use, no tool_result yet); askq-answered.jsonl the
// same followed by the tool_result.

func askFixturePane(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/claude/askq-tabbed-pane.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func askFixtureAsk(t *testing.T) pendingAsk {
	t.Helper()
	ask, ok := pendingAskFromTranscript("testdata/claude/askq-pending.jsonl")
	if !ok {
		t.Fatal("the pending AskUserQuestion was not found in the transcript fixture")
	}
	return ask
}

// The 2026-10-21 bug: the tab row starts with "←", the header regex wanted a leading ☐/☒/✔, so no
// card ever appeared. The transcript supplies all three questions.
func TestTabbedMenuCardFromFixtures(t *testing.T) {
	pane := askFixturePane(t)
	if info, ok := claudeQuestionInfo(pane); !ok || !info.ReadOnly || info.Questions[0].Header != "Question" {
		t.Errorf("pane-only fallback must now see the tabbed page (read-only): %+v ok=%v", info, ok)
	}
	ask := askFixtureAsk(t)
	if ask.ToolUseID != "toolu_018wyfyM59T6cyD6GSftuUeb" || len(ask.Questions) != 3 {
		t.Fatalf("ask = %+v", ask)
	}
	ap, ok := parseAskPane(pane)
	if !ok || len(ap.Tabs) != 3 || ap.Tabs[1].Header != "ESO comment" || ap.Tabs[0].Answered {
		t.Fatalf("pane = %+v ok=%v", ap, ok)
	}
	if f := ap.focus(ask); f != 0 {
		t.Errorf("focus = %d, want 0 (Leaf rule shows)", f)
	}
	if !ap.optionsMatch(ask.Questions[0]) || len(ap.Options) != 4 || ap.Options[2] != "Type something." {
		t.Errorf("options = %q", ap.Options)
	}
	info, ok := askMenuInfo(pane, ask)
	if !ok || !info.Pending || info.Kind != "question" || info.ReadOnly || strings.Join(info.Actions, ",") != "answer" {
		t.Fatalf("card = %+v ok=%v", info, ok)
	}
	if info.ID != "claude-askq-toolu_018wyfyM59T6cyD6GSftuUeb" || len(info.Questions) != 3 {
		t.Errorf("id/questions = %q / %d", info.ID, len(info.Questions))
	}
	q := info.Questions[2]
	if q.ID != "q2" || q.Header != "Other gates" || len(q.Options) != 2 || q.Options[0].Label != "Ship secret side, file rows" ||
		!strings.Contains(q.Options[0].Description, "narrow second refute") {
		t.Errorf("question 3 = %+v", q)
	}
	if !strings.Contains(info.Questions[0].Options[0].Label, "(Rec.)") {
		t.Errorf("the recommendation must stay in the label: %+v", info.Questions[0].Options[0])
	}
}

// Flipping tabs in the terminal must not mint a new request (the id is the tool_use id, so there is
// no re-push); an answered tool_use is no longer pending.
func TestPendingAskLifecycle(t *testing.T) {
	if _, ok := pendingAskFromTranscript("testdata/claude/askq-answered.jsonl"); ok {
		t.Error("an AskUserQuestion with a tool_result is not pending")
	}
	if _, ok := pendingAskFromTranscript("testdata/claude/no-such.jsonl"); ok {
		t.Error("a missing transcript has no pending question")
	}
	ask := askFixtureAsk(t)
	pane2 := newAskSim(ask).pressDigit(1).render()
	a, _ := askMenuInfo(askFixturePane(t), ask)
	b, ok := askMenuInfo(pane2, ask)
	if !ok || a.ID != b.ID {
		t.Errorf("ids differ across tabs: %q vs %q (ok=%v)", a.ID, b.ID, ok)
	}
}

// The adapter wires the transcript into Attention (transcript found via the list's claude id).
func TestAttentionUsesTranscript(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir := filepath.Join(cfg, "projects", "-work-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile("testdata/claude/askq-pending.jsonl")
	if err := os.WriteFile(filepath.Join(dir, "cid-1.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, config{token: "tok"})
	se := sessionInfo{ID: "s1", Tool: "claude", Path: "/work/proj", ClaudeSessionID: "cid-1"}
	info, err := claudeAdapter{s}.Attention(context.Background(), se, askFixturePane(t), true)
	if err != nil || len(info.Questions) != 3 || info.ReadOnly {
		t.Fatalf("attention = %+v, %v", info, err)
	}
	// without a transcript the page is still surfaced, read-only
	info, _ = claudeAdapter{s}.Attention(context.Background(), sessionInfo{ID: "s2", Tool: "claude"}, askFixturePane(t), true)
	if !info.Pending || !info.ReadOnly || len(info.Questions) != 1 {
		t.Errorf("fallback = %+v", info)
	}
}

// --- a fake TUI built from the real formats observed on a throwaway claude session ---------

type askSim struct {
	multi   [][]int // selected rows in toggle order (as observed in Claude's review)
	ask     pendingAsk
	focus   int // 0..N-1 questions, N review, N+1 gone
	answers []string
	editing bool
	typed   string
	cursor  int
	// freeText is what each question's free-text row holds once typed into (the TUI keeps it).
	freeText map[int]string
	keys     []string
	// hook runs before each key is applied; tests use it to change the menu under the sender.
	hook func(s *askSim, n int)
	// paneHook runs on each capture (1-based count): the menu changes between two reads.
	paneHook func(s *askSim, captures int)
	captures int
	steps    []askProgress
	// review overrides what the review screen shows for a question (index → text).
	review map[int]string
	// single draws a lone question without tab row or review screen; any selection submits it.
	single bool
	// lag models a slow host: a key's effect shows on screen only after this many more captures.
	lag   int
	queue []laggedKey
	// capDelay is how long each capture takes (a loaded host).
	capDelay time.Duration
	// failKey, when set, is asked before a key is delivered; true drops it with an error (the
	// send-keys process was killed with the request).
	failKey func(n int) error
	sent    int
}

type laggedKey struct {
	due   int
	apply func()
}

// deliver applies a key now, or after lag captures. A dropped key never reaches the screen.
func (s *askSim) deliver(apply func()) error {
	s.sent++
	if s.failKey != nil {
		if err := s.failKey(s.sent); err != nil {
			return err
		}
	}
	if s.lag == 0 {
		apply()
		return nil
	}
	s.queue = append(s.queue, laggedKey{due: s.captures + s.lag, apply: apply})
	return nil
}

func (s *askSim) flush() {
	for len(s.queue) > 0 && s.queue[0].due <= s.captures {
		s.queue[0].apply()
		s.queue = s.queue[1:]
	}
}

func newAskSim(ask pendingAsk) *askSim {
	return &askSim{ask: ask, multi: make([][]int, len(ask.Questions)), answers: make([]string, len(ask.Questions)), freeText: map[int]string{}}
}

func (s *askSim) gone() bool { return s.focus > len(s.ask.Questions) }

func (s *askSim) render() string {
	if s.gone() {
		return "❯ \n  ⏵⏵ auto mode on (shift+tab to cycle)\n"
	}
	var b strings.Builder
	if s.single {
		b.WriteString("⏺ earlier output\n──────────────────────────────────────────\n ☐ " + s.ask.Questions[0].Header + " \n")
	} else {
		b.WriteString("⏺ earlier output\n──────────────────────────────────────────\n←")
	}
	for i, q := range s.ask.Questions {
		if s.single {
			break
		}
		box := "☐"
		if s.answers[i] != "" {
			box = "☒"
		}
		b.WriteString("  " + box + " " + q.Header)
	}
	if !s.single {
		b.WriteString("  ✔ Submit  →\n\n")
	}
	n := len(s.ask.Questions)
	if s.focus == n {
		b.WriteString("Review your answers\n")
		for i, q := range s.ask.Questions {
			ans := s.answers[i]
			if o, ok := s.review[i]; ok {
				ans = o
			}
			fmt.Fprintf(&b, " ● %s\n   → %s\n", q.Question, ans)
		}
		b.WriteString("Ready to submit your answers?\n❯ 1. Submit answers\n  2. Cancel\n")
		return b.String()
	}
	q := s.ask.Questions[s.focus]
	b.WriteString("│ " + q.Question + "\n\n")
	for i, o := range q.Options {
		label := o.Label
		if q.MultiSelect {
			label = s.multiBox(i+1) + label
		}
		mark := ""
		if !q.MultiSelect && s.answers[s.focus] == o.Label {
			mark = " ✔"
		}
		cur := "  "
		if i == s.cursor {
			cur = "❯ "
		}
		fmt.Fprintf(&b, "%s%d. %s%s\n     %s\n", cur, i+1, label, mark, o.Description)
	}
	free := "Type something."
	if t := s.freeText[s.focus]; t != "" {
		free = t
		if !q.MultiSelect && s.answers[s.focus] == t {
			free += " ✔"
		}
	}
	if s.editing && s.typed != "" { // an empty field still shows the placeholder (observed live)
		free = s.typed
	}
	fcur := "  "
	if s.cursor == len(q.Options) {
		fcur = "❯ "
	}
	if q.MultiSelect {
		if free == "Type something." {
			free = "Type something"
		}
		free = s.multiBox(len(q.Options)+1) + free
	}
	fmt.Fprintf(&b, "%s%d. %s\n", fcur, len(q.Options)+1, free)
	if q.MultiSelect {
		cur, label := "     ", "Next"
		if s.cursor == len(q.Options)+1 {
			cur = "❯    "
		}
		if s.focus == n-1 {
			label = "Submit"
		}
		fmt.Fprintf(&b, "%s%s\n", cur, label)
	}
	fmt.Fprintf(&b, "──────────────────────────────────────────\n  %d. Chat about this\n\n", len(q.Options)+2)
	if s.editing {
		b.WriteString("Enter to select · Tab/Arrow keys to navigate · ctrl+g to edit in Vim · Esc to cancel\n")
	} else {
		b.WriteString("Enter to select · Tab/Arrow keys to navigate · Esc to cancel\n")
	}
	b.WriteString("──────────────────────────────────────────────── sess ─\n")
	return b.String()
}

func (s *askSim) pressDigit(d int) *askSim { s.sendText(fmt.Sprint(d)); return s }

func (s *askSim) before() {
	if s.hook != nil {
		s.hook(s, len(s.keys))
	}
}

func (s *askSim) sendText(text string) {
	s.before()
	s.keys = append(s.keys, "text:"+text)
	n := len(s.ask.Questions)
	switch {
	case s.gone():
	case s.editing:
		s.typed += text
		if s.ask.Questions[s.focus].MultiSelect {
			s.multiText()
		}
	case s.focus == n:
		if text == "1" {
			s.focus = n + 1
		}
	default:
		q := s.ask.Questions[s.focus]
		var d int
		fmt.Sscanf(text, "%d", &d)
		switch {
		case q.MultiSelect && d >= 1 && d <= len(q.Options)+1:
			s.toggleMulti(d)
		case d >= 1 && d <= len(q.Options):
			s.answers[s.focus] = q.Options[d-1].Label
			s.advance()
		case d == len(q.Options)+1 && s.freeText[s.focus] != "":
			s.answers[s.focus] = s.freeText[s.focus] // a filled row is selected, not edited
			s.advance()
		case d == len(q.Options)+1:
			s.editing, s.typed, s.cursor = true, "", len(q.Options)
		}
	}
}

func (s *askSim) sendEnter() {
	s.before()
	s.keys = append(s.keys, "enter")
	if s.editing {
		s.freeText[s.focus], s.answers[s.focus], s.editing = s.typed, s.typed, false
		s.advance()
	}
}

func (s *askSim) advance() {
	s.focus++
	s.cursor = 0
	if s.single {
		s.focus = len(s.ask.Questions) + 1 // no review screen: the selection submitted it
	}
}

func (s *askSim) sendNamed(k string) {
	s.before()
	s.keys = append(s.keys, "key:"+k)
	n := len(s.ask.Questions)
	switch {
	case s.gone() || s.focus >= n:
		if k == "Left" && s.focus == n {
			s.focus--
			s.cursor = 0
		}
	case k == "Left" && s.focus > 0 && !s.editing:
		s.focus--
		s.cursor = 0
	case k == "Right" && !s.editing:
		s.advance()
	case k == "Up" && s.cursor > 0:
		s.cursor--
		s.editing = s.cursor == len(s.ask.Questions[s.focus].Options)
	case k == "Down" && s.cursor <= len(s.ask.Questions[s.focus].Options):
		s.cursor++
		if s.cursor == len(s.ask.Questions[s.focus].Options) {
			s.editing, s.typed = true, s.freeText[s.focus]
		} else {
			s.editing = false
		}
	case k == "BSpace" && s.editing && s.typed != "":
		r := []rune(s.typed)
		s.typed = string(r[:len(r)-1])
		if s.ask.Questions[s.focus].MultiSelect {
			s.multiText()
		}
	}
}

func (s *askSim) multiBox(row int) string {
	if slices.Contains(s.multi[s.focus], row) {
		return "[✔] "
	}
	return "[ ] "
}

func (s *askSim) toggleMulti(row int) {
	rows := s.multi[s.focus]
	if i := slices.Index(rows, row); i >= 0 {
		rows = slices.Delete(rows, i, i+1)
	} else {
		rows = append(rows, row)
	}
	s.multi[s.focus] = rows
	s.multiAnswer()
}

func (s *askSim) multiText() {
	s.freeText[s.focus] = s.typed
	row := len(s.ask.Questions[s.focus].Options) + 1
	if slices.Contains(s.multi[s.focus], row) != (s.typed != "") {
		s.toggleMulti(row)
	}
	s.multiAnswer()
}

func (s *askSim) multiAnswer() {
	var labels []string
	q := s.ask.Questions[s.focus]
	for _, row := range s.multi[s.focus] {
		if row <= len(q.Options) {
			labels = append(labels, q.Options[row-1].Label)
		} else if s.freeText[s.focus] != "" {
			labels = append(labels, s.freeText[s.focus])
		}
	}
	s.answers[s.focus] = strings.Join(labels, ", ")
}

func (s *askSim) io() askIO {
	return askIO{
		pane: func(context.Context) (string, error) {
			s.captures++
			if s.capDelay > 0 {
				time.Sleep(s.capDelay)
			}
			s.flush()
			if s.paneHook != nil {
				s.paneHook(s, s.captures)
			}
			return s.render(), nil
		},
		text:  func(_ context.Context, t string) error { return s.deliver(func() { s.sendText(t) }) },
		named: func(_ context.Context, k string) error { return s.deliver(func() { s.sendNamed(k) }) },
		enter: func(context.Context) error { return s.deliver(s.sendEnter) },
		wait:  time.Millisecond, polls: 3,
		progress: func(p askProgress) { s.steps = append(s.steps, p) },
	}
}

func TestSimRendersParsable(t *testing.T) {
	ask := askFixtureAsk(t)
	s := newAskSim(ask)
	for i := range ask.Questions {
		ap, ok := parseAskPane(s.render())
		if !ok || ap.focus(ask) != i || !ap.headersMatch(ask) || !ap.optionsMatch(ask.Questions[i]) {
			t.Fatalf("page %d: %+v ok=%v", i, ap, ok)
		}
		s.pressDigit(1)
	}
	ap, ok := parseAskPane(s.render())
	if !ok || !ap.Review || len(ap.Answers) != 3 || ap.focus(ask) != 3 {
		t.Fatalf("review: %+v ok=%v", ap, ok)
	}
}

func TestAnswersMapToExactKeys(t *testing.T) {
	ask := askFixtureAsk(t)
	picks, err := askPicks(ask, []attentionAnswer{
		{ID: "q0", Answer: ask.Questions[0].Options[0].Label}, // the (Rec.) row
		{ID: "q2", Answer: ask.Questions[2].Options[1].Label},
		{ID: "q1", Answer: "Revert, but  keep\nthe note"}, // free text, one line
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newAskSim(ask)
	done, cleared, err := answerAsk(context.Background(), s.io(), ask, picks)
	if err != nil || done != 3 || !cleared {
		t.Fatalf("done=%d cleared=%v err=%v", done, cleared, err)
	}
	want := []string{"text:1", "text:3", "text:Revert, but keep the note", "enter", "text:2", "text:1"}
	if strings.Join(s.keys, "|") != strings.Join(want, "|") {
		t.Errorf("keys = %q\nwant   %q", s.keys, want)
	}
	if s.answers[0] != ask.Questions[0].Options[0].Label || s.answers[1] != "Revert, but keep the note" {
		t.Errorf("answers = %q", s.answers)
	}
}

// From a later tab the run first goes back to tab 0 when the tabs before it are not all answered
// (the operator only flipped around in the terminal).
func TestAnswerFromAnotherTab(t *testing.T) {
	ask := askFixtureAsk(t)
	s := newAskSim(ask)
	s.focus = 2 // Other gates showing, nothing answered yet
	picks := []askPick{{Option: 1}, {Option: 1}, {Option: 1}}
	if _, _, err := answerAsk(context.Background(), s.io(), ask, picks); err != nil {
		t.Fatal(err)
	}
	want := []string{"key:Left", "key:Left", "text:1", "text:1", "text:1", "text:1"}
	if strings.Join(s.keys, "|") != strings.Join(want, "|") {
		t.Errorf("keys = %q, want %q", s.keys, want)
	}
}

func TestAnswerValidation(t *testing.T) {
	ask := askFixtureAsk(t)
	for name, answers := range map[string][]attentionAnswer{
		"missing": {{ID: "q0", Answer: "x"}},
		"unknown": {{ID: "q0", Answer: "x"}, {ID: "q1", Answer: "x"}, {ID: "q7", Answer: "x"}},
		"repeat":  {{ID: "q0", Answer: "x"}, {ID: "q0", Answer: "x"}, {ID: "q1", Answer: "x"}},
		"empty":   {{ID: "q0", Answer: " "}, {ID: "q1", Answer: "x"}, {ID: "q2", Answer: "x"}},
	} {
		if _, err := askPicks(ask, answers); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Changed-menu refusal: whatever moves under the sender, no further key goes out.
func TestChangedMenuRefused(t *testing.T) {
	ask := askFixtureAsk(t)
	picks := []askPick{{Option: 1}, {Option: 1}, {Option: 1}}

	t.Run("cleared before the first key", func(t *testing.T) {
		s := newAskSim(ask)
		s.focus = 4
		_, _, err := answerAsk(context.Background(), s.io(), ask, picks)
		if !errors.Is(err, errAskChanged) || len(s.keys) != 0 {
			t.Errorf("err=%v keys=%q", err, s.keys)
		}
	})
	t.Run("a different menu after the first answer", func(t *testing.T) {
		s := newAskSim(ask)
		s.paneHook = func(s *askSim, n int) {
			if n == 3 { // the third capture is the first after key 0
				s.ask = pendingAsk{ToolUseID: "other", Questions: []askQuestion{{Header: "A", Question: "Q?", Options: ask.Questions[0].Options}, {Header: "B", Question: "R?", Options: ask.Questions[0].Options}, {Header: "C", Question: "S?", Options: ask.Questions[0].Options}}}
			}
		}
		_, _, err := answerAsk(context.Background(), s.io(), ask, picks)
		if !errors.Is(err, errAskChanged) || len(s.keys) != 1 {
			t.Errorf("err=%v keys=%q", err, s.keys)
		}
	})
	t.Run("operator typing in the terminal", func(t *testing.T) {
		s := newAskSim(ask)
		s.editing = true
		_, _, err := answerAsk(context.Background(), s.io(), ask, picks)
		if !errors.Is(err, errAskChanged) || len(s.keys) != 0 {
			t.Errorf("err=%v keys=%q", err, s.keys)
		}
	})
	t.Run("a stale card id", func(t *testing.T) {
		other := ask
		other.Questions = ask.Questions[:2]
		s := newAskSim(ask)
		_, _, err := answerAsk(context.Background(), s.io(), other, picks[:2])
		if !errors.Is(err, errAskChanged) || len(s.keys) != 0 {
			t.Errorf("err=%v keys=%q", err, s.keys)
		}
	})
	t.Run("multiSelect requires multiple-choice payload", func(t *testing.T) {
		m := ask
		m.Questions = append([]askQuestion(nil), ask.Questions...)
		m.Questions[1].MultiSelect = true
		s := newAskSim(ask)
		if _, _, err := answerAsk(context.Background(), s.io(), m, picks); err == nil || len(s.keys) != 0 {
			t.Errorf("err=%v keys=%q", err, s.keys)
		}
		if info, ok := askMenuInfo(askFixturePane(t), m); !ok || info.ReadOnly || len(info.Actions) != 1 {
			t.Errorf("a multiSelect menu must be answerable: %+v", info)
		}
	})
}

// The review screen must show exactly what was pressed; otherwise "1" is never sent.
func TestReviewMismatchDoesNotSubmit(t *testing.T) {
	ask := askFixtureAsk(t)
	s := newAskSim(ask)
	s.review = map[int]string{2: "Scout them now too"} // the TUI shows another answer for question 3
	_, _, err := answerAsk(context.Background(), s.io(), ask, []askPick{{Option: 1}, {Option: 1}, {Option: 1}})
	if !errors.Is(err, errAskChanged) || s.gone() || len(s.keys) != 3 {
		t.Errorf("err=%v gone=%v keys=%q", err, s.gone(), s.keys)
	}
}

// A free-text row that still holds earlier text (the TUI keeps it; its digit would only select
// it) is reached with Down, emptied with BSpace and retyped.
func TestFreeTextOverPriorText(t *testing.T) {
	ask := askFixtureAsk(t)
	s := newAskSim(ask)
	s.freeText[1] = "old"
	picks := []askPick{{Option: 2}, {Text: "new text"}, {Option: 1}}
	if _, cleared, err := answerAsk(context.Background(), s.io(), ask, picks); err != nil || !cleared {
		t.Fatalf("cleared=%v err=%v keys=%q", cleared, err, s.keys)
	}
	want := []string{"text:2", "key:Down", "key:Down", "key:BSpace", "key:BSpace", "key:BSpace", "text:new text", "enter", "text:1", "text:1"}
	if strings.Join(s.keys, "|") != strings.Join(want, "|") {
		t.Errorf("keys = %q\nwant   %q", s.keys, want)
	}
	if s.answers[1] != "new text" {
		t.Errorf("answers = %q", s.answers)
	}
}

func TestFreeTextNeedsLettersOrDigits(t *testing.T) {
	ask := askFixtureAsk(t)
	s := newAskSim(ask)
	_, _, err := answerAsk(context.Background(), s.io(), ask, []askPick{{Option: 1}, {Text: "?!"}, {Option: 1}})
	if err == nil || s.gone() {
		t.Errorf("err=%v keys=%q", err, s.keys)
	}
}

// The real review screen (captured from a throwaway session): a markdown question line is drawn
// as "│ ● ..." and must not leak into the answer above it.
func TestReviewScreenFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/claude/askq-review.txt")
	if err != nil {
		t.Fatal(err)
	}
	ap, ok := parseAskPane(string(b))
	if !ok || !ap.Review || len(ap.Answers) != 3 {
		t.Fatalf("review = %+v ok=%v", ap, ok)
	}
	want := []string{"Strict leaf only (Rec.)", "my own words, with: punctuation", "Tests only"}
	for i, w := range want {
		if ap.Answers[i] != w {
			t.Errorf("answer %d = %q, want %q", i, ap.Answers[i], w)
		}
	}
	if ap.Tabs[0].Header != "Leaf rule" || len(ap.Tabs) != 3 || !ap.Tabs[2].Answered {
		t.Errorf("tabs = %+v", ap.Tabs)
	}
}

// A menu in scrollback (anything but the bottom of the screen) is not live.
func TestStaleMenuInScrollbackIgnored(t *testing.T) {
	pane := askFixturePane(t) + strings.Repeat("⏺ more work\n", 30) + "❯ \n"
	if _, ok := parseAskPane(pane); ok {
		t.Error("a menu followed by 30 lines of output read as live")
	}
}
