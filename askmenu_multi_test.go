package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func multiAsk() pendingAsk {
	ask := soloAsk()
	ask.Questions[0].Header, ask.Questions[0].Question, ask.Questions[0].MultiSelect = "Multi", "Pick several?", true
	return ask
}

func TestMultiSelectKeys(t *testing.T) {
	ask := multiAsk()
	for _, tt := range []struct {
		name string
		pick askPick
		want string
		bad  bool
	}{
		{"one", askPick{Options: []int{1}}, "text:1|key:Right", false},
		{"several", askPick{Options: []int{1, 3}}, "text:1|text:3|key:Right", false},
		{"none", askPick{}, "", true},
		{"negative", askPick{Options: []int{-1}}, "", true},
		{"zero", askPick{Options: []int{0}}, "", true},
		{"out of range", askPick{Options: []int{4}}, "", true},
		{"duplicate", askPick{Options: []int{1, 1}}, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ap, _ := parseAskPane(claudeFixture(t, "askq-multi-single-pane.txt"))
			keys, err := multiSelectKeys(ask, 0, ap, tt.pick)
			if (err != nil) != tt.bad {
				t.Fatalf("keys=%+v err=%v", keys, err)
			}
			var got []string
			for _, k := range keys {
				if k.Named != "" {
					got = append(got, "key:"+k.Named)
				} else {
					got = append(got, "text:"+k.Text)
				}
			}
			if strings.Join(got, "|") != tt.want {
				t.Fatalf("keys=%v want=%s", got, tt.want)
			}
		})
	}
	t.Run("stale rows", func(t *testing.T) {
		ap, _ := parseAskPane(claudeFixture(t, "askq-multi-single-pane.txt"))
		ap.Options[0] = "Different"
		keys, err := multiSelectKeys(ask, 0, ap, askPick{Options: []int{1}})
		if !errors.Is(err, errAskChanged) || len(keys) != 0 {
			t.Fatalf("keys=%v err=%v", keys, err)
		}
	})
	t.Run("already selected", func(t *testing.T) {
		s := newAskSim(ask)
		s.toggleMulti(1)
		s.toggleMulti(3)
		ap, _ := parseAskPane(s.render())
		keys, err := multiSelectKeys(ask, 0, ap, askPick{Options: []int{1, 3}})
		if err != nil || len(keys) != 1 || keys[0].Named != "Right" {
			t.Fatalf("keys=%v err=%v", keys, err)
		}
	})
}

func TestMultiSelectCapturedTransitions(t *testing.T) {
	for _, tt := range []struct {
		file            string
		checked         []bool
		cursor          int
		editing, review bool
	}{
		{"askq-multi-toggled.txt", []bool{true, false, false, false}, 0, false, false},
		{"askq-multi-digit.txt", []bool{true, true, true, false}, 1, false, false},
		{"askq-multi-other-editing.txt", []bool{true, true, true, true}, 3, true, false},
		{"askq-multi-next.txt", []bool{true, true, true, true}, 4, false, false},
		{"askq-multi-empty.txt", []bool{false, false, false, false}, 0, false, false},
		{"askq-multi-review.txt", nil, -1, false, true},
		{"askq-multi-single-review.txt", nil, -1, false, true},
		{"askq-multi-other-auto.txt", []bool{false, true, false, true}, 3, true, false},
	} {
		t.Run(tt.file, func(t *testing.T) {
			ap, ok := parseAskPane(claudeFixture(t, tt.file))
			if !ok || ap.Cursor != tt.cursor || ap.Editing != tt.editing || ap.Review != tt.review || len(ap.Checked) != len(tt.checked) {
				t.Fatalf("pane=%+v ok=%v", ap, ok)
			}
			for i := range tt.checked {
				if ap.Checked[i] != tt.checked[i] {
					t.Fatalf("checked=%v", ap.Checked)
				}
			}
		})
	}
}

