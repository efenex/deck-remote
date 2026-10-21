package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wsTicketFor mints a terminal ticket the way terminal.html does: the bearer in the header,
// through serve with the operator's identity.
func wsTicketFor(t *testing.T, h http.Handler, tok, session string) string {
	t.Helper()
	rec := postWSTicket(h, tok, session, "op@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("ws ticket for %s: %d %s", session, rec.Code, rec.Body.String())
	}
	var out struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Ticket == "" || out.ExpiresIn != 30 {
		t.Fatalf("ws ticket body: %s", rec.Body.String())
	}
	return out.Ticket
}

func postWSTicket(h http.Handler, tok, session, login string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"session": session})
	req := httptest.NewRequest("POST", "/api/rc/ws/ticket", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// F12 = a (run-bang-phase2-review §5 #11): /ws/ takes a 30 s single-use ticket bound to the
// device token and the session; ?token= is refused; every issue and refusal is action-logged.
func TestWSTicket(t *testing.T) {
	var upstreamHits int
	var gotQuery, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	home := t.TempDir()
	state := filepath.Join(home, ".agent-deck")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_REMOTE_STATE_DIR", state)
	s := newServer(config{token: "shared", homeDir: home, profile: "default", towerOperator: "op@example.com", bin: "/nonexistent/agent-deck"}, u)
	clock := time.Unix(1_800_000_000, 0)
	s.wsTickets.now = func() time.Time { return clock }
	_, ro, _ := s.tokens.mint("ro-phone")
	actDT, act, _ := s.tokens.mint("act-phone", "act")
	_, act2, _ := s.tokens.mint("act-phone-2", "act")
	h := s.routes()

	open := func(path, login string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:1"
		if login != "" {
			req.Header.Set("Tailscale-User-Login", login)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	expect := func(name string, rec *httptest.ResponseRecorder, code int, body string) {
		t.Helper()
		if rec.Code != code || !strings.Contains(rec.Body.String(), body) {
			t.Errorf("%s: got %d %s, want %d containing %q", name, rec.Code, rec.Body.String(), code, body)
		}
	}

	// Valid ticket upgrades once; the second use is refused and never reaches agent-deck.
	tk := wsTicketFor(t, h, act, "sess-a")
	expect("first use", open("/ws/session/sess-a?ticket="+tk+"&cols=80", "op@example.com"), 200, "")
	if gotQuery != "cols=80" || gotAuth != "Bearer shared" {
		t.Errorf("upstream saw query %q auth %q; want the ticket stripped and the shared token", gotQuery, gotAuth)
	}
	expect("replay", open("/ws/session/sess-a?ticket="+tk, "op@example.com"), 403, "already used")

	// Expired: 30 s TTL.
	tk = wsTicketFor(t, h, act, "sess-a")
	clock = clock.Add(31 * time.Second)
	expect("expired", open("/ws/session/sess-a?ticket="+tk, "op@example.com"), 403, "expired")
	tk = wsTicketFor(t, h, act, "sess-a")
	clock = clock.Add(29 * time.Second)
	expect("just inside the TTL", open("/ws/session/sess-a?ticket="+tk, "op@example.com"), 200, "")

	// A ticket for session A does not open session B, and is spent by the attempt.
	tk = wsTicketFor(t, h, act, "sess-a")
	expect("wrong session", open("/ws/session/sess-b?ticket="+tk, "op@example.com"), 403, "another session")
	expect("spent by the wrong-session try", open("/ws/session/sess-a?ticket="+tk, "op@example.com"), 403, "already used")

	// Bearers on /ws/ are refused, even alongside a valid ticket.
	expect("?token= act", open("/ws/session/sess-a?token="+act, "op@example.com"), 403, "not accepted")
	tk = wsTicketFor(t, h, act, "sess-a")
	expect("?token= with a ticket", open("/ws/session/sess-a?ticket="+tk+"&token="+act, "op@example.com"), 403, "not accepted")
	req := httptest.NewRequest("GET", "/ws/session/sess-a", nil)
	req.Header.Set("Authorization", "Bearer "+act)
	req.Header.Set("Tailscale-User-Login", "op@example.com")
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expect("header bearer", rec, 403, "not accepted")
	expect("no credential", open("/ws/session/sess-a", "op@example.com"), 401, "")
	expect("bogus ticket", open("/ws/session/sess-a?ticket=nope", "op@example.com"), 403, "unknown")

	// The identity is checked again at the upgrade.
	tk = wsTicketFor(t, h, act, "sess-a")
	expect("no identity at upgrade", open("/ws/session/sess-a?ticket="+tk, ""), 403, "not the operator")
	tk = wsTicketFor(t, h, act, "sess-a")
	expect("other identity at upgrade", open("/ws/session/sess-a?ticket="+tk, "mallory@example.com"), 403, "not the operator")
	tk = wsTicketFor(t, h, act, "sess-a")
	req = httptest.NewRequest("GET", "/ws/session/sess-a?ticket="+tk, nil)
	req.Header.Set("Tailscale-User-Login", "op@example.com")
	req.RemoteAddr = "100.64.0.9:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	expect("non-loopback upgrade", rec, 403, "loopback")

	// Other /ws/ shapes are refused.
	tk = wsTicketFor(t, h, act, "sess-a")
	expect("other path", open("/ws/other?ticket="+tk, "op@example.com"), 403, "not a terminal path")

	// Minting a ticket: read-only, shared, no identity, ?token= → refused.
	expect("mint with ro", postWSTicket(h, ro, "sess-a", "op@example.com"), 403, "act")
	expect("mint with shared", postWSTicket(h, "shared", "sess-a", "op@example.com"), 403, "shared token")
	expect("mint without identity", postWSTicket(h, act, "sess-a", ""), 403, "Tailscale-User-Login")
	expect("mint without session", postWSTicket(h, act, "", "op@example.com"), 400, "session")
	expect("mint with a path in session", postWSTicket(h, act, "a/b", "op@example.com"), 400, "session")
	qreq := httptest.NewRequest("POST", "/api/rc/ws/ticket?token="+url.QueryEscape(act), strings.NewReader(`{"session":"sess-a"}`))
	qreq.Header.Set("Tailscale-User-Login", "op@example.com")
	qreq.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, qreq)
	expect("mint with ?token=", rec, 403, "device token required")

	// Revoking the device token voids its open tickets; another device's ticket still works.
	tk = wsTicketFor(t, h, act, "sess-a")
	tk2 := wsTicketFor(t, h, act2, "sess-a")
	if ok, err := s.tokens.revoke(actDT.ID); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	expect("revoked device's ticket", open("/ws/session/sess-a?ticket="+tk, "op@example.com"), 403, "revoked")
	expect("other device's ticket", open("/ws/session/sess-a?ticket="+tk2, "op@example.com"), 200, "")

	if upstreamHits != 3 {
		t.Errorf("agent-deck saw %d upgrades, want 3", upstreamHits)
	}

	// One action-log line per issue and per refusal; never the ticket or a token.
	logb, _ := os.ReadFile(filepath.Join(state, "deck-remote-tower-actions.log"))
	log := string(logb)
	issued := strings.Count(log, `"action":"ws/ticket","allowed":true`)
	if issued != 11 {
		t.Errorf("issued lines = %d, want 11", issued)
	}
	if n := strings.Count(log, `"action":"ws","allowed":false`); n != 13 {
		t.Errorf("ws refusal lines = %d, want 13:\n%s", n, log)
	}
	for _, secret := range []string{act, act2, tk, tk2} {
		if strings.Contains(log, secret) {
			t.Error("action log carries a secret")
		}
	}
}
