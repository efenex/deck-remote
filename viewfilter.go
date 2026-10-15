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
	v  *viewFilter // nil: no filter (unset, or the CLI lacks `view`)
	at time.Time
}

type viewCache struct {
	mu sync.Mutex
	m  map[string]viewCacheEntry
}

func newViewCache() *viewCache { return &viewCache{m: map[string]viewCacheEntry{}} }

// viewFilterFor returns the context profile's TUI filter, or nil.
func (s *server) viewFilterFor(ctx context.Context) *viewFilter {
	profile := profileFrom(ctx, s.cfg.profile)
	s.views.mu.Lock()
	defer s.views.mu.Unlock()
	if e, ok := s.views.m[profile]; ok && time.Since(e.at) < viewCacheTTL {
		return e.v
	}
	var v viewFilter
	var got *viewFilter
	if err := s.adeckJSON(ctx, &v, "view", "--json"); err == nil {
		got = &v
	}
	s.views.m[profile] = viewCacheEntry{v: got, at: time.Now()}
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
