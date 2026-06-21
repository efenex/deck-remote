package main

import (
	"context"
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
	replyHash    uint64    // hash of the last-seen reply content
	changedAt    time.Time // when replyHash last changed
	notifiedHash uint64    // hash we last pushed for (dedupe)
	permNotified bool      // a permission push is outstanding for the current dialog
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
		if !s.push.hasSubs() {
			continue // nothing to notify
		}
		s.sweep(ctx, seen)
	}
}

func (s *server) sweep(ctx context.Context, seen map[string]*sessWatch) {
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
			mu.Lock()
			sw, isNew := seen[se.ID], false
			if sw == nil {
				sw = &sessWatch{}
				seen[se.ID] = sw
				isNew = true
			}
			mu.Unlock()
			s.watchReply(sctx, se, sw, isNew)
			s.watchPermission(sctx, se, sw, isNew)
		}(se)
	}
	wg.Wait()
}

func (s *server) watchReply(ctx context.Context, se sessionInfo, sw *sessWatch, isNew bool) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := s.sessionReply(rctx, se.ID)
	if err != nil || out.Content == "" {
		return
	}
	h := hashStr(out.Content)
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
			Body:      preview(out.Content, 140),
			SessionID: se.ID,
			Kind:      "reply",
		})
	}
}

func (s *server) watchPermission(ctx context.Context, se sessionInfo, sw *sessWatch, isNew bool) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pane, err := s.sessionPane(pctx, se.ID)
	if err != nil {
		return // stale tmux name / not readable — best-effort only
	}
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
