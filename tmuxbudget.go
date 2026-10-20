package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tmux exec budget (task deckremote-poll-budget, 2026-10-21).
//
// The 6 s watcher sweep captured the pane of EVERY listed session, dead ones
// included: ~94-170 `tmux capture-pane -S -2000` execs per sweep (~25/s, 15-17k
// per 10 min) of which most just answered "can't find session". Three changes:
//
//  1. One `tmux list-panes -a -F #{session_name}` per socket per sweep gives the
//     set of live sessions. A session absent from it is gone: no capture, no
//     show-environment (tmuxLive).
//  2. Every tmux exec (capture-pane, list-panes, show-environment) passes one
//     pacing gate: at most one exec per tmuxInterval (500 ms = 2/s sustained,
//     no burst). A caller whose slot would fall past its deadline is refused
//     (errTmuxThrottled) without running anything; the sweep skips that session
//     for this tick and serves the longest-unrefreshed ones first next tick.
//  3. Exponential backoff after a tmux fault (server down, socket error, exec
//     failure): 1 s, 2 s, 4 s … 30 s, reset by the first success. A definitive
//     per-session answer (can't find session, unknown variable) is not a fault.

const (
	tmuxInterval   = 500 * time.Millisecond
	tmuxBackoffMin = time.Second
	tmuxBackoffMax = 30 * time.Second
	// tmuxLiveFresh: how long a list-panes answer may serve captures that are not
	// part of a sweep (a phone request between two sweeps).
	tmuxLiveFresh = 15 * time.Second
	// tmuxMaxWait caps a wait for a slot when the caller's context has no deadline.
	tmuxMaxWait = 30 * time.Second
)

// errTmuxThrottled: the budget (pacing or backoff) refused the exec. Nothing was
// run, and the session's state is unknown, not gone: callers must not fall back
// to a different exec path.
var errTmuxThrottled = errors.New("tmux exec budget exhausted")

type tmuxLiveSet struct {
	at    time.Time
	names map[string]bool
}

type tmuxBudget struct {
	mu       sync.Mutex
	interval time.Duration // 0 = unpaced (tests)
	next     time.Time     // earliest start of the next exec
	backoff  time.Duration
	until    time.Time // no exec before this (backoff)
	live     map[string]tmuxLiveSet
	paneAt   map[string]time.Time // last pane refresh per session, for sweep ordering

	throttled atomic.Int64
	faults    atomic.Int64
}

func newTmuxBudget() *tmuxBudget {
	return &tmuxBudget{interval: tmuxInterval, live: map[string]tmuxLiveSet{}, paneAt: map[string]time.Time{}}
}

// acquire reserves the next exec slot and sleeps until it. It refuses (without
// consuming a slot) when the slot would start after ctx's deadline.
func (b *tmuxBudget) acquire(ctx context.Context) error {
	b.mu.Lock()
	now := time.Now()
	slot := now
	if b.next.After(slot) {
		slot = b.next
	}
	if b.until.After(slot) {
		slot = b.until
	}
	limit := now.Add(tmuxMaxWait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(limit) {
		limit = dl
	}
	if slot.After(limit) {
		b.mu.Unlock()
		b.throttled.Add(1)
		return errTmuxThrottled
	}
	b.next = slot.Add(b.interval)
	b.mu.Unlock()
	if d := time.Until(slot); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// fault records a tmux-level failure and doubles the backoff; ok resets it.
func (b *tmuxBudget) fault() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.backoff == 0 {
		b.backoff = tmuxBackoffMin
	} else if b.backoff *= 2; b.backoff > tmuxBackoffMax {
		b.backoff = tmuxBackoffMax
	}
	b.until = time.Now().Add(b.backoff)
	b.faults.Add(1)
}

func (b *tmuxBudget) ok() {
	b.mu.Lock()
	b.backoff, b.until = 0, time.Time{}
	b.mu.Unlock()
}

func (b *tmuxBudget) setLive(socket string, names map[string]bool) {
	b.mu.Lock()
	b.live[socket] = tmuxLiveSet{at: time.Now(), names: names}
	b.mu.Unlock()
}

// gone reports that a fresh list-panes answer for the socket does not list the
// session. An unknown or stale answer is "not known to be gone".
func (b *tmuxBudget) gone(socket, name string) bool {
	if name == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ls, ok := b.live[socket]
	return ok && time.Since(ls.at) < tmuxLiveFresh && !ls.names[name]
}

func (b *tmuxBudget) touched(id string) {
	b.mu.Lock()
	b.paneAt[id] = time.Now()
	b.mu.Unlock()
}

// tmuxFault: an error that says tmux itself is unhealthy, as opposed to a
// definitive answer about one session.
func tmuxFault(err error) bool {
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return true // could not start, killed by timeout…
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "no server running") || strings.Contains(m, "error connecting to") ||
		strings.Contains(m, "server exited") || strings.Contains(m, "lost server")
}

// tmuxExec runs one tmux command through the budget. socket "" = tmux's default.
func (s *server) tmuxExec(ctx context.Context, socket string, args ...string) ([]byte, error) {
	if err := s.tmux.acquire(ctx); err != nil {
		return nil, err
	}
	bin := s.cfg.tmuxBin
	if bin == "" {
		bin = "tmux"
	}
	if socket != "" {
		args = append([]string{"-L", socket}, args...)
	}
	s.execTmux.Add(1)
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		err = fmt.Errorf("tmux %s: %w: %s", args[0], err, stderr)
		if tmuxFault(err) && ctx.Err() == nil {
			s.tmux.fault()
		} else if ctx.Err() == nil {
			s.tmux.ok() // tmux answered
		}
		return out, err
	}
	s.tmux.ok()
	return out, nil
}

// refreshLive runs one list-panes per distinct socket among the sessions and
// stores the live-session sets. A refused or failed list leaves the previous
// answer in place (it simply ages out); "no server" means nothing is alive there.
func (s *server) refreshLive(ctx context.Context, sessions []sessionInfo) {
	seen := map[string]bool{}
	for _, se := range sessions {
		if se.TmuxSession == "" || seen[se.TmuxSocket] {
			continue
		}
		seen[se.TmuxSocket] = true
		out, err := s.tmuxExec(ctx, se.TmuxSocket, "list-panes", "-a", "-F", "#{session_name}")
		if err != nil {
			if tmuxSessionGone(err) { // no server on the socket
				s.tmux.setLive(se.TmuxSocket, map[string]bool{})
			}
			continue
		}
		names := map[string]bool{}
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				names[l] = true
			}
		}
		s.tmux.setLive(se.TmuxSocket, names)
	}
}

// order sorts sessions in place for a sweep: working or waiting ones first (the
// phone cares most about them), then by how long ago their pane was refreshed.
func (b *tmuxBudget) order(sessions []sessionInfo) {
	b.mu.Lock()
	defer b.mu.Unlock()
	hot := func(se sessionInfo) int {
		if se.Status == "running" || se.Status == "waiting" {
			return 0
		}
		return 1
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		if hi, hj := hot(sessions[i]), hot(sessions[j]); hi != hj {
			return hi < hj
		}
		return b.paneAt[sessions[i].ID].Before(b.paneAt[sessions[j].ID])
	})
}
