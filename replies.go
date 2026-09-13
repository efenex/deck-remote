package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Last-reply source for Claude sessions. The old path shelled `agent-deck
// session output --json` per session per 6s sweep AND per PWA deck refresh —
// the second big slice of the CLI polling burn. But the CLI itself just reads
// the transcript JSONL, which we already locate and parse for /api/rc/history
// (findTranscript/parseTranscript). So the hot path becomes: resolve the
// transcript once, then a stat() per poll — parse only when mtime/size moved,
// exec never. The CLI survives as a throttled fallback for sessions whose
// transcript isn't on this filesystem (e.g. --ssh sessions).

// cliReplyTTL throttles the exec fallback: a session without a local
// transcript is re-fetched via the CLI at most this often.
const cliReplyTTL = 15 * time.Second

// cliEmptyReplyTTL is the throttle after a CLI attempt that yielded NOTHING
// (error, or empty content matching an empty transcript). Retrying such a
// session every cliReplyTTL re-creates measurable exec churn (~40 spawns/10m
// per reply-less session) for zero information; new replies almost always
// land in the transcript anyway, which is polled for free.
const cliEmptyReplyTTL = 5 * time.Minute

// envRecheckEvery bounds how often we re-read CLAUDE_SESSION_ID from the live
// tmux environment. The list's claudeSessionId goes stale when /clear mints a
// new conversation (the CLI re-reads the env on every call for this reason);
// one tiny tmux exec per session per minute keeps us converging on the current
// transcript without re-introducing per-poll process churn.
const envRecheckEvery = 60 * time.Second

type replyEntry struct {
	mu       sync.Mutex
	envID    string    // CLAUDE_SESSION_ID from the live tmux env
	envAt    time.Time // when envID was last (re)read
	claudeID string    // id whose transcript we last served
	path     string
	mtime    time.Time
	size     int64
	out      replyOutput
	valid    bool
	cliAt    time.Time   // last CLI-fallback attempt (throttle)
	cliOut   replyOutput // last successful CLI-fallback result
	cliEmpty bool        // last CLI attempt yielded nothing (longer backoff)
}

type replyCache struct {
	mu sync.Mutex
	m  map[string]*replyEntry
}

func newReplyCache() *replyCache { return &replyCache{m: map[string]*replyEntry{}} }

func (c *replyCache) entry(id string) *replyEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[id]
	if e == nil {
		e = &replyEntry{}
		c.m[id] = e
	}
	return e
}

// claudeReply returns the session's last assistant reply, transcript-first.
// Callers still run cleanReplyContent over .Content (no-op for the transcript
// path, needed for the CLI fallback).
func (s *server) claudeReply(ctx context.Context, se sessionInfo) (replyOutput, error) {
	e := s.replies.entry(se.ID)
	e.mu.Lock()
	defer e.mu.Unlock()

	// Refresh the live-env session id occasionally; it wins over the list's
	// (possibly stale-after-/clear) id when it resolves to a transcript.
	if se.TmuxSession != "" && time.Since(e.envAt) >= envRecheckEvery {
		e.envAt = time.Now()
		if id, err := s.tmuxEnvironment(ctx, se.TmuxSession, "CLAUDE_SESSION_ID"); err == nil && id != "" {
			e.envID = id
		}
	}

	// Candidate ids, most-trusted first: the live tmux env, then the id a
	// previous read (transcript or CLI) confirmed, then the list's — the web
	// snapshot's claudeSessionId can be STALE after /clear and resolve to an
	// old (often reply-less) transcript, so it goes last.
	path, claudeID := "", ""
	for _, id := range []string{e.envID, e.claudeID, se.ClaudeSessionID} {
		if id == "" {
			continue
		}
		if p := findTranscript(id, se.Path); p != "" {
			path, claudeID = p, id
			break
		}
	}

	tOut, tOK := replyOutput{}, false
	if path != "" {
		if st, err := os.Stat(path); err == nil {
			if e.valid && e.path == path && st.ModTime().Equal(e.mtime) && st.Size() == e.size {
				tOut, tOK = e.out, true
			} else if out, perr := lastReplyFromTranscript(path, claudeID); perr == nil {
				e.claudeID, e.path, e.out, e.valid = claudeID, path, out, true
				e.mtime, e.size = st.ModTime(), st.Size()
				tOut, tOK = out, true
			}
		}
	}
	if tOK && tOut.Content != "" {
		return tOut, nil
	}

	// Transcript missing OR empty: the id we resolved may be the wrong
	// conversation — ask the CLI (throttled so polling can't re-create the
	// exec churn; harder backoff once the CLI proves equally empty). A
	// successful CLI read teaches us the real claude id, so subsequent polls
	// resolve the right transcript exec-free.
	ttl := cliReplyTTL
	if e.cliEmpty {
		ttl = cliEmptyReplyTTL
	}
	if time.Since(e.cliAt) < ttl {
		if e.cliOut.Content != "" || !tOK {
			return e.cliOut, nil
		}
		return tOut, nil
	}
	e.cliAt = time.Now()
	out, err := s.sessionReply(ctx, se.ID)
	if err != nil {
		e.cliEmpty = true
		if tOK {
			return tOut, nil
		}
		return replyOutput{}, err
	}
	e.cliOut = out
	e.cliEmpty = out.Content == ""
	if out.ClaudeSessionID != "" {
		e.claudeID = out.ClaudeSessionID
	}
	if out.Content != "" || !tOK {
		return out, nil
	}
	return tOut, nil
}

// replyTailBytes is how much of the transcript tail we scan for the last
// assistant message before falling back to a full parse. Assistant text
// entries are small; the tail almost always contains the latest one.
const replyTailBytes = 256 * 1024

// lastReplyFromTranscript extracts the newest assistant text message from a
// transcript JSONL, reading only the file tail in the common case.
func lastReplyFromTranscript(path, claudeID string) (replyOutput, error) {
	f, err := os.Open(path)
	if err != nil {
		return replyOutput{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return replyOutput{}, err
	}
	off := st.Size() - replyTailBytes
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return replyOutput{}, err
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return replyOutput{}, err
	}
	if off > 0 {
		// Drop the first (almost certainly partial) line.
		if i := bytes.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		} else {
			tail = nil
		}
	}
	if out, ok := lastAssistantInLines(tail, claudeID); ok {
		return out, nil
	}
	if off == 0 {
		return replyOutput{ClaudeSessionID: claudeID, Role: "assistant"}, nil // no reply yet
	}
	// Tail had no complete assistant entry (e.g. giant tool_use lines): full parse.
	msgs, err := parseTranscript(path)
	if err != nil {
		return replyOutput{}, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "reply" {
			return replyOutput{
				ClaudeSessionID: claudeID,
				Content:         msgs[i].Content,
				Role:            "assistant",
				Timestamp:       time.Unix(msgs[i].Ts, 0).UTC().Format(time.RFC3339),
			}, nil
		}
	}
	return replyOutput{ClaudeSessionID: claudeID, Role: "assistant"}, nil
}

// lastAssistantInLines scans complete JSONL lines for the last assistant entry
// with text content.
func lastAssistantInLines(data []byte, claudeID string) (replyOutput, bool) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	var out replyOutput
	found := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var l jsonlLine
		if json.Unmarshal(line, &l) != nil || l.IsMeta || l.Type != "assistant" {
			continue
		}
		if txt := assistantText(l.Message.Content); txt != "" {
			out = replyOutput{ClaudeSessionID: claudeID, Content: txt, Role: "assistant", Timestamp: l.Timestamp}
			found = true
		}
	}
	return out, found
}
