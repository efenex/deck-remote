package main

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Session-list view sync. The agent-deck TUI persists its list filter (status
// filter ! @ # $ % / archived, time range *) per profile; `agent-deck view
// --json` (fork build, cmd/agent-deck/view_cmd.go) resolves it to a status set
// and an activity lower bound. handleSessions applies it so the phone shows
// what the TUI shows, except that a session needing attention is never hidden.
// The TUI stays the only place the filter is set; the PWA can bypass it per
// request (?all=1). A binary without the command means no filter.

// viewCacheTTL bounds how stale the synced filter may be. The TUI writes it
// immediately on '*' and within ~10s for status keys, so a few seconds is in
// the noise, and it keeps the CLI exec off the PWA's polling path.
const viewCacheTTL = 5 * time.Second

// viewFilter mirrors `agent-deck view --json`.
type viewFilter struct {
	StatusFilter    string   `json:"status_filter"`
	Statuses        []string `json:"statuses"` // nil: no status restriction
	Archived        bool     `json:"archived"`
	TimeFilter      string   `json:"time_filter"`
	TimeFilterLabel string   `json:"time_filter_label"`
	ActiveSince     string   `json:"active_since,omitempty"`
}

// viewInfo is the PWA-facing summary of the filter applied to a list.
type viewInfo struct {
	Label  string `json:"label"`  // e.g. "Open · Today"; "" when unfiltered
	Hidden int    `json:"hidden"` // sessions the filter removed
}

var statusFilterLabels = map[string]string{
	"active": "Open", "archived": "Archived", "running": "Running",
	"waiting": "Waiting", "idle": "Idle", "error": "Error",
}

type viewCacheEntry struct {
	v     *viewFilter // nil: no filter (unset, or the CLI lacks `view`)
	until time.Time   // the entry is fresh until then
	fails int         // consecutive exec failures, for backoff
}

// viewCall is one in-flight `view --json` exec that concurrent callers share.
type viewCall struct {
	done chan struct{}
	v    *viewFilter
}

type viewCache struct {
	mu    sync.Mutex // guards m and calls; never held across the exec
	m     map[string]viewCacheEntry
	calls map[string]*viewCall
}

func newViewCache() *viewCache {
	return &viewCache{m: map[string]viewCacheEntry{}, calls: map[string]*viewCall{}}
}

// viewFailMax caps the exponential backoff after failed execs (an old binary
// without `view` fails every time; it should not cost an exec per TTL).
const viewFailMax = 5 * time.Minute

// viewExecTimeout bounds the shared exec, which runs detached from any one
// caller's context so a cancelled request cannot poison the cache.
const viewExecTimeout = 10 * time.Second

// viewFilterFor returns the context profile's TUI filter, or nil. Concurrent
// callers for one profile share a single exec and none holds views.mu while
// it runs.
func (s *server) viewFilterFor(ctx context.Context) *viewFilter {
	profile := profileFrom(ctx, s.cfg.profile)
	vc := s.views
	vc.mu.Lock()
	if e, ok := vc.m[profile]; ok && time.Now().Before(e.until) {
		vc.mu.Unlock()
		return e.v
	}
	if c, ok := vc.calls[profile]; ok {
		vc.mu.Unlock()
		select {
		case <-c.done:
			return c.v
		case <-ctx.Done():
			return nil
		}
	}
	c := &viewCall{done: make(chan struct{})}
	vc.calls[profile] = c
	prev := vc.m[profile]
	vc.mu.Unlock()

	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), viewExecTimeout)
	defer cancel()
	var v viewFilter
	var got *viewFilter
	err := s.adeckJSON(ectx, &v, "view", "--json")
	ttl, fails := viewCacheTTL, 0
	if err == nil {
		got = &v
	} else {
		fails = prev.fails + 1
		ttl = viewCacheTTL << min(fails, 8)
		if ttl > viewFailMax {
			ttl = viewFailMax
		}
	}
	vc.mu.Lock()
	vc.m[profile] = viewCacheEntry{v: got, until: time.Now().Add(ttl), fails: fails}
	delete(vc.calls, profile)
	vc.mu.Unlock()
	c.v = got
	close(c.done)
	return got
}

// label renders the filter for the PWA chip; "" when it filters nothing.
func (v *viewFilter) label() string {
	if v == nil {
		return ""
	}
	var parts []string
	if v.StatusFilter != "" {
		l := statusFilterLabels[v.StatusFilter]
		if l == "" {
			l = v.StatusFilter
		}
		parts = append(parts, l)
	}
	if v.ActiveSince != "" {
		parts = append(parts, v.TimeFilterLabel)
	}
	return strings.Join(parts, " · ")
}

// keep reports whether the TUI's view shows se at now.
func (v *viewFilter) keep(se sessionInfo) bool {
	if v == nil {
		return true
	}
	if se.Archived != v.Archived {
		return false
	}
	if v.Statuses != nil && !v.Archived {
		ok := false
		for _, st := range v.Statuses {
			if st == se.Status {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if v.ActiveSince != "" {
		since, err := time.Parse(time.RFC3339, v.ActiveSince)
		if err == nil && !se.activityTime().IsZero() && se.activityTime().Before(since) {
			return false
		}
	}
	return true
}

// activityTime is the TUI's "last active" time when agent-deck supplied it,
// else the transcript's last reply.
func (se sessionInfo) activityTime() time.Time {
	if se.LastActivityAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, se.LastActivityAt); err == nil && !t.IsZero() {
			return t
		}
	}
	if se.LastActivity > 0 {
		return time.Unix(se.LastActivity, 0)
	}
	return time.Time{}
}

// applyView filters sessions by v, never hiding one that needs attention.
func applyView(sessions []sessionInfo, v *viewFilter) ([]sessionInfo, viewInfo) {
	info := viewInfo{Label: v.label()}
	if v == nil {
		return sessions, info
	}
	out := sessions[:0]
	for _, se := range sessions {
		if se.NeedsAttention || v.keep(se) {
			out = append(out, se)
		} else {
			info.Hidden++
		}
	}
	return out, info
}
