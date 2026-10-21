package main

// Claude's tabbed multi-question AskUserQuestion menu (2026-10-21, ga-rdb2 sat on one for hours
// with no card). The TUI draws ONE question at a time under a tab row
//
//	←  ☐ Leaf rule  ☐ ESO comment  ☐ Other gates  ✔ Submit  →
//
// so the pane alone cannot yield "one section per question". The card therefore comes from the
// transcript (the pending AskUserQuestion tool_use: every question, option and description, and
// an id that stays put while the operator flips tabs) and the pane supplies liveness, the focused
// tab and per-key verification. Key semantics, verified against a throwaway claude session:
//
//   - a digit on a question page selects that option and advances to the next tab (☐ → ☒);
//   - the row after the options ("Type something.") takes the digit len(options)+1; the cursor
//     then sits in an inline text field (footer gains "ctrl+g to edit in Vim"): type, Enter;
//   - after the last question the Submit tab shows "Review your answers" with a "→ answer" line
//     per question and "1. Submit answers / 2. Cancel": "1" submits;
//   - Left/Right/Tab move between tabs, without wrapping at either end.
//
// Nothing is typed unless the live pane still matches the transcript question for question, and
// the review screen must show exactly the answers about to be submitted.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// --- transcript side ---------------------------------------------------------------------------

type askQuestion struct {
	Header      string            `json:"header"`
	Question    string            `json:"question"`
	MultiSelect bool              `json:"multiSelect"`
	Options     []attentionOption `json:"options"`
}

// pendingAsk is an AskUserQuestion tool_use the transcript has no tool_result for yet.
type pendingAsk struct {
	ToolUseID string
	Questions []askQuestion
}

// askTailBytes bounds how much of the transcript tail is read: a pending question is the last
// substantive record, only metadata lines follow it.
const askTailBytes = 2 << 20

type askCacheEntry struct {
	size  int64
	mtime time.Time
	ask   pendingAsk
	ok    bool
}

var askCache = struct {
	mu sync.Mutex
	m  map[string]askCacheEntry
}{m: map[string]askCacheEntry{}}

// pendingAskFromTranscript returns the unanswered AskUserQuestion at the end of a Claude
// transcript. Cached by size+mtime, so a sweep over an unchanged file costs one stat.
func pendingAskFromTranscript(path string) (pendingAsk, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return pendingAsk{}, false
	}
	askCache.mu.Lock()
	e, hit := askCache.m[path]
	askCache.mu.Unlock()
	if hit && e.size == st.Size() && e.mtime.Equal(st.ModTime()) {
		return e.ask, e.ok
	}
	ask, ok := scanPendingAsk(path, st.Size())
	askCache.mu.Lock()
	if len(askCache.m) > 64 {
		askCache.m = map[string]askCacheEntry{}
	}
	askCache.m[path] = askCacheEntry{size: st.Size(), mtime: st.ModTime(), ask: ask, ok: ok}
	askCache.mu.Unlock()
	return ask, ok
}

type askRecord struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type askBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	Input     struct {
		Questions []askQuestion `json:"questions"`
	} `json:"input"`
}

