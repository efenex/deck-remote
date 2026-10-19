package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const transcriptLine = `{"type":"user","timestamp":"2026-10-14T10:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"

// The PWA polls history every few seconds; an unchanged transcript must not
// be re-read, a changed one must.
func TestParseTranscriptCachesUntilFileChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(transcriptLine), 0o600); err != nil {
		t.Fatal(err)
	}
	parses := func() int { transcripts.mu.Lock(); defer transcripts.mu.Unlock(); return transcripts.parses }
	before := parses()
	a, err := parseTranscript(path)
	if err != nil || len(a) != 1 {
		t.Fatalf("first parse: %v, %d msgs", err, len(a))
	}
	if _, err := parseTranscript(path); err != nil {
		t.Fatal(err)
	}
	if got := parses() - before; got != 1 {
		t.Fatalf("unchanged file parsed %d times, want 1", got)
	}
	a[0].Content = "mutated" // callers own their copy
	b, _ := parseTranscript(path)
	if b[0].Content != "hello" {
		t.Fatalf("cache shared with a caller: %q", b[0].Content)
	}

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(transcriptLine)
	f.Close()
	c, _ := parseTranscript(path)
	if len(c) != 2 || parses()-before != 2 {
		t.Fatalf("changed file: %d msgs, %d parses", len(c), parses()-before)
	}
}

// A transcript that can't be found is not searched for again within the TTL.
func TestFindTranscriptCachesMisses(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "projects", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := "miss-" + time.Now().Format("150405.000000000")
	if p := findTranscript(id, "/nowhere"); p != "" {
		t.Fatalf("unexpected %q", p)
	}
	// Create it now; within the TTL the cached miss still answers.
	real := filepath.Join(cfg, "projects", "x", id+".jsonl")
	if err := os.WriteFile(real, []byte(transcriptLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := findTranscript(id, "/nowhere"); p != "" {
		t.Fatalf("miss not cached: %q", p)
	}
	if p := findTranscriptUncached(id, "/nowhere"); p != real {
		t.Fatalf("uncached lookup = %q, want %q", p, real)
	}
}

// A growing transcript is parsed incrementally: the cached prefix is reused, only
// the appended bytes are decoded, and a half-written last line waits for its newline.
func TestParseTranscriptIsIncrementalAndSkipsToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendTo := func(s string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		f.Close()
	}
	reply := `{"type":"assistant","timestamp":"2026-10-14T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}]}}` + "\n"
	toolResult := `{"type":"user","timestamp":"2026-10-14T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","content":"big output"}]}}` + "\n"
	appendTo(transcriptLine)
	msgs, err := parseTranscript(path)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("first: %v %d", err, len(msgs))
	}
	appendTo(toolResult + reply + `{"type":"user","timestamp":"2026-10-14T10:00:03Z","message":{"role":"user","con`) // trailing partial line
	msgs, err = parseTranscript(path)
	if err != nil || len(msgs) != 2 || msgs[1].Role != "reply" || msgs[1].Content != "hi there" {
		t.Fatalf("after append (tool_result skipped, partial line pending): %v %+v", err, msgs)
	}
	transcripts.mu.Lock()
	off := transcripts.parsed[path].off
	transcripts.mu.Unlock()
	if want := int64(len(transcriptLine) + len(toolResult) + len(reply)); off != want {
		t.Fatalf("offset must stop before the partial line: got %d want %d", off, want)
	}
	appendTo(`tent":"third"}}` + "\n")
	msgs, _ = parseTranscript(path)
	if len(msgs) != 3 || msgs[2].Content != "third" {
		t.Fatalf("completed line must be picked up: %+v", msgs)
	}
	// a rewritten (shorter) file is re-read from the start
	if err := os.WriteFile(path, []byte(transcriptLine), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(3 * time.Second)
	_ = os.Chtimes(path, future, future)
	if msgs, _ = parseTranscript(path); len(msgs) != 1 {
		t.Fatalf("truncated file must be reparsed from scratch: %+v", msgs)
	}
}
