package main

import (
	"sync"
	"time"
)

// activityState is the cached, parsed live-activity for a single session. The
// 6s watcher is the SINGLE component that captures the pane and parses it; both
// handleSessions and handleActivity READ from this cache instead of each doing
// their own capture (fresh-wins-over-stale becomes automatic, and we drop the
// redundant per-request capture-pane).
type activityState struct {
	Working      bool
	Activity     string
	CurrentTool  string
	Stalled      bool      // working, but the activity string hasn't moved for >=2 polls
	lastActivity string    // last parsed activity string, for stall comparison
	sameCount    int       // consecutive polls with an unchanged activity string while working
	lastChangeAt time.Time // when the activity string last changed
	updatedAt    time.Time // when this entry was last refreshed (freshness)
}

// activityCache is a concurrency-safe per-session store of parsed activity.
type activityCache struct {
	mu sync.RWMutex
	m  map[string]activityState
}

func newActivityCache() *activityCache {
	return &activityCache{m: map[string]activityState{}}
}

// stallThreshold is the number of consecutive unchanged polls (while working)
// before we flag the spinner as stalled. With a 6s watch interval, >=2 means a
// spinner frozen for ~12s+ is surfaced as "stalled" rather than "live working".
const stallThreshold = 2

// stepStall is the PURE stall-detection state transition: given the previous
// cached state and a freshly parsed activity, it returns the next state with
// Stalled/sameCount/lastChangeAt updated. Kept side-effect-free and time-injected
// so it can be unit-tested without a pane or a clock.
func stepStall(prev activityState, parsed activityInfo, now time.Time) activityState {
	next := activityState{
		Working:     parsed.Working,
		Activity:    parsed.Activity,
		CurrentTool: parsed.CurrentTool,
		updatedAt:   now,
	}
	if !parsed.Working {
		// Not working: never stalled; reset the run so a future spinner starts clean.
		next.lastActivity = parsed.Activity
		next.sameCount = 0
		next.lastChangeAt = now
		return next
	}
	// Working: track whether the activity string moved since last poll. An empty
	// activity string (spinner present but no parsed label) does not count as a
	// stall signal — we only stall on a concrete, unchanging label.
	if parsed.Activity != "" && parsed.Activity == prev.lastActivity {
		next.sameCount = prev.sameCount + 1
		next.lastChangeAt = prev.lastChangeAt
		if next.lastChangeAt.IsZero() {
			next.lastChangeAt = now
		}
	} else {
		next.sameCount = 1
		next.lastChangeAt = now
	}
	next.lastActivity = parsed.Activity
	// Stalled once the same concrete label has persisted across >=stallThreshold
	// polls. working stays true — we add a separate flag, never silently flip it.
	next.Stalled = parsed.Activity != "" && next.sameCount >= stallThreshold
	return next
}

// update applies stall detection for one session and stores the result.
func (c *activityCache) update(id string, parsed activityInfo) activityState {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := stepStall(c.m[id], parsed, time.Now())
	c.m[id] = next
	return next
}

// get returns the cached state and whether an entry exists.
func (c *activityCache) get(id string) (activityState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.m[id]
	return st, ok
}

// info projects the cached state onto the wire shape the endpoints return.
func (st activityState) info() activityInfo {
	return activityInfo{
		Working:     st.Working,
		Activity:    st.Activity,
		CurrentTool: st.CurrentTool,
		Stalled:     st.Stalled,
	}
}
