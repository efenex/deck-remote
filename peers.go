package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Peer and slash rows (session-store design §6.4, operator decision D4: shown
// by default, collapsed). One classifier serves the recall path and the
// transcript fallback so both produce the same rows:
//
//	Role "peer",  Kind "peer",  Origin "peer:<from-id>", Content = the message
//	              without the "Another Claude session sent a message:" preamble
//	              and the [agent-deck from:…] envelope line; Sender = the
//	              display name when the envelope names one (cross-session form).
//	Role "slash", Kind "slash", Content = "/command args".
//
// Task-notifications, hook feedback, local-command output and skill prefaces
// stay hidden (they never reach here: humanText and the callers drop them).

var (
	peerPreambleRE = regexp.MustCompile(`^\s*Another Claude session sent a message:[ \t]*\r?\n`)
	// [agent-deck from:<id>] on its own first line.
	peerEnvelopeRE = regexp.MustCompile(`^\s*\[agent-deck from:([^\]\s]*)\][ \t]*\r?\n?`)
	// <cross-session-message from="…" from-name="…" …>\nbody</cross-session-message>
	crossSessionRE = regexp.MustCompile(`(?s)^\s*<cross-session-message\s+([^>]*)>\s*(.*?)\s*(?:</cross-session-message>\s*)?$`)
	crossAttrRE    = regexp.MustCompile(`([a-z-]+)="([^"]*)"`)

	cmdNameRE = regexp.MustCompile(`(?s)<command-name>\s*(.*?)\s*</command-name>`)
	cmdArgsRE = regexp.MustCompile(`(?s)<command-args>\s*(.*?)\s*</command-args>`)
)

// parsePeer recognises a message another session sent (agent-deck's envelope,
// with or without Claude Code's preamble, or the cross-session form). from is
// the sender's id; name a display name when the envelope carries one.
func parsePeer(body string) (from, name, text string, ok bool) {
	s := body
	pre := false
	if m := peerPreambleRE.FindString(s); m != "" {
		s, pre = s[len(m):], true
	}
	if m := peerEnvelopeRE.FindStringSubmatch(s); m != nil {
		return m[1], "", strings.TrimSpace(s[len(m[0]):]), true
	}
	if m := crossSessionRE.FindStringSubmatch(s); m != nil {
		attrs := map[string]string{}
		for _, a := range crossAttrRE.FindAllStringSubmatch(m[1], -1) {
			attrs[a[1]] = a[2]
		}
		return attrs["from"], attrs["from-name"], strings.TrimSpace(m[2]), true
	}
	if pre { // a preamble without a recognisable envelope is still a peer message
		return "", "", strings.TrimSpace(s), true
	}
	return "", "", "", false
}

// parseSlash turns a Claude Code command wrapper
// (<command-name>/tick</command-name><command-args>x</command-args>) into
// "/tick x". ok=false when body is not a command wrapper.
func parseSlash(body string) (string, bool) {
	m := cmdNameRE.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	name := strings.TrimSpace(m[1])
	if name == "" {
		return "", false
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	if a := cmdArgsRE.FindStringSubmatch(body); a != nil {
		if args := strings.TrimSpace(a[1]); args != "" {
			name += " " + args
		}
	}
	return name, true
}

// peerMsg / slashMsg build the rows.
func peerMsg(from, name, text string, ts int64, id string, queued bool) histMsg {
	origin := "peer:" + from
	return histMsg{Role: "peer", Kind: "peer", Content: text, Ts: ts, ID: id, Queued: queued, Origin: origin, Sender: name}
}

func slashMsg(cmd string, ts int64, id string) histMsg {
	return histMsg{Role: "slash", Kind: "slash", Content: cmd, Ts: ts, ID: id, Origin: "human"}
}

// userEntryRow classifies a plain user-entry string that is not a `!` line:
// a peer message, a slash command, a human prompt, or nothing (synthetic).
func userEntryRow(raw json.RawMessage, ts int64, id string, queued bool) (histMsg, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return histMsg{}, false
	}
	s = strings.TrimSpace(reminderBlockRE.ReplaceAllString(s, ""))
	if from, name, text, ok := parsePeer(s); ok {
		if text == "" {
			return histMsg{}, false
		}
		return peerMsg(from, name, text, ts, id, queued), true
	}
	if cmd, ok := parseSlash(s); ok {
		return slashMsg(cmd, ts, id), true
	}
	t := humanText(raw)
	if t == "" {
		return histMsg{}, false
	}
	return histMsg{Role: "user", Content: t, Ts: ts, ID: id, Queued: queued}, true
}
