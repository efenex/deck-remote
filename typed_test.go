package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F7 = a: every endpoint that types into a pane, sends keys or answers a prompt takes the
// act cap + operator identity, with the token from the Authorization header only. Reads and
// the proxied GETs keep working with any token.
func TestTypedInputBoundary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".agent-deck"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_REMOTE_STATE_DIR", filepath.Join(home, ".agent-deck"))
	s := newServer(config{token: "shared", homeDir: home, profile: "default", towerOperator: "op@example.com", bin: "/nonexistent/agent-deck"}, u)
	_, ro, _ := s.tokens.mint("ro-phone")
	_, doc, _ := s.tokens.mint("doc-phone", "doc")
	_, act, _ := s.tokens.mint("act-phone", "act")
	h := s.routes()

	type kind struct {
		name    string
		tok     string
		query   bool // token in ?token= instead of the header
		login   string
		remote  string
		allowed bool
	}
	kinds := []kind{
		{"none", "", false, "op@example.com", "127.0.0.1:1", false},
		{"shared", "shared", false, "op@example.com", "127.0.0.1:1", false},
		{"read-only device", ro, false, "op@example.com", "127.0.0.1:1", false},
		{"doc device", doc, false, "op@example.com", "127.0.0.1:1", false},
		{"act without identity", act, false, "", "127.0.0.1:1", false},
		{"act, wrong identity", act, false, "mallory@example.com", "127.0.0.1:1", false},
		{"act, not via serve (non-loopback peer)", act, false, "op@example.com", "100.64.0.9:1", false},
		{"act via ?token=", act, true, "op@example.com", "127.0.0.1:1", false},
		{"act with identity", act, false, "op@example.com", "127.0.0.1:1", true},
	}
	do := func(method, path string, k kind) *httptest.ResponseRecorder {
		p := path
		if k.query && k.tok != "" {
			p += "?token=" + url.QueryEscape(k.tok)
		}
		req := httptest.NewRequest(method, p, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if !k.query && k.tok != "" {
			req.Header.Set("Authorization", "Bearer "+k.tok)
		}
		if k.login != "" {
			req.Header.Set("Tailscale-User-Login", k.login)
		}
		req.RemoteAddr = k.remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	targets := append([]string{}, typedPaths...)
	targets = append(targets, "POST /api/sessions/x/send") // agent-deck's write API over the proxy
	for _, tp := range targets {
		method, path, _ := strings.Cut(tp, " ")
		for _, k := range kinds {
			rec := do(method, path, k)
			denied := rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden
			if k.allowed && denied {
				t.Errorf("%s as %s: got %d, want through the gate (%s)", tp, k.name, rec.Code, rec.Body.String())
			}
			if !k.allowed {
				want := http.StatusForbidden
				if k.tok == "" {
					want = http.StatusUnauthorized
				}
				if rec.Code != want {
					t.Errorf("%s as %s: got %d, want %d", tp, k.name, rec.Code, want)
				}
				if want == http.StatusForbidden && !strings.Contains(rec.Body.String(), "act-capable device token") {
					t.Errorf("%s as %s: refusal must say what is needed: %s", tp, k.name, rec.Body.String())
				}
			}
		}
	}

	// The terminal WebSocket takes a ticket only (ws_ticket.go): minted through the typed gate
	// above (the ticket endpoint is in its matrix below), never ?token= or a header bearer.
	for _, c := range []struct {
		k    kind
		want int
	}{
		{kind{"act ws query", act, true, "op@example.com", "127.0.0.1:1", false}, 403},
		{kind{"act ws header", act, false, "op@example.com", "127.0.0.1:1", false}, 403},
		{kind{"ro ws query", ro, true, "op@example.com", "127.0.0.1:1", false}, 403},
		{kind{"shared ws query", "shared", true, "op@example.com", "127.0.0.1:1", false}, 403},
	} {
		if rec := do("GET", "/ws/session/x", c.k); rec.Code != c.want {
			t.Errorf("/ws/ as %s: got %d, want %d", c.k.name, rec.Code, c.want)
		}
	}
	for _, k := range kinds {
		rec := do("POST", "/api/rc/ws/ticket", k)
		denied := rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden
		if k.allowed == denied {
			t.Errorf("POST /api/rc/ws/ticket as %s: got %d (%s)", k.name, rec.Code, rec.Body.String())
		}
	}

	// Reads keep working for every valid token, header or ?token=.
	for _, k := range []kind{{"ro header", ro, false, "", "192.0.2.1:1", true}, {"ro query", ro, true, "", "192.0.2.1:1", true}, {"shared query", "shared", true, "", "192.0.2.1:1", true}} {
		for _, path := range []string{"/api/rc/version", "/api/sessions", "/events/menu"} {
			if rec := do("GET", path, k); rec.Code != http.StatusOK {
				t.Errorf("GET %s as %s: got %d, want 200", path, k.name, rec.Code)
			}
		}
	}
}
