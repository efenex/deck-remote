package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"time"
)

// Watcher tuning. The sweep runs every watchInterval; a changed reply must be
// stable for replySettle before we treat the turn as "done, needs you" (avoids
// notifying on every mid-turn append).
const (
	watchInterval = 6 * time.Second
	replySettle   = 8 * time.Second
)

type sessWatch struct {
	replyHash     uint64    // hash of the last-seen reply content
	changedAt     time.Time // when replyHash last changed
	notifiedHash  uint64    // hash we last pushed for (dedupe)
	permNotified  bool      // a permission push is outstanding for the current dialog
	stallNotified bool      // a stall push is outstanding for the current frozen run
}

func hashStr(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// runWatcher polls Claude sessions and pushes on RELIABLE events:
//   - a reply that has settled (changed, then stable for replySettle) -> "reply"
//   - a real permission dialog appearing on the pane -> "approval"
//
// Pre-existing replies are baselined on first sight (no push). Reply detection
// is transcript-based (name-independent, survives the stale-registry churn);
// permission detection is best-effort via the pane.
//
// The watcher stays a dumb producer: per-event toggles and quiet-hours are
// applied downstream by pushManager.send (via allow()), so it always emits both
// "reply" and "approval" payloads and lets send() decide whether to deliver.
//
// PROFILE SCOPE: the watcher is server-side with no per-client profile, so its
// sweep uses the daemon's default cfg.profile (ctx carries no override). Push
// therefore only covers the default profile; per-profile push would need N
// watchers (one per profile) and is out of scope. The PWA profile selector
// scopes only the on-demand CLI surface, not these background notifications.
func (s *server) runWatcher(ctx context.Context) {
	seen := map[string]*sessWatch{}
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Always refresh the activity cache (single pane-reader): endpoints read
		// it and must not go stale just because nobody is subscribed to push.
		s.sweep(ctx, seen, s.push.hasSubs())
	}
}

// sweep refreshes the per-session activity cache (always) and, when notify is
// true, also runs reply/permission push detection. The pane is captured ONCE
// per session here — this is the single place that does capture-pane + parse.
func (s *server) sweep(ctx context.Context, seen map[string]*sessWatch, notify bool) {
	sctx, cancel := context.WithTimeout(ctx, watchInterval-time.Second)
	defer cancel()
	sessions, err := s.listSessions(sctx)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex // guards `seen`
	for _, se := range sessions {
		if se.Tool != "claude" {
			continue
		}
		wg.Add(1)
		go func(se sessionInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Single capture-pane for this session, shared by activity caching and
			// (best-effort) permission/stall detection.
			pctx, pc := context.WithTimeout(sctx, 5*time.Second)
			pane, paneErr := s.sessionPane(pctx, se.ID)
			pc()
			var st activityState
			if paneErr == nil {
				st = s.acts.update(se.ID, parseActivity(pane))
			}

			if !notify {
				return
			}
			mu.Lock()
			sw, isNew := seen[se.ID], false
			if sw == nil {
				sw = &sessWatch{}
				seen[se.ID] = sw
				isNew = true
			}
			mu.Unlock()
			s.watchReply(sctx, se, sw, isNew)
			if paneErr == nil {
				s.watchPermissionPane(se, sw, isNew, pane)
				s.watchStall(se, sw, isNew, st)
			}
		}(se)
	}
	wg.Wait()
}

func (s *server) watchReply(ctx context.Context, se sessionInfo, sw *sessWatch, isNew bool) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := s.sessionReply(rctx, se.ID)
	if err != nil {
		return
	}
	// Sanitize raw/structured content (serialized tool_use / JSON blocks) before
	// hashing and pushing, so a settled-reply notification carries clean text and
	// the hash tracks the rendered content, not the raw JSON.
	content := cleanReplyContent(out.Content)
	if content == "" {
		return
	}
	h := hashStr(content)
	if isNew {
		// Baseline pre-existing reply; never push for what was already there.
		sw.replyHash, sw.notifiedHash, sw.changedAt = h, h, time.Time{}
		return
	}
	if h != sw.replyHash {
		sw.replyHash = h
		sw.changedAt = time.Now()
	}
	// Settled: changed, stable for replySettle, and not yet pushed.
	if h != sw.notifiedHash && !sw.changedAt.IsZero() && time.Since(sw.changedAt) >= replySettle {
		sw.notifiedHash = h
		log.Printf("watcher: reply settled session=%s", se.ID)
		s.push.send(pushPayload{
			Title:     se.Title,
			Body:      preview(content, 140),
			SessionID: se.ID,
			Kind:      "reply",
		})
	}
}

// watchPermissionPane runs permission detection off an already-captured pane
// (the sweep captures once and shares it with activity caching).
func (s *server) watchPermissionPane(se sessionInfo, sw *sessWatch, isNew bool, pane string) {
	if isClaudePermissionPrompt(pane) {
		if !sw.permNotified {
			sw.permNotified = true
			if !isNew { // don't fire for a dialog that was already up when we started
				log.Printf("watcher: permission dialog session=%s", se.ID)
				s.push.send(pushPayload{
					Title:     se.Title,
					Body:      "Permission requested — tap to review",
					SessionID: se.ID,
					Kind:      "approval",
				})
			}
		}
	} else {
		sw.permNotified = false
	}
}

// watchStall pushes once when a session transitions INTO the stalled state (a
// spinner whose label has been frozen across >=stallThreshold polls; see
// stepStall). A pre-existing stall on first sight is baselined (no push) so a
// daemon restart doesn't fire for every already-frozen session. The body carries
// the frozen label + how long it has been frozen so a FALSE stall is easy to spot
// — stall detection is still best-effort and this surfaces its mistakes.
func (s *server) watchStall(se sessionInfo, sw *sessWatch, isNew bool, st activityState) {
	if !st.Stalled {
		sw.stallNotified = false
		return
	}
	if isNew {
		sw.stallNotified = true // baseline a pre-existing stall; don't fire on first sight
		return
	}
	if sw.stallNotified {
		return // already pushed for this frozen run
	}
	sw.stallNotified = true
	log.Printf("watcher: stall detected session=%s activity=%q", se.ID, st.Activity)
	s.push.send(pushPayload{
		Title:     se.Title,
		Body:      stallBody(st),
		SessionID: se.ID,
		Kind:      "stall",
	})
}

// stallBody renders a diagnostic stall notification: the frozen spinner label (or
// current tool) and how long it has been frozen, so a spurious stall is obvious.
func stallBody(st activityState) string {
	label := st.Activity
	if label == "" {
		label = st.CurrentTool
	}
	if label == "" {
		label = "(no spinner label)"
	}
	frozen := time.Since(st.lastChangeAt).Round(time.Second)
	return fmt.Sprintf("Possibly stalled — spinner frozen %s at: %s", frozen, preview(label, 100))
}
