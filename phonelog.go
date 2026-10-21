package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Phone-row log (session-store design F3, operator decision D7: JSONL).
//
// Every message the phone sends, and every state it passes through (sent,
// queued, delivering, delivered, unconfirmed, failed, stopping…), is appended as
// one JSON line to <state dir>/deck-remote-phone-rows.jsonl. The log is
// write-only for now: it is the durable record of "what the phone sent and what
// became of it" that survives a deck-remote restart, and the input for a later
// slice that serves phone rows back after a reload.
//
// The design doc fixes the format (JSONL) but neither a path nor rotation; chosen
// here, next to the other deck-remote logs (doc reads, tower actions): the file
// is 0600; at phoneLogMax it is renamed to <name>.1 (replacing the previous
// generation) and a fresh file starts, so at most ~2×phoneLogMax stays on disk.
// Message text is logged (once, on the event that carries it) because without it
// the log could not restore a row; it is capped at phoneLogTextMax bytes.

const (
	phoneLogName    = "deck-remote-phone-rows.jsonl"
	phoneLogMax     = 4 << 20
	phoneLogTextMax = 4096
)

type phoneLog struct {
	mu   sync.Mutex
	path string // "" = disabled

	// Texts the phone sent, oldest first (from the log on first use, then from
	// tap), so history can tell a phone send from another session's message.
	sent   []phoneSent
	loaded bool
}

type phoneSent struct {
	session, text string
	at            int64 // unix seconds the phone sent it
}

const (
	phoneSentKeep = 512
	// A send can wait out a long turn in agent-deck's queue before it lands.
	phoneEchoWindow = 3 * 3600
	// Transcript and server clocks are the same host, but the transcript stamps
	// the landing and the log the "sent" event: allow a little either way.
	phoneEchoSkew = 120
)

func newPhoneLog(home string) *phoneLog {
	if home == "" && os.Getenv("DECK_REMOTE_STATE_DIR") == "" {
		return &phoneLog{}
	}
	return &phoneLog{path: filepath.Join(stateDir(home), phoneLogName)}
}

// tap is installed as the SSE hub's publish hook; it records ask-state events
// that belong to a phone request.
func (p *phoneLog) tap(ev any) {
	m, ok := ev.(map[string]any)
	if !ok || m["type"] != "ask-state" || p.path == "" {
		return
	}
	req, _ := m["requestId"].(string)
	if req == "" {
		return // e.g. the interrupt's "stopping" has no request
	}
	rec := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339), "requestId": req}
	for _, k := range []string{"sessionId", "sendId", "state", "position", "error", "retryable", "slash"} {
		if v, ok := m[k]; ok && v != nil && v != "" && v != false {
			rec[k] = v
		}
	}
	if text, _ := m["text"].(string); text != "" {
		if len(text) > phoneLogTextMax {
			text = strings.ToValidUTF8(text[:phoneLogTextMax], "")
			rec["textTruncated"] = true
		}
		rec["text"] = text
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadLocked() // before this line is written, or it would be read back twice
	if sess, _ := m["sessionId"].(string); sess != "" && rec["text"] != nil {
		p.remember(sess, rec["text"].(string), time.Now().Unix())
	}
	if fi, err := os.Stat(p.path); err == nil && fi.Size() >= phoneLogMax {
		_ = os.Rename(p.path, p.path+".1")
	}
	f, err := os.OpenFile(p.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("phone log: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func (p *phoneLog) remember(session, text string, at int64) {
	p.sent = append(p.sent, phoneSent{session: session, text: normText(text), at: at})
	if n := len(p.sent) - phoneSentKeep; n > 0 {
		p.sent = append([]phoneSent(nil), p.sent[n:]...)
	}
}

// loadLocked reads the sent texts back from both log generations, once.
func (p *phoneLog) loadLocked() {
	if p.loaded || p.path == "" {
		return
	}
	p.loaded = true
	for _, path := range []string{p.path + ".1", p.path} {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			var rec struct {
				Ts        string `json:"ts"`
				SessionID string `json:"sessionId"`
				Text      string `json:"text"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil || rec.SessionID == "" || rec.Text == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339, rec.Ts)
			if err != nil {
				continue
			}
			p.remember(rec.SessionID, rec.Text, t.Unix())
		}
	}
}

// normText compares message texts modulo whitespace runs.
func normText(s string) string { return strings.Join(strings.Fields(s), " ") }

// claimEchoes turns a session's peer rows that are the phone's own sends back
// into the operator's rows. A phone message reaches the target through
// agent-deck, and Claude Code files it as a peer message from "unknown"; the
// phone also shows it as its own bubble, so it appeared twice, and agent-deck's
// landed check, looking for a typed prompt, called it unconfirmed. A peer row
// is the echo of a send when it has no sender session (from "" or "unknown"),
// its text equals a send logged for this session, and it landed within
// [sent-phoneEchoSkew, sent+phoneEchoWindow]. One send claims at most one row,
// oldest first. The row becomes Role "user", Origin "phone".
func (p *phoneLog) claimEchoes(session string, msgs []histMsg) []histMsg {
	if p == nil || p.path == "" {
		return msgs
	}
	p.mu.Lock()
	p.loadLocked()
	var sent []phoneSent
	for _, e := range p.sent {
		if e.session == session {
			sent = append(sent, e)
		}
	}
	p.mu.Unlock()
	if len(sent) == 0 {
		return msgs
	}
	used := make([]bool, len(sent))
	var out []histMsg
	for i, m := range msgs {
		if m.Role != "peer" || m.Sender != "" || (m.Origin != "peer:" && m.Origin != "peer:unknown") {
			continue
		}
		text := normText(m.Content)
		for j, e := range sent {
			if used[j] || e.text != text || m.Ts < e.at-phoneEchoSkew || m.Ts > e.at+phoneEchoWindow {
				continue
			}
			used[j] = true
			if out == nil {
				out = append([]histMsg(nil), msgs...)
			}
			out[i].Role, out[i].Kind, out[i].Origin = "user", "user", "phone"
			break
		}
	}
	if out == nil {
		return msgs
	}
	return out
}
