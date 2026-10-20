package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os/exec"
	"strings"
	"time"
)

// One long-lived `agent-deck recall follow` per open session (design F2).
//
// historyWindow used to run `recall timeline --since` (a full agent-deck start)
// at most every recallMinInterval for every open sheet: ~350 execs / 10 min with
// one phone open, 2.5k with several. A follower replaces that poll: the process
// streams row/update/remove frames (agent-deck polls the native file at 250 ms)
// and recallFollower applies them to the same cached rows, so a healthy
// session costs one exec for as long as its sheet is open.
//
// Lifecycle. The server never learns that a sheet opened or closed, so
// historyWindow calls are the signal: the first call starts a follower and it
// stops itself once nobody has asked for recallFollowIdle. A bounded number of
// followers run at once (recallMaxFollowers); the rest keep polling. A follower
// that exits is restarted with exponential backoff, resuming from the last
// cursor; while it is down, historyWindow polls, so the thread never goes stale.
// recallFollowFastExits quick exits in a row (recall off, a build without
// `follow`, a vanished transcript) give the session back to polling for
// recallFollowBlock.

const (
	recallMaxFollowers    = 8
	recallFollowIdle      = 60 * time.Second
	recallFollowTick      = 5 * time.Second
	recallFollowBackoffM  = 30 * time.Second
	recallFollowFastExit  = 3 * time.Second // an exit sooner than this after start is a "fast" one
	recallFollowFastExits = 3
	recallFollowBlock     = 2 * time.Minute
	// A healthy follower still gets a cheap `--since` check this often: it
	// notices a /clear (the follower is bound to one transcript file) and
	// anything a wedged follower missed.
	recallSafetyEvery = 2 * time.Minute
)

// recallFollowBackoff0 is the first restart delay (a var: tests shorten it).
var recallFollowBackoff0 = time.Second

// recallFollower is one running follow process for a session.
type recallFollower struct {
	cancel context.CancelFunc
	live   bool // a process is running right now (guarded by the session's mu)
}

// recallFrame is one JSONL line of `recall follow --jsonl`.
type recallFrame struct {
	Frame  string    `json:"frame"` // row | update | remove | resync_required | status
	Row    recallRow `json:"row"`
	ID     string    `json:"id"`
	Cursor string    `json:"cursor"`
	Reason string    `json:"reason"`
}

// applyFrame merges one follow frame into the cached rows (sv.mu held). It
// reports resync=true when the source changed under the cursor and the caller
// must reopen with a tail read.
func (sv *recallSess) applyFrame(f recallFrame) (resync bool) {
	switch f.Frame {
	case "row":
		sv.apply(recallTimeline{Turns: []recallRow{f.Row}})
	case "update":
		sv.apply(recallTimeline{Updates: []recallRow{f.Row}})
	case "remove":
		id := f.ID
		if id == "" {
			id = f.Row.ID
		}
		if id != "" {
			sv.apply(recallTimeline{Removed: []string{id}})
		}
	case "resync_required":
		return true
	}
	if f.Cursor != "" {
		sv.through = f.Cursor
	}
	return false
}

// followHealthy: rows are arriving by stream, so the poll can be skipped.
func (sv *recallSess) followHealthy() bool { return sv.fol != nil && sv.fol.live }

// ensureFollow starts a follower for the session when allowed (sv.mu held).
func (s *server) ensureFollow(sv *recallSess, id, profile string) {
	rs := s.recall
	if !rs.follow || sv.fol != nil || sv.through == "" || time.Now().Before(sv.followBlockUntil) {
		return
	}
	if !rs.acquireFollower() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	sv.fol = &recallFollower{cancel: cancel}
	go s.followLoop(ctx, sv, sv.fol, id, profile)
}

// stopFollow ends a session's follower (sv.mu held); safe to call without one.
func (sv *recallSess) stopFollow() {
	if sv.fol != nil {
		sv.fol.cancel()
		sv.fol = nil
	}
}

