package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// preview collapses a reply to a single-line snippet of at most n runes.
func preview(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// GET /api/rc/sessions — sessions + best-effort harness snapshots. The PWA is
// detail-first: agent-deck's status is unreliable in
// some setups (stale registry / churn), so we surface the real last reply
// (transcript-based, name-independent) rather than a status chip.
//
// Building the answer shells out to agent-deck and snapshots every session, which
// takes seconds on a busy host (measured 2-5 s; far more under load). So the built
// body is cached per (profile, all) and served stale-while-revalidate: a fresh hit
// is instant, a stale one is returned after at most sessionsGrace while a single
// background rebuild runs, and only a cold cache waits for a full build.
const (
	sessionsFresh    = 4 * time.Second         // served as-is
	sessionsGrace    = 1200 * time.Millisecond // how long a stale hit waits for the rebuild before answering with the stale body
	sessionsMaxStale = 10 * time.Minute        // older than this: block on a rebuild
)

type sessionsEntry struct {
	body   []byte
	at     time.Time
	timing string // Server-Timing of the build that produced body
}

type sessionsCache struct {
	mu     sync.Mutex
	m      map[string]*sessionsEntry
	flight map[string]chan struct{} // closed when the in-flight build for the key finishes
}

func newSessionsCache() *sessionsCache {
	return &sessionsCache{m: map[string]*sessionsEntry{}, flight: map[string]chan struct{}{}}
}

// refresh starts (or joins) the build for key and returns the channel closed when it is done.
func (s *server) refreshSessions(key, profile string, all bool) <-chan struct{} {
	c := s.sess
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.flight[key]; ok {
		return ch
	}
	ch := make(chan struct{})
	c.flight[key] = ch
	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.flight, key)
			c.mu.Unlock()
			close(ch)
		}()
		ctx, cancel := cliCtx(withProfile(context.Background(), profile), 30*time.Second)
		defer cancel()
		build := s.sessBuild
		if build == nil {
			build = s.buildSessions
		}
		body, timing, err := build(ctx, all)
		if err != nil {
			log.Printf("sessions: build %s: %v", key, err)
			return
		}
		c.mu.Lock()
		c.m[key] = &sessionsEntry{body: body, at: time.Now(), timing: timing}
		c.mu.Unlock()
	}()
	return ch
}

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	profile := profileFrom(withProfile(r.Context(), reqProfile(r)), s.cfg.profile)
	all := r.URL.Query().Get("all") == "1"
	key := profile + "|" + strconv.FormatBool(all)

	s.sess.mu.Lock()
	e := s.sess.m[key]
	s.sess.mu.Unlock()

	state := "hit"
	switch {
	case e != nil && time.Since(e.at) < sessionsFresh:
	case e != nil && time.Since(e.at) < sessionsMaxStale:
		done := s.refreshSessions(key, profile, all)
		select {
		case <-done:
		case <-time.After(sessionsGrace):
		case <-r.Context().Done():
		}
		state = "stale"
	default:
		state = "miss"
		select {
		case <-s.refreshSessions(key, profile, all):
		case <-r.Context().Done():
			return
		}
	}
	s.sess.mu.Lock()
	e = s.sess.m[key]
	s.sess.mu.Unlock()
	if e == nil {
		httpError(w, http.StatusBadGateway, "session list unavailable")
		return
	}
	if state == "stale" && time.Since(e.at) < sessionsFresh {
		state = "refreshed"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Sessions-Cache", state)
	w.Header().Set("X-Sessions-Age-Ms", strconv.FormatInt(time.Since(e.at).Milliseconds(), 10))
	w.Header().Set("Server-Timing", e.timing)
	_, _ = w.Write(e.body)
}

// warmSessions builds the default view once at startup so the first page load
// after a restart does not pay the cold cost.
func (s *server) warmSessions() {
	s.refreshSessions(s.cfg.profile+"|false", s.cfg.profile, false)
}

