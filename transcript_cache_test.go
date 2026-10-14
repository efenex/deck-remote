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