func TestMultiSelectAnswerWalk(t *testing.T) {
	for _, tt := range []struct {
		name  string
		pick  askPick
		prior bool
		want  string
	}{
		{"one", askPick{Options: []int{1}}, false, "text:1|key:Right|text:1"},
		{"several", askPick{Options: []int{1, 3}}, false, "text:1|text:3|key:Right|text:1"},
		{"replace checked", askPick{Options: []int{1, 3}}, true, "text:1|text:2|key:Right|text:1"},
		{"other", askPick{Text: "custom"}, false, "key:Down|key:Down|key:Down|text:custom|key:Up|key:Right|text:1"},
		{"other and option", askPick{Options: []int{2}, Text: "custom"}, false, "text:2|key:Down|key:Down|key:Down|text:custom|key:Up|key:Right|text:1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ask := multiAsk()
			s := newAskSim(ask)
			if tt.prior {
				s.toggleMulti(3)
				s.toggleMulti(2)
			}
			done, cleared, err := answerAsk(context.Background(), s.io(), ask, []askPick{tt.pick})
			if err != nil || !cleared || done != 1 || strings.Join(s.keys, "|") != tt.want {
				t.Fatalf("done=%d cleared=%v err=%v keys=%v", done, cleared, err, s.keys)
			}
		})
	}
	t.Run("mixed question tabs", func(t *testing.T) {
		ask := multiAsk()
		ask.Questions = append(ask.Questions, soloAsk().Questions...)
		s := newAskSim(ask)
		done, cleared, err := answerAsk(context.Background(), s.io(), ask, []askPick{{Options: []int{2, 3}}, {Option: 1}})
		if err != nil || !cleared || done != 2 {
			t.Fatalf("done=%d cleared=%v err=%v keys=%v", done, cleared, err, s.keys)
		}
	})
	t.Run("replace Other text", func(t *testing.T) {
		ask := multiAsk()
		s := newAskSim(ask)
		s.freeText[0] = "old"
		s.toggleMulti(4)
		_, cleared, err := answerAsk(context.Background(), s.io(), ask, []askPick{{Text: "new"}})
		if err != nil || !cleared || !strings.Contains(strings.Join(s.keys, "|"), "key:BSpace|key:BSpace|key:BSpace|text:new") {
			t.Fatalf("cleared=%v err=%v keys=%v", cleared, err, s.keys)
		}
	})
}

func TestMultiSelectStaleState(t *testing.T) {
	ask := multiAsk()
	for _, mode := range []string{"card", "before first key", "between keys", "review"} {
		t.Run(mode, func(t *testing.T) {
			s := newAskSim(ask)
			io := s.io()
			ap, _ := parseAskPane(s.render())
			io.initialState = ap.state()
			switch mode {
			case "card":
				s.toggleMulti(2)
			case "before first key":
				s.paneHook = func(s *askSim, n int) {
					if n == 2 {
						s.toggleMulti(2)
					}
				}
			case "between keys":
				changed := false
				s.paneHook = func(s *askSim, _ int) {
					if len(s.keys) == 1 && !changed {
						changed = true
						s.toggleMulti(2)
					}
				}
			case "review":
				s.review = map[int]string{0: "One, Two"}
			}
			_, _, err := answerAsk(context.Background(), io, ask, []askPick{{Options: []int{1, 3}}})
			if !errors.Is(err, errAskChanged) || s.gone() {
				t.Fatalf("err=%v keys=%v", err, s.keys)
			}
			if (mode == "card" || mode == "before first key") && len(s.keys) != 0 || mode == "between keys" && len(s.keys) != 1 {
				t.Fatalf("unexpected keys=%v", s.keys)
			}
		})
	}
}

func TestMultiSelectPayloadAndReview(t *testing.T) {
	ask := multiAsk()
	picks, err := askPicks(ask, []attentionAnswer{{ID: "q0", Options: []int{3, 1}, Answer: "custom words"}})
	if err != nil || len(picks[0].Options) != 2 || picks[0].Text != "custom words" {
		t.Fatalf("picks=%v err=%v", picks, err)
	}
	for _, answer := range []attentionAnswer{{ID: "q0"}, {ID: "q0", Options: []int{4}}, {ID: "q0", Options: []int{1, 1}}} {
		if _, err := askPicks(ask, []attentionAnswer{answer}); err == nil {
			t.Fatalf("accepted invalid answer %+v", answer)
		}
	}
	q := ask.Questions[0]
	q.Options[0].Label = "One, with comma"
	p := askPick{Options: []int{1, 3}, Text: "custom, words"}
	if !multiReviewMatches("Three, One, with comma, custom, words", q, p) || multiReviewMatches("One with comma, Three, custom, words", q, p) || multiReviewMatches("Three, custom, words", q, p) {
		t.Fatal("review failed exact order-independent comparison")
	}
}