// buildSessions does the actual work (agent-deck list, per-session snapshots, the
// TUI view filter) and returns the JSON body plus a Server-Timing value.
func (s *server) buildSessions(ctx context.Context, all bool) ([]byte, string, error) {
	t0 := time.Now()
	sessions, err := s.listSessions(ctx)
	if err != nil {
		return nil, "", err
	}
	tList := time.Since(t0)

	// Concurrently fetch structured snapshots (bounded). Unsupported/custom
	// harnesses remain listed with terminal-only capabilities.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rctx, rc := context.WithTimeout(ctx, 5*time.Second)
			defer rc()
			adapter := s.adapterFor(sessions[i])
			snap, snapErr := adapter.Snapshot(rctx, sessions[i], "", false)
			if snapErr == nil {
				if snap.DegradedReason == "" {
					sessions[i].Capabilities = adapter.Capabilities(rctx, sessions[i])
				}
				sessions[i].LastReply = preview(snap.LastReply, 160)
				sessions[i].LastActivity = snap.LastActivity
				sessions[i].Working = snap.Activity.Working
				sessions[i].Activity = snap.Activity.Activity
				sessions[i].CurrentTool = snap.Activity.CurrentTool
				sessions[i].Stalled = snap.Activity.Stalled
				sessions[i].State = snap.State
				sessions[i].DegradedReason = snap.DegradedReason
			}
			if attn, ok := s.attn.get(sessions[i].ID); ok {
				sessions[i].NeedsAttention = attn.Pending
			} else {
				sessions[i].NeedsAttention = snap.Attention.Pending
			}
		}(i)
	}
	wg.Wait()
	tSnap := time.Since(t0) - tList

	// Mirror the TUI's list filter unless the PWA asked for everything.
	var view viewInfo
	if !all {
		sessions, view = applyView(sessions, s.viewFilterFor(ctx))
	} else {
		view.Label = s.viewFilterFor(ctx).label()
	}
	tView := time.Since(t0) - tList - tSnap

	body, err := json.Marshal(map[string]any{"sessions": sessions, "view": view})
	if err != nil {
		return nil, "", err
	}
	timing := "list;dur=" + ms(tList) + ", snapshots;dur=" + ms(tSnap) + ", view;dur=" + ms(tView) + ", build;dur=" + ms(time.Since(t0))
	return body, timing, nil
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

// GET /api/rc/reply?id=<id|title> — last clean reply (wraps `session output --json`).
func (s *server) handleReply(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing id")
		return
	}
	ctx, cancel := cliCtx(withProfile(r.Context(), reqProfile(r)), 10*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if snapshotHarness(se.Tool) {
		snap, snapErr := s.adapterFor(se).Snapshot(ctx, se, "", false)
		if snapErr != nil || snap.DegradedReason != "" {
			if snapErr != nil {
				httpError(w, http.StatusBadGateway, snapErr.Error())
			} else {
				httpError(w, http.StatusNotImplemented, snap.DegradedReason)
			}
			return
		}
		out := replyOutput{Content: snap.LastReply, Role: "assistant"}
		if snap.LastActivity > 0 {
			out.Timestamp = time.Unix(snap.LastActivity, 0).UTC().Format(time.RFC3339)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	out, err := s.claudeReply(ctx, se)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	// Sanitize raw/structured content (e.g. a serialized tool_use block) so the
	// client never renders a reply turn as a literal JSON object/array.
	out.Content = cleanReplyContent(out.Content)
	writeJSON(w, http.StatusOK, out)
}

// GET /api/rc/status?id=<id|title> — quick status for one session.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		httpError(w, http.StatusBadRequest, "missing id")
		return
	}
	ctx, cancel := cliCtx(withProfile(r.Context(), reqProfile(r)), 10*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, id)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": se.ID, "title": se.Title, "status": se.Status, "tool": se.Tool})
}
