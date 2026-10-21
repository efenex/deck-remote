package main

// The typed-input boundary (F7 = a; docs/design/run-bang-phase2-review.md §0 blocker 1).
//
// Everything that types into a session's pane, sends it keys or answers its prompt acts as the
// operator (since 8cd787e a phone send reaches the target as the operator's typed input). Those
// endpoints take the same gate as the tower actions: an act-cap device token from the
// Authorization header (never ?token=, never the shared token) plus the operator's Tailscale
// identity through `tailscale serve` (towerActionOK). Reads, the read-only SSE hub and push
// stay on s.auth.
//
// The terminal WebSocket (/ws/, agent-deck's pane over the proxy) is the same power, but a
// browser cannot set headers on a WebSocket, so there alone the token may come from ?token=;
// the act cap and the identity are still required.

import (
	"net/http"
	"strings"
)

// typedPaths are deck-remote's own endpoints that type into a pane or answer a prompt.
var typedPaths = []string{
	"POST /api/rc/ask",
	"POST /api/rc/slash",
	"POST /api/rc/approve",
	"POST /api/rc/attention/respond",
	"POST /api/rc/interrupt",
}

func typedRefusal(reason string) string {
	return "sending into a session needs an act-capable device token and the operator's identity: " + reason
}

// authTyped gates the typed-input endpoints. Refusals are action-logged like authAct's.
func (s *server) authTyped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		device, reason := s.towerActionOK(r)
		if reason != "" {
			s.towerLog(map[string]any{"action": r.URL.Path, "allowed": false, "device": device, "login": tsLogin(r), "reason": reason})
			httpError(w, http.StatusForbidden, typedRefusal(reason))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authProxyTyped gates the agent-deck reverse proxy: the terminal WebSocket needs act +
// identity (token from the header or, for the WebSocket only, ?token=); any other non-GET
// through the proxy (agent-deck's own write API) takes authTyped. GET/HEAD pass unchanged.
func (s *server) authProxyTyped(next http.Handler) http.Handler {
	typed := s.authTyped(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ws/") {
			device, reason := s.actorOK(r, bearer(r))
			if reason != "" {
				s.towerLog(map[string]any{"action": "ws", "allowed": false, "device": device, "login": tsLogin(r), "reason": reason})
				httpError(w, http.StatusForbidden, "the terminal "+typedRefusal(reason)[len("sending into a session "):])
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			typed.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
