package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"sync"
	"time"
)

// Watcher tuning. The sweep runs every watchInterval; a changed reply must be
// stable for replySettle before we treat the turn as "done, needs you" (avoids
// notifying on every mid-turn append).
const (
	watchInterval = 6 * time.Second
	replySettle   = 8 * time.Second
	// execStatsEvery paces the subprocess-counter log line (see runWatcher).
	execStatsEvery = 10 * time.Minute
)

type sessWatch struct {
	replyHash     uint64    // hash of the last-seen reply content
	changedAt     time.Time // when replyHash last changed
	notifiedHash  uint64    // hash we last pushed for (dedupe)
	permNotified  bool      // a permission push is outstanding for the current dialog
	stallNotified bool      // a stall push is outstanding for the current frozen run
	attentionID   string    // pending approval/question already pushed
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
	stats := time.NewTicker(execStatsEvery)
	defer stats.Stop()
	var lastAdeck, lastTmux int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-stats.C:
			// Subprocess budget log: agent-deck execs at steady state should be
			// ~0 (writes + rare fallbacks only). A climbing count here is the
			// CLI polling burn coming back — catch it in the log, not in
			// powermetrics DEAD_TASKS.
			a, tm := s.execAdeck.Load(), s.execTmux.Load()
			log.Printf("exec stats: agent-deck=%d tmux=%d (last %s)", a-lastAdeck, tm-lastTmux, execStatsEvery)
			lastAdeck, lastTmux = a, tm
			continue
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
		if se.Tool != "claude" && se.Tool != "codex" {
			continue
		}
		wg.Add(1)
		go func(se sessionInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Single capture-pane for this session, shared by activity caching and
			// (best-effort) permission/stall detection. paneFor (tmux-direct) —
			// the sweep already holds the resolved session.
			pctx, pc := context.WithTimeout(sctx, 5*time.Second)
			pane, paneErr := s.paneFor(pctx, se)
			pc()
			adapter := s.adapterFor(se)
			snap, snapErr := adapter.Snapshot(sctx, se, pane, paneErr == nil)
			if snapErr != nil || snap.DegradedReason != "" {
				s.attn.set(se.ID, attentionInfo{})
				return
			}
			var st activityState
			if se.Tool == "claude" {
				if cached, ok := s.acts.get(se.ID); ok {
					st = cached // claudeAdapter.Snapshot updated it from this same pane
				}
			} else {
				snap.Activity.stallKeySet = true
				if paneErr == nil && !snap.Attention.Pending {
					snap.Activity.stallKey = codexPaneProgressKey(pane)
				}
				st = s.acts.update(se.ID, snap.Activity)
				snap.Activity.Stalled = st.Stalled
			}
			s.attn.set(se.ID, snap.Attention)

			// Request/reply correlation is useful even without push subscribers.
			if se.Tool == "codex" {
				for _, reply := range s.codexReqs.reconcile(se.ID, snap) {
					s.hub.publish(map[string]any{
						"type": "reply", "requestId": reply.RequestID, "sessionId": se.ID,
						"content": reply.Message.Content, "ts": reply.Message.Ts,
					})
				}
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
			if se.Tool == "claude" {
				s.watchReply(sctx, se, sw, isNew)
			} else {
				s.watchSnapshotReply(se, sw, isNew, snap)
			}
			if se.Tool == "claude" && paneErr == nil {
				s.watchPermissionPane(se, sw, isNew, pane)
			}
			if se.Tool == "codex" {
				s.watchAttention(se, sw, isNew, snap.Attention)
			}
			s.watchStall(se, sw, isNew, st)
		}(se)
	}
	wg.Wait()
}

func (s *server) watchSnapshotReply(se sessionInfo, sw *sessWatch, isNew bool, snap harnessSnapshot) {
	content := strings.TrimSpace(snap.LastReply)
	if content == "" {
		return
	}
	h := hashStr(content)
	if isNew {
		sw.replyHash, sw.notifiedHash = h, h
		return
	}
	if h == sw.notifiedHash {
		return
	}
	sw.replyHash, sw.notifiedHash = h, h
	s.push.send(pushPayload{Title: se.Title, Body: content, SessionID: se.ID, Kind: "reply"})
}

func (s *server) watchAttention(se sessionInfo, sw *sessWatch, isNew bool, info attentionInfo) {
	if !stepAttentionNotification(sw, isNew, info) {
		return
	}
	body := "Permission requested — tap to review"
	kind := "approval"
	if info.Kind == "question" {
		body, kind = "Question waiting — tap to answer", "question"
		if len(info.Questions) > 0 {
			body = info.Questions[0].Question
		}
	}
	s.push.send(pushPayload{Title: se.Title, Body: body, SessionID: se.ID, Kind: kind})
}

func stepAttentionNotification(sw *sessWatch, isNew bool, info attentionInfo) bool {
	if !info.Pending {
		sw.attentionID = ""
		return false
	}
	if sw.attentionID == info.ID {
		return false
	}
	sw.attentionID = info.ID
	if isNew {
		return false
	}
	return true
}

func (s *server) watchReply(ctx context.Context, se sessionInfo, sw *sessWatch, isNew bool) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := s.claudeReply(rctx, se)
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
			Body:      content,
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

// stallNotifyAfter is how long a spinner label must stay frozen before we push a
// stall alert. The Stalled flag itself trips much sooner (>=stallThreshold polls,
// ~12s) and still drives the UI; short stalls typically resolve on their own, so
// we hold the notification back until the freeze has persisted this long to keep
// the alerts from being noise.
const stallNotifyAfter = 60 * time.Second

// stallShouldNotify is the PURE decision for watchStall: given the current stall
// signal and how long the label has been frozen, it returns whether to push now
// and the next value of the stallNotified latch. Kept side-effect-free and
// duration-injected so the 60s gate is unit-testable without a clock or a push.
func stallShouldNotify(stalled, isNew, notified bool, frozen time.Duration) (push, nextNotified bool) {
	if !stalled {
		return false, false // run ended (or never stalled) — clear the latch for next time
	}
	if isNew {
		return false, true // baseline a pre-existing stall; don't fire on first sight
	}
	if notified {
		return false, true // already pushed for this frozen run
	}
	if frozen < stallNotifyAfter {
		return false, false // frozen, but not long enough yet — re-check next poll, don't latch
	}
	return true, true
}

// watchStall pushes once when a session has been stalled (a spinner whose label
// has been frozen; see stepStall) for at least stallNotifyAfter. A pre-existing
// stall on first sight is baselined (no push) so a daemon restart doesn't fire for
// every already-frozen session. The body carries the frozen label + how long it
// has been frozen so a FALSE stall is easy to spot — stall detection is still
// best-effort and this surfaces its mistakes.
func (s *server) watchStall(se sessionInfo, sw *sessWatch, isNew bool, st activityState) {
	push, next := stallShouldNotify(st.Stalled, isNew, sw.stallNotified, time.Since(st.lastChangeAt))
	sw.stallNotified = next
	if !push {
		return
	}
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
