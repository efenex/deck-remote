package main

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

func validateMultiPick(q askQuestion, p askPick) error {
	if p.Option != 0 || (len(p.Options) == 0 && p.Text == "") {
		return fmt.Errorf("select at least one choice for %q", q.Header)
	}
	seen := map[int]bool{}
	for _, row := range p.Options {
		if row < 1 || row > len(q.Options) || seen[row] {
			return fmt.Errorf("invalid or repeated option %d for %q", row, q.Header)
		}
		seen[row] = true
	}
	if len([]rune(p.Text)) > maxEraseRunes || strings.IndexFunc(p.Text, unicode.IsControl) >= 0 {
		return fmt.Errorf("Other answer must be one line of at most %d characters", maxEraseRunes)
	}
	return nil
}

// Each step carries the expected rendered state. The sender checks it before the
// next key, so an operator's cursor move or checkbox toggle stops the walk.
type askMultiKey struct {
	Text  string
	Named string
	After askPane
}

func cloneAskPane(ap askPane) askPane {
	ap.Tabs = append([]askTab(nil), ap.Tabs...)
	ap.Options = append([]string(nil), ap.Options...)
	ap.Checked = append([]bool(nil), ap.Checked...)
	return ap
}

func multiSelectKeys(ask pendingAsk, i int, ap askPane, p askPick) ([]askMultiKey, error) {
	if i < 0 || i >= len(ask.Questions) {
		return nil, askChanged("unknown question")
	}
	q := ask.Questions[i]
	if err := validateMultiPick(q, p); err != nil {
		return nil, err
	}
	if !q.MultiSelect || len(q.Options)+1 > 9 || !ap.headersMatch(ask) || ap.focus(ask) != i ||
		!ap.optionsMatch(q) || ap.Editing || ap.Cursor < 0 || ap.Cursor > len(q.Options)+1 {
		return nil, askChanged("stale or unsupported multi-select menu; refresh the question")
	}
	ap = cloneAskPane(ap)
	var keys []askMultiKey
	add := func(text, named string) {
		ap.Tabs[i].Answered = false
		for _, checked := range ap.Checked {
			ap.Tabs[i].Answered = ap.Tabs[i].Answered || checked
		}
		keys = append(keys, askMultiKey{Text: text, Named: named, After: cloneAskPane(ap)})
	}
	want := make([]bool, len(q.Options)+1)
	for _, row := range p.Options {
		want[row-1] = true
	}
	want[len(q.Options)] = p.Text != ""
	other := len(q.Options)
	for row := range want {
		// Typing Other automatically selects it. Its digit only toggles, never edits.
		if row == other && p.Text != "" && ap.Options[row] != p.Text {
			continue
		}
		if ap.Checked[row] != want[row] {
			ap.Checked[row] = want[row]
			add(fmt.Sprint(row+1), "")
		}
	}
	if p.Text != "" && ap.Options[other] != p.Text {
		prior := ap.Options[other]
		if prior == "Type something" {
			prior = ""
		}
		if len([]rune(prior)) > maxEraseRunes {
			return nil, askChanged("Other already contains a long answer; edit it in the terminal")
		}
		for ap.Cursor != other {
			key := "Down"
			if ap.Cursor > other {
				key = "Up"
				ap.Cursor--
			} else {
				ap.Cursor++
			}
			ap.ContinueFocused = false
			ap.Editing = ap.Cursor == other
			add("", key)
		}
		for r := []rune(prior); len(r) > 0; {
			r = r[:len(r)-1]
			ap.Options[other], ap.Checked[other] = string(r), len(r) > 0
			if len(r) == 0 {
				ap.Options[other] = "Type something"
			}
			add("", "BSpace")
		}
		ap.Options[other], ap.Checked[other] = p.Text, true
		add(p.Text, "")
		// Right edits the text cursor while Other is focused. Up leaves the editor.
		ap.Cursor--
		ap.Editing = false
		add("", "Up")
	}
	// Right reaches the next question or review, including a lone multi-select.
	keys = append(keys, askMultiKey{Named: "Right"})
	return keys, nil
}

func (io askIO) multiSelect(ctx context.Context, ask pendingAsk, i int, ap askPane, p askPick) error {
	keys, err := multiSelectKeys(ask, i, ap, p)
	if err != nil {
		return err
	}
	for j, key := range keys {
		live, err := io.read(ctx, ask)
		if err != nil {
			return err
		}
		if live.state() != ap.state() {
			return askChanged("multi-select state changed; refresh the question")
		}
		if key.Named != "" {
			err = io.named(ctx, key.Named)
		} else {
			err = io.text(ctx, key.Text)
		}
		if err != nil {
			return err
		}
		if j == len(keys)-1 {
			break
		} // caller verifies the next tab/review
		ap = key.After
		ok, err := io.until(ctx, ask, false, func(live askPane) bool { return live.state() == ap.state() })
		if err != nil || !ok {
			return orChanged(err, "multi-select toggle or text did not match; stopped before submitting")
		}
	}
	return nil
}

// Claude reviews multi-select labels in toggle order, not row order. Match all
// chosen labels exactly, with their comma separators, without splitting labels
// that themselves contain commas. At most nine rows are remotely answerable.
func multiReviewMatches(answer string, q askQuestion, p askPick) bool {
	var labels []string
	for _, row := range p.Options {
		labels = append(labels, q.Options[row-1].Label)
	}
	if p.Text != "" {
		labels = append(labels, p.Text)
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for i := range labels {
		labels[i] = norm(labels[i])
	}
	var match func(string, []string) bool
	match = func(rest string, remaining []string) bool {
		for i, label := range remaining {
			if len(remaining) == 1 {
				return rest == label
			}
			if strings.HasPrefix(rest, label+", ") {
				next := append([]string(nil), remaining[:i]...)
				next = append(next, remaining[i+1:]...)
				if match(strings.TrimPrefix(rest, label+", "), next) {
					return true
				}
			}
		}
		return false
	}
	return match(norm(answer), labels)
}