func (rs *recallStore) acquireFollower() bool {
	for {
		n := rs.nFollowers.Load()
		if n >= recallMaxFollowers {
			return false
		}
		if rs.nFollowers.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (rs *recallStore) releaseFollower() { rs.nFollowers.Add(-1) }

// followLoop runs the follow process until ctx is cancelled, the session goes
// idle, or the process keeps dying at once. It owns one slot of nFollowers.
func (s *server) followLoop(ctx context.Context, sv *recallSess, fol *recallFollower, id, profile string) {
	rs := s.recall
	defer rs.releaseFollower()
	defer func() {
		sv.mu.Lock()
		fol.live = false
		if sv.fol == fol {
			sv.fol = nil
		}
		sv.mu.Unlock()
	}()
	backoff, fast := recallFollowBackoff0, 0
	for ctx.Err() == nil {
		sv.mu.Lock()
		after := sv.through
		sv.mu.Unlock()
		if after == "" {
			return
		}
		started := time.Now()
		err, resync := s.runFollow(ctx, sv, fol, id, profile, after)
		if ctx.Err() != nil {
			return
		}
		if resync {
			// The source was rewritten or replaced: drop the cache; the next
			// historyWindow call reopens with a tail read and starts a new follower.
			sv.mu.Lock()
			sv.native, sv.rows, sv.through, sv.before = "", nil, "", ""
			sv.mu.Unlock()
			return
		}
		if err != nil && recallUnavailable(err) {
			rs.setOff()
			return
		}
		if time.Since(started) < recallFollowFastExit {
			fast++
		} else {
			fast, backoff = 0, recallFollowBackoff0
		}
		if fast >= recallFollowFastExits {
			sv.mu.Lock()
			sv.followBlockUntil = time.Now().Add(recallFollowBlock)
			sv.mu.Unlock()
			log.Printf("recall follow: %s exits at once (%v); polling instead", id, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > recallFollowBackoffM {
			backoff = recallFollowBackoffM
		}
	}
}

// runFollow spawns one follow process, applies its frames and returns when it
// ends. It counts as one agent-deck exec. resync=true: the source changed.
func (s *server) runFollow(ctx context.Context, sv *recallSess, fol *recallFollower, id, profile, after string) (err error, resync bool) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.execAdeck.Add(1)
	cmd := exec.CommandContext(pctx, s.cfg.bin, adeckArgs(profile, "recall", "follow", "--jsonl", "--after", after, id)...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	out, perr := cmd.StdoutPipe()
	if perr != nil {
		return perr, false
	}
	if perr = cmd.Start(); perr != nil {
		return perr, false
	}
	sv.mu.Lock()
	fol.live = true
	sv.mu.Unlock()

	// Idle watchdog: stop once nobody has asked for the session lately.
	go func() {
		t := time.NewTicker(recallFollowTick)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
				sv.mu.Lock()
				idle := time.Since(sv.lastAsk) > recallFollowIdle
				sv.mu.Unlock()
				if idle {
					cancelFollowOf(sv, fol)
					return
				}
			}
		}
	}()

	rd := bufio.NewReaderSize(out, 256*1024)
	for {
		line, rerr := rd.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var f recallFrame
			if json.Unmarshal(t, &f) == nil {
				sv.mu.Lock()
				stale := sv.fol != fol // replaced or stopped: its rows are no longer ours
				if !stale {
					resync = sv.applyFrame(f)
				}
				sv.mu.Unlock()
				if stale {
					break
				}
				if resync {
					cancel()
					break
				}
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				err = rerr
			}
			break
		}
	}
	werr := cmd.Wait()
	sv.mu.Lock()
	fol.live = false
	sv.mu.Unlock()
	if err == nil && werr != nil && !resync && ctx.Err() == nil {
		err = errors.New(strings.TrimSpace(stderr.String()) + ": " + werr.Error())
	}
	return err, resync
}

// cancelFollowOf stops fol (the idle watchdog's exit).
func cancelFollowOf(sv *recallSess, fol *recallFollower) {
	sv.mu.Lock()
	if sv.fol == fol {
		sv.fol = nil
	}
	sv.mu.Unlock()
	fol.cancel()
}

// limitedWriter keeps the first n bytes written (stderr of a follow process).
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
