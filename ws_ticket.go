package main

// Terminal WebSocket tickets (F12 = a; docs/design/run-bang-phase2-review.md §5 #11).
//
// /ws/ is a live keyboard into a pane. A browser cannot set headers on a WebSocket, so before
// this the act bearer rode in ?token= on every terminal open (a referrer/screenshot leak of a
// long-lived secret). Now terminal.html first POSTs /api/rc/ws/ticket with the bearer in the
// Authorization header (authTyped: act cap + operator identity, header only) and gets a 30 s,
// single-use ticket bound to its device-token id and the session id; /ws/ takes ?ticket= only.
// Tickets live in memory: a restart voids them, which is fine at 30 s.

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const wsTicketTTL = 30 * time.Second

type wsTicket struct {
	device  string // device-token id
	session string
	exp     time.Time
}

type wsTicketStore struct {
	mu  sync.Mutex
	m   map[string]wsTicket // keyed by hashToken(ticket): the raw ticket is never kept
	now func() time.Time
}

func newWSTicketStore() *wsTicketStore {
	return &wsTicketStore{m: map[string]wsTicket{}, now: time.Now}
}

// issue returns a fresh ticket for (device id, session).
func (ts *wsTicketStore) issue(device, session string) (string, error) {
	raw, err := genSecret()
	if err != nil {
		return "", err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	now := ts.now()
	for k, t := range ts.m {
		if !now.Before(t.exp) {
			delete(ts.m, k)
		}
	}
	ts.m[hashToken(raw)] = wsTicket{device: device, session: session, exp: now.Add(wsTicketTTL)}
	return raw, nil
}

// redeem spends a ticket (whatever the outcome, a presented ticket is gone) and returns it
// when it existed and had not expired.
func (ts *wsTicketStore) redeem(raw string) (wsTicket, string) {
	if raw == "" {
		return wsTicket{}, "ticket required"
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	k := hashToken(raw)
	t, ok := ts.m[k]
	if !ok {
		return wsTicket{}, "unknown or already used ticket"
	}
	delete(ts.m, k)
	if !ts.now().Before(t.exp) {
		return wsTicket{}, "ticket expired"
	}
	return t, ""
}

// wsSessionID is the session id of a terminal path /ws/session/<id>, or "" for any other shape.
func wsSessionID(path string) string {
	id, ok := strings.CutPrefix(path, "/ws/session/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}

// handleWSTicket issues a ticket. Behind s.auth + s.authTyped, so the caller is an act-cap device
// token from the Authorization header with the operator's identity through serve.
func (s *server) handleWSTicket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	session := strings.TrimSpace(body.Session)
	dt, ok := s.tokens.find(bearerHeaderOnly(r))
	if !ok { // authTyped already required a device token; belt and braces
		httpError(w, http.StatusForbidden, "device token required")
		return
	}
	if session == "" || strings.ContainsAny(session, "/?#") {
		s.towerLog(map[string]any{"action": "ws/ticket", "allowed": false, "device": dt.Name, "login": tsLogin(r), "reason": "bad session id"})
		httpError(w, http.StatusBadRequest, "session id required")
		return
	}
	raw, err := s.wsTickets.issue(dt.ID, session)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "ticket failed")
		return
	}
	s.towerLog(map[string]any{"action": "ws/ticket", "allowed": true, "device": dt.Name, "login": tsLogin(r), "session": session})
	writeJSON(w, http.StatusOK, map[string]any{"ticket": raw, "expiresIn": int(wsTicketTTL / time.Second)})
}

// authWS gates /ws/: a ticket from ?ticket= only (never ?token=, never a header bearer), for
// this path's session, whose device token still exists with act, presented with the operator's
// identity through serve. Every refusal is action-logged.
func (s *server) authWS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		device := ""
		refuse := func(reason string) {
			s.towerLog(map[string]any{"action": "ws", "allowed": false, "device": device, "login": tsLogin(r), "reason": reason})
			httpError(w, http.StatusForbidden, "the terminal needs a ticket from /api/rc/ws/ticket: "+reason)
		}
		q := r.URL.Query()
		if !q.Has("ticket") && !q.Has("token") && r.Header.Get("Authorization") == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if q.Has("token") || r.Header.Get("Authorization") != "" {
			refuse("bearer tokens are not accepted on /ws/; use ?ticket=")
			return
		}
		session := wsSessionID(r.URL.Path)
		if session == "" {
			refuse("not a terminal path")
			return
		}
		t, why := s.wsTickets.redeem(q.Get("ticket"))
		if why != "" {
			refuse(why)
			return
		}
		dt, ok := s.tokens.byID(t.device)
		if !ok {
			refuse("the ticket's device token was revoked")
			return
		}
		device = dt.Name
		if t.session != session {
			refuse("ticket is for another session")
			return
		}
		if !dt.hasCap("act") {
			refuse("device token " + dt.Name + " lacks the act capability")
			return
		}
		if s.cfg.towerOperator == "" {
			refuse("actions disabled: --tower-operator not set")
			return
		}
		if ok, why := viaServeProxy(r); !ok {
			refuse(why)
			return
		}
		if login := tsLogin(r); !strings.EqualFold(login, s.cfg.towerOperator) || login == "" {
			refuse("Tailscale identity is not the operator")
			return
		}
		next.ServeHTTP(w, r)
	})
}
