package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Optional read boundary: no control or parent-history methods are added.
// A child is a durable conversation; invocations can resume that same child.
// Monitor/Bash task IDs never enter this contract.
type subagentReader interface {
	Subagents(context.Context, sessionInfo) subagentList
}
type subagentCapabilities struct {
	Summary  bool   `json:"summary"`
	Thread   bool   `json:"thread"`
	Live     bool   `json:"live"`
	Provider string `json:"provider,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func childCapabilities(se sessionInfo) subagentCapabilities {
	if se.Tool == "claude" && se.SSHHost == "" {
		return subagentCapabilities{Summary: true, Provider: "claude-jsonl-2.1"}
	}
	if se.Tool == "opencode" {
		return subagentCapabilities{Provider: "opencode-2", Reason: "child linkage unknown; awaiting 2.x evidence"}
	}
	return subagentCapabilities{Reason: "child source unavailable"}
}

type childInvocation struct {
	ID               string    `json:"id"`
	ParentRowID      string    `json:"parentRowId"`
	ParentToolCallID string    `json:"parentToolCallId"`
	ParentBlock      int       `json:"parentBlock"`
	StartedAt        time.Time `json:"startedAt"`
	Outcome          string    `json:"outcome,omitempty"`
}
type childAction struct {
	Kind string    `json:"kind"`
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}
type childRun struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	Label           string            `json:"label"`
	Status          string            `json:"status"`
	StateSource     string            `json:"stateSource"`
	Freshness       string            `json:"freshness"`
	EvidenceAt      time.Time         `json:"evidenceAt"`
	Availability    string            `json:"availability"`
	Linked          bool              `json:"linked"`
	LastAction      *childAction      `json:"lastAction,omitempty"`
	Invocations     []childInvocation `json:"invocations"`
	ThreadAvailable bool              `json:"threadAvailable"`
}
type subagentSummary struct {
	Running      int       `json:"running"`
	Waiting      int       `json:"waiting"`
	Unknown      int       `json:"unknown"`
	ObservedAt   time.Time `json:"observedAt"`
	FreshUntil   time.Time `json:"freshUntil"`
	Availability string    `json:"availability"`
	Complete     bool      `json:"complete"`
	Revision     string    `json:"revision"`
}
type subagentList struct {
	subagentSummary
	Schema     string     `json:"schema"`
	Root       string     `json:"root"`
	Segment    string     `json:"segment"`
	Children   []childRun `json:"children"`
	NextCursor string     `json:"nextCursor,omitempty"`
	Truncated  bool       `json:"truncated"`
	Reset      bool       `json:"reset,omitempty"`
}

const childFreshFor = 30 * time.Second
const childCacheTTL = 6 * time.Second
const childMaxRoots = 128
const childMaxRuns = 100
const childPageBytes = 128 << 10

type subagentCache struct {
	mu       sync.Mutex
	entries  map[string]childCacheEntry
	bindings map[string]childBinding
	epoch    string
}
type childBinding struct {
	at             time.Time
	listed, native string
}
type childCacheEntry struct {
	at    time.Time
	list  subagentList
	files map[string]os.FileInfo
}

func childToken(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}
func emptyChildren(se sessionInfo, now time.Time) subagentList {
	return subagentList{Schema: "deck-remote.subagents/v1", Root: se.ID, Children: []childRun{}, subagentSummary: subagentSummary{Availability: "unavailable", ObservedAt: now}}
}
func (s *server) readSubagents(ctx context.Context, se sessionInfo) subagentList {
	if reader, ok := s.adapterFor(se).(subagentReader); ok && childCapabilities(se).Summary {
		return reader.Subagents(ctx, se)
	}
	return emptyChildren(se, time.Now().UTC())
}
func (a claudeAdapter) Subagents(ctx context.Context, se sessionInfo) subagentList {
	now := time.Now().UTC()
	if se.SSHHost != "" {
		return emptyChildren(se, now)
	}
	// Read the authorized root's native binding. The older reply cache is keyed
	// only by Deck ID; do not borrow it across profiles for child authorization.
	native := a.s.childNative(ctx, se, now)
	if !childNativeID.MatchString(native) {
		return emptyChildren(se, now)
	}
	path := findTranscript(native, se.Path)
	if path == "" {
		return emptyChildren(se, now)
	}
	return a.s.children.read(ctx, profileFrom(ctx, a.s.cfg.profile), se, path, native, now)
}
func (s *server) childNative(ctx context.Context, se sessionInfo, now time.Time) string {
	c := &s.children
	c.mu.Lock()
	defer c.mu.Unlock()
	key := strings.Join([]string{profileFrom(ctx, s.cfg.profile), se.ID, se.Path, se.SSHHost, se.TmuxSocket, se.TmuxSession}, "\x00")
	if c.bindings == nil {
		c.bindings = map[string]childBinding{}
	}
	if b, ok := c.bindings[key]; ok && b.listed == se.ClaudeSessionID && now.Sub(b.at) < envRecheckEvery {
		return b.native
	}
	if len(c.bindings) >= childMaxRoots {
		for k, b := range c.bindings {
			if now.Sub(b.at) >= envRecheckEvery {
				delete(c.bindings, k)
			}
		}
		if len(c.bindings) >= childMaxRoots {
			for k := range c.bindings {
				delete(c.bindings, k)
				break
			}
		}
	}
	native := se.ClaudeSessionID
	if se.TmuxSession != "" && se.SSHHost == "" {
		// Same pacing gate and refresh cadence as the parent reply resolver,
		// but explicitly scoped to this root's socket and profile.
		if out, err := s.tmuxExec(ctx, se.TmuxSocket, "show-environment", "-t", se.TmuxSession, "CLAUDE_SESSION_ID"); err == nil {
			line := strings.TrimSpace(string(out))
			if strings.HasPrefix(line, "CLAUDE_SESSION_ID=") {
				native = strings.TrimPrefix(line, "CLAUDE_SESSION_ID=")
			}
		}
	}
	c.bindings[key] = childBinding{now, se.ClaudeSessionID, native}
	return native
}
func (c *subagentCache) read(ctx context.Context, profile string, se sessionInfo, path, native string, now time.Time) subagentList {
	// Single-flight ingestion shared by all phones. No persistent file handles,
	// goroutines, or detailed thread buffers; each bounded scan expires after 6s.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]childCacheEntry)
		var seed [32]byte
		_, _ = rand.Read(seed[:])
		c.epoch = hex.EncodeToString(seed[:])
	}
	key := strings.Join([]string{profile, se.SSHHost, se.ID, path, native}, "\x00")
	if e, ok := c.entries[key]; ok && now.Sub(e.at) < childCacheTTL {
		return ageChildList(e.list, now)
	}
	files := childSourceFiles(path, native)
	if e, ok := c.entries[key]; ok && sameChildFiles(e.files, files) {
		list := ageChildList(e.list, now)
		list.ObservedAt = now
		c.entries[key] = childCacheEntry{now, list, files}
		return list
	}
	for k, e := range c.entries {
		if now.Sub(e.at) > time.Minute {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= childMaxRoots { // bounded LRU eviction, no live readers to close
		var oldest string
		var at time.Time
		for k, e := range c.entries {
			if oldest == "" || e.at.Before(at) {
				oldest, at = k, e.at
			}
		}
		delete(c.entries, oldest)
	}
	list := readClaudeChildren(ctx, se, path, native, childToken(c.epoch, key), now)
	c.entries[key] = childCacheEntry{now, list, files}
	return list
}
func ageChildList(list subagentList, now time.Time) subagentList {
	list.Children = append([]childRun{}, list.Children...)
	list.Running, list.Waiting, list.Unknown = 0, 0, 0
	finishChildList(&list, now)
	return list
}
func childPage(list subagentList, segment, cursor string, limit int) (subagentList, int) {
	if segment != "" && segment != list.Segment {
		list.Children = []childRun{}
		list.Reset = true
		return list, http.StatusConflict
	}
	start := 0
	if cursor != "" {
		start = -1
		for i := 1; i < len(list.Children); i++ {
			if cursor == childToken(list.Segment, list.Revision, strconv.Itoa(i)) {
				start = i
				break
			}
		}
		if start < 0 {
			list.Children = []childRun{}
			list.Reset = true
			return list, http.StatusConflict
		}
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 50 {
		limit = 50
	}
	end := min(start+limit, len(list.Children))
	bytes := 0
	for i := start; i < end; i++ {
		encoded, _ := json.Marshal(list.Children[i])
		if i > start && bytes+len(encoded) > childPageBytes {
			end = i
			break
		}
		bytes += len(encoded)
	}
	if end < len(list.Children) {
		list.NextCursor = childToken(list.Segment, list.Revision, strconv.Itoa(end))
	}
	list.Children = list.Children[start:end]
	return list, http.StatusOK
}
func (s *server) handleSubagents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(w, 400, "missing id")
		return
	}
	// S1 only accepts root, segment and cursor tokens; no child/path/URL lookup.
	for _, key := range []string{"child", "path", "url", "host"} {
		if r.URL.Query().Has(key) {
			httpError(w, 400, "unsupported child lookup")
			return
		}
	}
	ctx, cancel := cliCtx(withProfile(r.Context(), reqProfile(r)), 10*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, id)
	if err != nil || se.ID != id {
		httpError(w, 404, "root session unavailable")
		return
	}
	list := s.readSubagents(ctx, se)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, code := childPage(list, r.URL.Query().Get("segment"), r.URL.Query().Get("cursor"), limit)
	writeJSON(w, code, list)
}
func finishChildList(list *subagentList, now time.Time) {
	list.FreshUntil = now.Add(childFreshFor)
	for i := range list.Children {
		child := &list.Children[i]
		age := now.Sub(child.EvidenceAt)
		child.Freshness = "stale"
		if !child.EvidenceAt.IsZero() && age >= -time.Second && age < childFreshFor {
			child.Freshness = "fresh"
		}
		if child.Status == "running" && (child.Freshness != "fresh" || child.Availability != "available" || !child.Linked) {
			child.Status = "unknown"
		}
		switch child.Status {
		case "running":
			list.Running++
			until := child.EvidenceAt.Add(childFreshFor)
			if until.Before(list.FreshUntil) {
				list.FreshUntil = until
			}
		case "unknown":
			list.Unknown++
		}
	}
	// Observation time does not invalidate paging. Source revisions, lifecycle
	// changes, and restart-scoped segment identity do.
	data, _ := json.Marshal(struct {
		Children     []childRun
		Complete     bool
		Availability string
	}{list.Children, list.Complete, list.Availability})
	list.Revision = childToken(list.Segment, string(data))
}
