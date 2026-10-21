package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// The reverse-proxied agent-deck routes (terminal WebSocket, its API and SSE)
// must be bearer-gated like deck-remote's own, and agent-deck must only ever
// see the shared token, never the caller's.
func TestProxiedRoutesRequireAuth(t *testing.T) {
	var gotAuth, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".agent-deck"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := newServer(config{token: "shared", homeDir: home, profile: "default", towerOperator: "op@example.com"}, u)
	_, dev, err := s.tokens.mint("phone", "act")
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()

	for _, path := range []string{"/api/sessions", "/events/menu", "/ws/session/x"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without token: got %d, want 401", path, rec.Code)
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path+"?token=wrong", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with bad token: got %d, want 401", path, rec.Code)
		}
	}

	gotAuth, gotQuery = "", ""
	rec := httptest.NewRecorder()
	ws := httptest.NewRequest("GET", "/ws/session/x?token="+dev+"&cols=80", nil)
	ws.RemoteAddr = "127.0.0.1:5555"
	ws.Header.Set("Tailscale-User-Login", "op@example.com")
	h.ServeHTTP(rec, ws)
	if rec.Code != http.StatusOK {
		t.Fatalf("device token via query: got %d, want 200", rec.Code)
	}
	if gotAuth != "Bearer shared" {
		t.Errorf("upstream Authorization = %q, want the shared token", gotAuth)
	}
	if gotQuery != "cols=80" {
		t.Errorf("upstream query = %q, want the token stripped", gotQuery)
	}

	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+dev)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gotAuth != "Bearer shared" {
		t.Errorf("device token via header: code %d, upstream auth %q", rec.Code, gotAuth)
	}
}

// The phone's own origin is accepted and presented to agent-deck as agent-deck's
// own (its WebSocket origin check refused the phone's with 403); another
// site's origin is refused by deck-remote.
func TestProxiedRoutesOrigin(t *testing.T) {
	var gotOrigin string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin = r.Header.Get("Origin")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".agent-deck"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := newServer(config{token: "shared", homeDir: home, profile: "default", towerOperator: "op@example.com"}, u)
	_, dev, err := s.tokens.mint("phone", "act")
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()

	req := httptest.NewRequest("GET", "https://deck.example/ws/session/x?token="+dev, nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Tailscale-User-Login", "op@example.com")
	req.Header.Set("Origin", "https://deck.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || gotOrigin != "http://"+u.Host {
		t.Fatalf("same origin: code %d, upstream Origin %q", rec.Code, gotOrigin)
	}

	req = httptest.NewRequest("GET", "https://deck.example/ws/session/x?token="+dev, nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Tailscale-User-Login", "op@example.com")
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross origin: got %d, want 403", rec.Code)
	}
}
