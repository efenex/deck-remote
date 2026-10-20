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
}

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
