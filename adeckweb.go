package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Session-list source. The old code shelled `agent-deck list --json` for EVERY
// list/findSession — the watcher every 6s plus each PWA probe — and each spawn
// is a full Go-binary startup + registry load. That process churn (invisible to
// top; powermetrics DEAD_TASKS ~2 cores on this machine) is the CLI polling
// burn this file removes: the agent-deck web server we already proxy to serves
// the same list from an in-memory snapshot over HTTP, so we ask it first and
// fall back to the CLI only when the web server is unreachable or serves a
// different profile (CLI-first stays the contract, exec becomes the exception).
// A short TTL cache in front collapses the many per-request findSession lookups
// into at most one upstream fetch per listCacheTTL.

// listCacheTTL bounds staleness of the session list served to findSession and
// the endpoints. Shorter than the 6s watcher sweep so the watcher itself always
// refetches; long enough to absorb the PWA's 3s activity probes.
const listCacheTTL = 2500 * time.Millisecond

// adeckWebTimeout caps the HTTP list fetch; the CLI fallback has its own
// caller-supplied deadline.
const adeckWebTimeout = 4 * time.Second

type listCacheEntry struct {
	sessions []sessionInfo
	at       time.Time
}

type listCache struct {
	mu sync.Mutex // also serializes upstream fetches (single-flight per cache)
	m  map[string]listCacheEntry
}

func newListCache() *listCache { return &listCache{m: map[string]listCacheEntry{}} }

// menuSessionWire is the subset of agent-deck web's MenuSession we consume
// (GET /api/sessions). Field names are the web API's camelCase, mapped onto the
// CLI-shaped sessionInfo the rest of the daemon speaks.
type menuSessionWire struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Tool            string `json:"tool"`
	Status          string `json:"status"`
	GroupPath       string `json:"groupPath"`
	ProjectPath     string `json:"projectPath"`
	TmuxSession     string `json:"tmuxSession"`
	TmuxSocketName  string `json:"tmuxSocketName"`
	Model           string `json:"model"`
	ClaudeSessionID string `json:"claudeSessionId"`
	CodexSessionID  string `json:"codexSessionId"`
	SSHHost         string `json:"sshHost"`
}

type sessionsWireResp struct {
	Sessions []menuSessionWire `json:"sessions"`
	Profile  string            `json:"profile"`
}

func mapMenuSession(m menuSessionWire, profile string) sessionInfo {
	return sessionInfo{
		ID:              m.ID,
		Title:           m.Title,
		Path:            m.ProjectPath,
		Group:           m.GroupPath,
		Tool:            m.Tool,
		Status:          m.Status,
		TmuxSession:     m.TmuxSession,
		TmuxSocket:      m.TmuxSocketName,
		Profile:         profile,
		Model:           m.Model,
		ClaudeSessionID: m.ClaudeSessionID,
		CodexSessionID:  m.CodexSessionID,
		SSHHost:         m.SSHHost,
	}
}

// httpListSessions fetches the session list from the agent-deck web server the
// daemon already proxies to. Returns the sessions and the profile that server
// is bound to (it serves exactly one).
func (s *server) httpListSessions(ctx context.Context) ([]sessionInfo, string, error) {
	ctx, cancel := context.WithTimeout(ctx, adeckWebTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.agentdeckURL+"/api/sessions", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("agent-deck web /api/sessions: %s", resp.Status)
	}
	var wire sessionsWireResp
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, "", fmt.Errorf("decode /api/sessions: %w", err)
	}
	out := make([]sessionInfo, 0, len(wire.Sessions))
	for _, m := range wire.Sessions {
		out = append(out, mapMenuSession(m, wire.Profile))
	}
	return out, wire.Profile, nil
}

// listSessions returns the session list for the context's profile: TTL cache →
// agent-deck web HTTP (when it serves that profile) → stock CLI. Callers get a
// fresh copy — handleSessions mutates elements in place, and the cache must not
// see that.
func (s *server) listSessions(ctx context.Context) ([]sessionInfo, error) {
	profile := profileFrom(ctx, s.cfg.profile)
	s.lists.mu.Lock()
	defer s.lists.mu.Unlock()
	if e, ok := s.lists.m[profile]; ok && time.Since(e.at) < listCacheTTL {
		return append([]sessionInfo(nil), e.sessions...), nil
	}
	sessions, gotProfile, err := s.httpListSessions(ctx)
	if err != nil || gotProfile != profile {
		// Web server down, or bound to another profile than the one requested:
		// shell the CLI (profile-scoped via the global -p flag).
		sessions, err = s.cliListSessions(ctx)
		if err != nil {
			return nil, err
		}
	}
	s.lists.m[profile] = listCacheEntry{sessions: sessions, at: time.Now()}
	return append([]sessionInfo(nil), sessions...), nil
}
