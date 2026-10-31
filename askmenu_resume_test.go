package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 2026-10-31 17:31Z (tower, 4 single-select questions, load avg ~44): the phone's 12 s fetch
// timeout aborted the request, Go cancelled the walk's context and the 4th digit's send-keys was
// killed: 3 of 4 answers in the menu, "Not sent: Host did not respond within 12 s" on the card.
// These tests pin the server half: a slow host does not fail a walk, and a cut-off walk is finished
// by sending again without answering any tab twice.

func fourQuestionAsk() pendingAsk {
	var ask pendingAsk
	ask.ToolUseID = "toolu_four"
	for i := 1; i <= 4; i++ {
		ask.Questions = append(ask.Questions, askQuestion{
			Header:   fmt.Sprintf("Gate %d", i),
			Question: fmt.Sprintf("Question number %d is which one?", i),
			Options: []attentionOption{
				{Label: fmt.Sprintf("Alpha %d", i), Description: "first"},
				{Label: fmt.Sprintf("Beta %d", i), Description: "second"},
			},
		})
	}
	return ask
}

func pickFirsts(n int) []askPick {
	p := make([]askPick, n)
	for i := range p {
		p[i] = askPick{Option: 1}
	}
	return p
}

// A slow host shows each key only some captures later, and each capture takes time. The walk waits
// on the clock, not on a handful of polls, and finishes all four tabs.
func TestSlowHostFourTabsCompletes(t *testing.T) {
	ask := fourQuestionAsk()
	s := newAskSim(ask)
	s.lag, s.capDelay = 12, 2*time.Millisecond
	io := s.io()
	io.stepTimeout = 5 * time.Second
	done, cleared, err := answerAsk(context.Background(), io, ask, pickFirsts(4))
	if err != nil || done != 4 || !cleared {
		t.Fatalf("done=%d cleared=%v err=%v", done, cleared, err)
	}
	if got := strings.Join(s.keys, "|"); got != "text:1|text:1|text:1|text:1|text:1" {
		t.Errorf("keys = %s", got)
	}
}

// Without the wall-clock budget (the old behaviour: 3 polls per step) the same host fails mid-walk.
func TestSlowHostFailsWithoutClockBudget(t *testing.T) {
	ask := fourQuestionAsk()
	s := newAskSim(ask)
	s.lag = 12
	done, _, err := answerAsk(context.Background(), s.io(), ask, pickFirsts(4))
	if err == nil || done != 1 {
		t.Fatalf("done=%d err=%v; expected the short poll budget to give up after the first key", done, err)
	}
}

// The connection drops while the 4th tab's key is being sent. The menu stands at tab 4 with three
// tabs answered; sending again presses only the 4th answer and the review submits.
func TestCutOffWalkIsFinishedOnRetry(t *testing.T) {
	ask := fourQuestionAsk()
	s := newAskSim(ask)
	ctx, cancel := context.WithCancel(context.Background())
	s.failKey = func(n int) error {
		if n == 4 { // the 4th digit: send-keys killed with the request
			cancel()
			return context.Canceled
		}
		return nil
	}
	done, _, err := answerAsk(ctx, s.io(), ask, pickFirsts(4))
	if !errors.Is(err, context.Canceled) || done != 3 {
		t.Fatalf("first walk: done=%d err=%v", done, err)
	}
	if s.focus != 3 || s.answers[2] == "" || s.answers[3] != "" {
		t.Fatalf("menu after the cut: focus=%d answers=%q", s.focus, s.answers)
	}
	s.keys, s.failKey = nil, nil
	done, cleared, err := answerAsk(context.Background(), s.io(), ask, pickFirsts(4))
	if err != nil || done != 4 || !cleared {
		t.Fatalf("retry: done=%d cleared=%v err=%v", done, cleared, err)
	}
	// exactly the missing answer, then Submit: no Left, no digit for tabs 1-3
	if got := strings.Join(s.keys, "|"); got != "text:1|text:1" {
		t.Errorf("retry keys = %s", got)
	}
	for i, a := range s.answers {
		if a != ask.Questions[i].Options[0].Label {
			t.Errorf("answer %d = %q", i, a)
		}
	}
}

// All four tabs answered and the review showing (the walk died before "1"): retry only submits.
func TestRetryFromReviewOnlySubmits(t *testing.T) {
	ask := fourQuestionAsk()
	s := newAskSim(ask)
	for i := 0; i < 4; i++ {
		s.pressDigit(1)
	}
	s.keys = nil
	done, cleared, err := answerAsk(context.Background(), s.io(), ask, pickFirsts(4))
	if err != nil || done != 4 || !cleared {
		t.Fatalf("done=%d cleared=%v err=%v", done, cleared, err)
	}
	if got := strings.Join(s.keys, "|"); got != "text:1" {
		t.Errorf("keys = %s", got)
	}
}

// Resume never overwrites: if the tabs already entered differ from what the phone sends, the
// review check refuses and nothing is submitted.
func TestRetryWithDifferentEarlierAnswersDoesNotSubmit(t *testing.T) {
	ask := fourQuestionAsk()
	s := newAskSim(ask)
	s.pressDigit(2).pressDigit(2).pressDigit(2) // entered earlier: Beta, Beta, Beta
	s.keys = nil
	picks := pickFirsts(4)
	_, _, err := answerAsk(context.Background(), s.io(), ask, picks)
	if err == nil || !errors.Is(err, errAskChanged) || !strings.Contains(err.Error(), "not the answer sent") {
		t.Fatalf("err = %v", err)
	}
	for _, k := range s.keys {
		if k == "key:Left" {
			t.Errorf("navigated back and re-answered: %q", s.keys)
		}
	}
	if s.gone() || s.focus != 4 {
		t.Errorf("submitted or moved: focus=%d keys=%q", s.focus, s.keys)
	}
}

// A different menu (other tabs) is still refused on a resume.
func TestResumeKeepsDialogIdentity(t *testing.T) {
	ask := fourQuestionAsk()
	other := fourQuestionAsk()
	other.Questions[0].Header = "Elsewhere"
	s := newAskSim(other)
	s.pressDigit(1).pressDigit(1).pressDigit(1)
	s.keys = nil
	_, _, err := answerAsk(context.Background(), s.io(), ask, pickFirsts(4))
	if !errors.Is(err, errAskChanged) || len(s.keys) != 0 {
		t.Fatalf("err=%v keys=%q", err, s.keys)
	}
}

func TestPartialErrorCarriesProgress(t *testing.T) {
	var err error = &askPartialError{Done: 3, Total: 4, Err: askChanged("the menu did not advance")}
	var p *askPartialError
	if !errors.As(err, &p) || p.Done != 3 || p.Total != 4 || !errors.Is(err, errAskChanged) {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), "3 of 4") {
		t.Errorf("message %q", err.Error())
	}
}
