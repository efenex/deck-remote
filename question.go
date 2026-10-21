package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Claude's AskUserQuestion menu ("card choice") is not a permission dialog: it
// has a ☐ header, the question, numbered options with indented descriptions and
// the footer "Enter to select · ↑/↓ to navigate · Esc to cancel". None of the
// permission markers appear in it, so isClaudePermissionPrompt never saw it and
// a session parked on one read as plainly idle (2026-10-21, ga-rdb).

var (
	claudeMenuFooter   = regexp.MustCompile(`(?i)enter to select.*esc to cancel`)
	claudeMenuHeader   = regexp.MustCompile(`^[\s│]*[☐☒✔]\s+(\S.*?)\s*$`)
	claudeMenuOption   = regexp.MustCompile(`^[\s│]*(?:❯\s*)?(\d+)\.\s+(\S.*?)\s*$`)
	claudeMenuRule     = regexp.MustCompile(`^[\s─━-]{6,}$`)
	claudeMenuSelected = regexp.MustCompile(`(?m)^[\s│]*❯\s*\d+\.`)
)

// claudeMenuTail is how many trailing non-blank lines are searched for the
// footer: the footer ends a live menu, with the background-task, shortcut and
// status rows Claude draws under it (cf. claudeDialogWindow).
const claudeMenuTail = 10

// claudeMenuMax bounds the walk up from the footer to the ☐ header.
const claudeMenuMax = 60

// parseClaudeQuestion returns the live AskUserQuestion menu at the bottom of the
// pane, if any. Permission dialogs are not menus (checked first by the caller).
func parseClaudeQuestion(pane string) (attentionQuestion, string, bool) {
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(stripANSI(pane), "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimRight(l, " "))
		}
	}
	foot := -1
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-claudeMenuTail; i-- {
		if claudeMenuFooter.MatchString(lines[i]) {
			foot = i
			break
		}
	}
	if foot < 0 {
		return attentionQuestion{}, "", false
	}
	head := -1
	for i := foot; i >= 0 && i > foot-claudeMenuMax; i-- {
		if claudeMenuHeader.MatchString(lines[i]) || askTabRow.MatchString(lines[i]) {
			head = i
			break
		}
	}
	if head < 0 {
		return attentionQuestion{}, "", false
	}
	block := lines[head:foot]
	if !claudeMenuSelected.MatchString(strings.Join(block, "\n")) {
		return attentionQuestion{}, "", false
	}
	// A tabbed multi-question menu starts with the "← ☐ A ☐ B ✔ Submit →" row instead of a
	// "☐ header" line; the pane shows one page of it, so the header is only "Question".
	q := attentionQuestion{Header: "Question", ID: "claude-menu"}
	if m := claudeMenuHeader.FindStringSubmatch(lines[head]); m != nil {
		q.Header = m[1]
	}
	var text []string
	for _, l := range block[1:] {
		if claudeMenuRule.MatchString(l) {
			continue
		}
		if m := claudeMenuOption.FindStringSubmatch(l); m != nil {
			q.Options = append(q.Options, attentionOption{Label: m[2]})
			continue
		}
		clean := strings.TrimSpace(strings.Trim(strings.TrimSpace(l), "│"))
		switch {
		case clean == "":
		case len(q.Options) == 0:
			text = append(text, clean)
		default: // indented description of the option above
			last := &q.Options[len(q.Options)-1]
			if last.Description != "" {
				last.Description += " "
			}
			last.Description += clean
		}
	}
	q.Question = strings.Join(text, " ")
	if len(q.Options) < 2 {
		return attentionQuestion{}, "", false
	}
	return q, strings.Join(block, "\n"), true
}

func isClaudeQuestionMenu(pane string) bool {
	_, _, ok := parseClaudeQuestion(pane)
	return ok
}

// claudeQuestionInfo wraps the parsed menu as an attention request. It is
// read-only (no Actions): the menu is answered in the terminal and no key is
// ever sent to it from here. The ID covers the question and option labels but
// not the cursor, so moving the highlight does not mint a new request.
func claudeQuestionInfo(pane string) (attentionInfo, bool) {
	q, text, ok := parseClaudeQuestion(pane)
	if !ok {
		return attentionInfo{}, false
	}
	key := q.Header + "\n" + q.Question
	for _, o := range q.Options {
		key += "\n" + o.Label
	}
	return attentionInfo{
		Pending: true, Kind: "question", ID: fmt.Sprintf("claude-question-%x", hashStr(key)),
		Text: text, Questions: []attentionQuestion{q}, ReadOnly: true,
	}, true
}