func scanPendingAsk(path string, size int64) (pendingAsk, bool) {
	f, err := os.Open(path)
	if err != nil {
		return pendingAsk{}, false
	}
	defer f.Close()
	skipFirst := false
	if size > askTailBytes {
		if _, err := f.Seek(size-askTailBytes, io.SeekStart); err != nil {
			return pendingAsk{}, false
		}
		skipFirst = true // starts mid-line
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	var cur pendingAsk
	have := false
	for {
		line, err := rd.ReadString('\n')
		if skipFirst {
			skipFirst = false
		} else if strings.Contains(line, `"AskUserQuestion"`) || strings.Contains(line, `"tool_result"`) {
			var rec askRecord
			if json.Unmarshal([]byte(line), &rec) == nil && !rec.IsSidechain {
				var blocks []askBlock
				if json.Unmarshal(rec.Message.Content, &blocks) == nil {
					for _, b := range blocks {
						switch {
						case rec.Type == "assistant" && b.Type == "tool_use" && b.Name == "AskUserQuestion" && len(b.Input.Questions) > 0:
							cur, have = pendingAsk{ToolUseID: b.ID, Questions: b.Input.Questions}, true
						case b.Type == "tool_result" && have && b.ToolUseID == cur.ToolUseID:
							have = false
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return cur, have
}

// --- pane side ---------------------------------------------------------------------------------

var (
	askTabRow         = regexp.MustCompile(`^[\s│]*←\s+(.+?)\s+→\s*$`)
	askReviewAnswer   = regexp.MustCompile(`^\s*→\s*(.*?)\s*$`)
	askReviewQuestion = regexp.MustCompile(`^[\s│]*●`)
	askGlyph          = regexp.MustCompile(`[☐☒✔]`)
)

type askTab struct {
	Header   string
	Answered bool
}

// askPane is what the live pane shows of a tabbed menu.
type askPane struct {
	Tabs     []askTab // the question tabs, without Submit
	Review   bool     // the Submit tab ("Review your answers")
	Editing  bool     // the inline free-text field has the cursor
	Question string   // the displayed question text (question pages)
	Options  []string // numbered rows as displayed, trailing " ✔" removed (question pages)
	Cursor   int      // index in Options of the row with the ❯ cursor, -1 when none
	Answers  []string // "→ answer" lines (review)
}

// alnumKey folds text to lowercase letters and digits, so markdown the TUI renders (backticks,
// bold, bullets) and line wrapping do not matter when comparing pane text to the transcript.
func alnumKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func cleanPaneLines(pane string) []string {
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(stripANSI(pane), "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimRight(l, " "))
		}
	}
	return lines
}

// parseTabRow splits "←  ☐ A b  ☒ C  ✔ Submit  →" into its tabs; ok is false unless the row ends
// in the Submit tab.
func parseTabRow(line string) (tabs []askTab, ok bool) {
	m := askTabRow.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	row := m[1]
	idx := askGlyph.FindAllStringIndex(row, -1)
	if len(idx) < 2 {
		return nil, false
	}
	for i, p := range idx {
		end := len(row)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		glyph := row[p[0]:p[1]]
		name := strings.TrimSpace(row[p[1]:end])
		if i == len(idx)-1 {
			return tabs, glyph == "✔" && name == "Submit"
		}
		tabs = append(tabs, askTab{Header: name, Answered: glyph == "☒"})
	}
	return nil, false
}

// parseAskPane reads the live tabbed menu at the bottom of the pane. ok is false when there is
// no tab row, or it is not the live bottom of the screen (an old menu in scrollback).
func parseAskPane(pane string) (askPane, bool) {
	lines := cleanPaneLines(pane)
	row := -1
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-claudeMenuMax; i-- {
		if _, ok := parseTabRow(lines[i]); ok {
			row = i
			break
		}
	}
	if row < 0 {
		return askPane{}, false
	}
	ap := askPane{Cursor: -1}
	ap.Tabs, _ = parseTabRow(lines[row])
	body := lines[row+1:]
	footer := -1
	for i, l := range body {
		if claudeMenuFooter.MatchString(l) {
			footer = i
			ap.Editing = strings.Contains(strings.ToLower(l), "ctrl+g to edit")
			break
		}
	}
	review := false
	for _, l := range body {
		if strings.Contains(l, "Review your answers") {
			review = true
		}
	}
	switch {
	case review:
		// No footer on the review screen: it ends at "2. Cancel" and must be at the bottom.
		ready := false
		for _, l := range body {
			if strings.Contains(l, "Ready to submit your answers?") {
				ready = true
			}
		}
		if !ready || len(body) > claudeMenuMax || !lastRowsInclude(body, "Submit answers") {
			return askPane{}, false
		}
		ap.Review = true
		inAnswers, inQuestion := false, false
		for _, l := range body {
			switch {
			case strings.Contains(l, "Ready to submit your answers?"):
				inAnswers = false
			case strings.Contains(l, "Review your answers"):
				inAnswers = true
			case !inAnswers:
			case askReviewQuestion.MatchString(l): // "● question", possibly "│ ● ..." and wrapped
				inQuestion = true
			case askReviewAnswer.MatchString(l):
				inQuestion = false
				ap.Answers = append(ap.Answers, askReviewAnswer.FindStringSubmatch(l)[1])
			case !inQuestion && len(ap.Answers) > 0: // wrapped answer
				ap.Answers[len(ap.Answers)-1] += " " + strings.TrimSpace(l)
			}
		}
		return ap, true
	case footer < 0 || footer < len(body)-claudeMenuTail:
		return askPane{}, false
	}
	var text []string
	for _, l := range body[:footer] {
		if claudeMenuRule.MatchString(l) {
			continue
		}
		if m := claudeMenuOption.FindStringSubmatch(l); m != nil {
			if strings.Contains(strings.SplitN(l, m[1]+".", 2)[0], "❯") {
				ap.Cursor = len(ap.Options)
			}
			ap.Options = append(ap.Options, strings.TrimSpace(strings.TrimSuffix(m[2], "✔")))
			continue
		}
		clean := strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│"))
		if clean != "" && len(ap.Options) == 0 {
			text = append(text, clean)
		}
	}
	ap.Question = strings.Join(text, " ")
	return ap, len(ap.Options) >= 2
}

func lastRowsInclude(body []string, s string) bool {
	for i := len(body) - 1; i >= 0 && i >= len(body)-6; i-- {
		if strings.Contains(body[i], s) {
			return true
		}
	}
	return false
}

// headersMatch: the tab row is this transcript question set, in order.
func (ap askPane) headersMatch(ask pendingAsk) bool {
	if len(ap.Tabs) != len(ask.Questions) {
		return false
	}
	for i, q := range ask.Questions {
		if alnumKey(ap.Tabs[i].Header) != alnumKey(q.Header) {
			return false
		}
	}
	return true
}

// focus is the page shown: 0..N-1 a question, N the review screen, -1 unknown or ambiguous.
func (ap askPane) focus(ask pendingAsk) int {
	if ap.Review {
		return len(ask.Questions)
	}
	key := alnumKey(ap.Question)
	if key == "" {
		return -1
	}
	hit := -1
	for i, q := range ask.Questions {
		qk := alnumKey(q.Question)
		if qk == key || (len(key) > 24 && (strings.HasPrefix(qk, key) || strings.HasPrefix(key, qk))) {
			if hit >= 0 {
				return -1
			}
			hit = i
		}
	}
	return hit
}

// optionsMatch: the displayed rows are the transcript's options, then the TUI's own rows
// ("Type something." — or the typed text — and "Chat about this").
func (ap askPane) optionsMatch(q askQuestion) bool {
	if len(ap.Options) < len(q.Options)+1 {
		return false
	}
	for i, o := range q.Options {
		if alnumKey(ap.Options[i]) != alnumKey(o.Label) {
			return false
		}
	}
	return true
}

// --- the card ----------------------------------------------------------------------------------

// askMenuInfo builds the answerable card for a live tabbed menu, or ok=false when the pane and
// transcript do not describe the same menu (the caller falls back to the read-only pane view).
func askMenuInfo(pane string, ask pendingAsk) (attentionInfo, bool) {
	ap, ok := parseAskPane(pane)
	if !ok || !ap.headersMatch(ask) {
		return attentionInfo{}, false
	}
	info := attentionInfo{Pending: true, Kind: "question", ID: "claude-askq-" + ask.ToolUseID}
	answerable := true
	var text []string
	for i, q := range ask.Questions {
		if q.MultiSelect || len(q.Options) < 1 || len(q.Options)+1 > 9 {
			answerable = false // key semantics unverified: answered in the terminal
		}
		info.Questions = append(info.Questions, attentionQuestion{
			Header: q.Header, ID: fmt.Sprintf("q%d", i), Question: q.Question, Options: q.Options,
		})
		text = append(text, q.Header+": "+q.Question)
	}
	info.Text = strings.Join(text, "\n")
	if answerable {
		info.Actions = []string{"answer"}
	} else {
		info.ReadOnly = true
	}
	return info, true
}

// --- answering ---------------------------------------------------------------------------------

// askPick is one question's answer: Option is the 1-based row to press, or 0 for free text.
type askPick struct {
	Option int
	Text   string
}

func (p askPick) key(q askQuestion) string {
	if p.Option > 0 {
		return alnumKey(q.Options[p.Option-1].Label)
	}
	return alnumKey(p.Text)
}

// askPicks maps the PWA's answers (by question id "q<i>") to picks: an answer equal to an
// option's label picks that row, anything else is typed into the free-text row.
func askPicks(ask pendingAsk, answers []attentionAnswer) ([]askPick, error) {
	picks := make([]askPick, len(ask.Questions))
	seen := make([]bool, len(ask.Questions))
	for _, a := range answers {
		var i int
		if _, err := fmt.Sscanf(a.ID, "q%d", &i); err != nil || i < 0 || i >= len(ask.Questions) || seen[i] || fmt.Sprintf("q%d", i) != a.ID {
			return nil, fmt.Errorf("unknown or repeated question %q", a.ID)
		}
		seen[i] = true
		ans := strings.TrimSpace(a.Answer)
		if ans == "" {
			return nil, fmt.Errorf("empty answer for question %d", i+1)
		}
		picks[i] = askPick{Text: strings.Join(strings.Fields(ans), " ")} // one line: Enter would submit it early
		for j, o := range ask.Questions[i].Options {
			if o.Label == ans {
				picks[i] = askPick{Option: j + 1}
				break
			}
		}
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("question %d (%s) is unanswered", i+1, ask.Questions[i].Header)
		}
	}
	return picks, nil
}

// askIO is the pane and key access the answer run needs; tests fake it.
type askIO struct {
	pane  func(ctx context.Context) (string, error)
	text  func(ctx context.Context, s string) error
	named func(ctx context.Context, key string) error
	enter func(ctx context.Context) error
	// wait/polls bound how long a key may take to show on screen.
	wait  time.Duration
	polls int
}

var errAskChanged = fmt.Errorf("question menu changed or cleared")

func askChanged(why string) error { return fmt.Errorf("%w: %s", errAskChanged, why) }

// read captures and parses the live menu.
func (io askIO) read(ctx context.Context, ask pendingAsk) (askPane, error) {
	pane, err := io.pane(ctx)
	if err != nil {
		return askPane{}, err
	}
	ap, ok := parseAskPane(pane)
	if !ok {
		return askPane{}, askChanged("no live menu on screen")
	}
	if !ap.headersMatch(ask) {
		return askPane{}, askChanged("tabs differ from the pending question")
	}
	return ap, nil
}

// until polls until cond holds on the parsed pane (or the menu is gone, when goneOK).
func (io askIO) until(ctx context.Context, ask pendingAsk, goneOK bool, cond func(askPane) bool) (bool, error) {
	for i := 0; i < io.polls; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(io.wait):
			}
		}
		pane, err := io.pane(ctx)
		if err != nil {
			return false, err
		}
		ap, ok := parseAskPane(pane)
		if !ok {
			if goneOK {
				return true, nil
			}
			continue
		}
		if cond(ap) {
			return true, nil
		}
	}
	return false, nil
}

// answerAsk walks the menu: back to the first tab, per question (re-verify, press, wait for the
// next page), then the review screen is compared with the picks before "1" submits. Any
// mismatch stops before the next key. done is the number of questions already pressed.
func answerAsk(ctx context.Context, io askIO, ask pendingAsk, picks []askPick) (done int, cleared bool, err error) {
	n := len(ask.Questions)
	if len(picks) != n {
		return 0, false, fmt.Errorf("need %d answers", n)
	}
	for i, q := range ask.Questions {
		if q.MultiSelect || len(q.Options)+1 > 9 {
			return 0, false, fmt.Errorf("question %q cannot be answered from here; use the terminal", q.Header)
		}
		if picks[i].Option > len(q.Options) || (picks[i].Option == 0 && picks[i].Text == "") {
			return 0, false, fmt.Errorf("invalid answer for %q", q.Header)
		}
	}
	ap, err := io.read(ctx, ask)
	if err != nil {
		return 0, false, err
	}
	if ap.Editing {
		return 0, false, askChanged("a free-text answer is being typed in the terminal")
	}
	f := ap.focus(ask)
	if f < 0 {
		return 0, false, askChanged("cannot tell which tab is showing")
	}
	if f != 0 {
		for i := 0; i < f; i++ {
			if err := io.named(ctx, "Left"); err != nil {
				return 0, false, err
			}
		}
		ok, err := io.until(ctx, ask, false, func(p askPane) bool { return !p.Review && !p.Editing && p.focus(ask) == 0 })
		if err != nil || !ok {
			return 0, false, orChanged(err, "could not return to the first question")
		}
	}
	for i, q := range ask.Questions {
		ap, err := io.read(ctx, ask)
		if err != nil {
			return done, false, err
		}
		if ap.Editing || ap.focus(ask) != i || !ap.optionsMatch(q) {
			return done, false, askChanged(fmt.Sprintf("question %d is not the one showing", i+1))
		}
		pk := picks[i]
		if pk.Option > 0 {
			if err := io.text(ctx, fmt.Sprint(pk.Option)); err != nil {
				return done, false, err
			}
		} else if err := io.freeText(ctx, ask, i, ap, pk.Text); err != nil {
			return done, false, err
		}
		done = i + 1
		ok, err := io.until(ctx, ask, false, func(p askPane) bool { return !p.Editing && p.focus(ask) == i+1 })
		if err != nil || !ok {
			return done, false, orChanged(err, "the menu did not advance")
		}
	}
	ap, err = io.read(ctx, ask)
	if err != nil {
		return done, false, err
	}
	if !ap.Review || len(ap.Answers) != n {
		return done, false, askChanged("the review screen is not showing")
	}
	for i, q := range ask.Questions {
		if alnumKey(ap.Answers[i]) != picks[i].key(q) {
			return done, false, askChanged(fmt.Sprintf("review shows %q for question %d, not the answer sent; not submitted", ap.Answers[i], i+1))
		}
	}
	if err := io.text(ctx, "1"); err != nil {
		return done, false, err
	}
	cleared, err = io.until(ctx, ask, true, func(askPane) bool { return false })
	return done, cleared, err
}

func orChanged(err error, why string) error {
	if err != nil {
		return err
	}
	return askChanged(why)
}

// askPlaceholder is the free-text row until something is typed into it.
const askPlaceholder = "type something."

// maxEraseRunes bounds how much earlier text is deleted with BSpace.
const maxEraseRunes = 200

// freeText types text into question i's free-text row. A fresh row opens with its digit; a row
// that still holds text typed earlier (the TUI keeps it, and its digit would just select it) is
// reached with Down, emptied with BSpace and retyped. Enter is only sent once the row shows
// exactly the text.
func (io askIO) freeText(ctx context.Context, ask pendingAsk, i int, ap askPane, text string) error {
	q := ask.Questions[i]
	row := len(q.Options)
	if alnumKey(text) == "" {
		return fmt.Errorf("a typed answer needs at least one letter or digit")
	}
	prior := ap.Options[row]
	editing := func(p askPane) bool { return p.Editing && !p.Review && p.focus(ask) == i && p.Cursor == row }
	if strings.ToLower(prior) == askPlaceholder {
		if err := io.text(ctx, fmt.Sprint(row+1)); err != nil {
			return err
		}
	} else {
		erase := len([]rune(prior))
		if ap.Cursor < 0 || ap.Cursor > row {
			return askChanged("cannot tell where the cursor is")
		}
		if erase > maxEraseRunes {
			return askChanged("the free-text row already holds a long answer; use the terminal")
		}
		for k := ap.Cursor; k < row; k++ {
			if err := io.named(ctx, "Down"); err != nil {
				return err
			}
		}
		if ok, err := io.until(ctx, ask, false, editing); err != nil || !ok {
			return orChanged(err, "could not reach the free-text row")
		}
		for k := 0; k < erase; k++ {
			if err := io.named(ctx, "BSpace"); err != nil {
				return err
			}
		}
	}
	if ok, err := io.until(ctx, ask, false, editing); err != nil || !ok {
		return orChanged(err, "the free-text field did not open")
	}
	if err := io.text(ctx, text); err != nil {
		return err
	}
	shows := func(p askPane) bool {
		if !editing(p) || len(p.Options) <= row {
			return false
		}
		got, want := alnumKey(p.Options[row]), alnumKey(text)
		// a long answer wraps in the row; its first line is a long enough prefix
		return got == want || (len(got) >= 40 && strings.HasPrefix(want, got))
	}
	if ok, err := io.until(ctx, ask, false, shows); err != nil || !ok {
		return orChanged(err, "the typed text is not what the row shows; not confirmed")
	}
	return io.enter(ctx)
}
