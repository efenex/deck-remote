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
)

// run-bang-phase2-review §5 #4: run:mut implies run:ro (and nothing else); act is a mint rule,
// never derived; the phone still runs ro only (runCapFor).
func TestRunCapsImplication(t *testing.T) {
	cases := []struct {
		caps []string
		want map[string]bool
	}{
		{[]string{"act", "run:ro"}, map[string]bool{"act": true, "run:ro": true, "run:mut": false, "run:secret": false}},
		{[]string{"act", "run:mut"}, map[string]bool{"act": true, "run:ro": true, "run:mut": true, "run:secret": false}},
		{[]string{"act", "run:secret"}, map[string]bool{"act": true, "run:ro": false, "run:mut": false, "run:secret": true}},
		{[]string{"act", "RUN:MUT"}, map[string]bool{"run:ro": true, "run:mut": true}},
		{[]string{"run:mut"}, map[string]bool{"act": false, "run:ro": true}}, // hand-edited store: act is not derived
		{nil, map[string]bool{"act": false, "run:ro": false, "run:mut": false, "run:secret": false}},
	}
	for _, c := range cases {
		dt := deviceToken{Name: "p", Caps: c.caps}
		for cap, want := range c.want {
			if got := dt.hasCap(cap); got != want {
				t.Errorf("caps %v: hasCap(%q) = %v, want %v", c.caps, cap, got, want)
			}
		}
	}
	if got := (deviceToken{Caps: []string{"act", "run:mut"}}).effectiveCaps(); strings.Join(got, ",") != "act,run:mut,run:ro" {
		t.Errorf("effectiveCaps = %v", got)
	}
	// Phase 2 W3: mut maps to run:mut (plus a passkey); secret stays phase 3.
	for cls, want := range map[string]string{"ro": "run:ro", "mut": "run:mut", "secret": "", "destructive": "", "interactive": "", "copy": ""} {
		if got := runCapFor(cls); got != want {
			t.Errorf("runCapFor(%q) = %q, want %q", cls, got, want)
		}
	}
	// run:* without act is still refused at mint.
	if _, _, err := newTokenStore(t.TempDir()).mint("x", "run:mut"); err == nil {
		t.Error("mint run:mut without act must fail")
	}
}

// Minting any run:* cap is refused unless the request comes from the laptop itself: a loopback
// peer with no serve headers. The shared token over serve (or from a LAN/tailnet peer) gets 403
// and one action-log line; plain caps still mint from anywhere with the shared token.
func TestRunCapMintLocalOnly(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".agent-deck")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_REMOTE_STATE_DIR", state)
	s := newServer(config{token: "shared", homeDir: home, profile: "default", towerOperator: "op@example.com", bin: "/nonexistent/agent-deck"}, &url.URL{Scheme: "http", Host: "127.0.0.1:1"})
	h := s.routes()
	mint := func(caps []string, remote string, hdr map[string]string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"name": "p", "caps": caps})
		req := httptest.NewRequest("POST", "/api/rc/devices/mint", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer shared")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	serve := map[string]string{"Tailscale-User-Login": "op@example.com", "X-Forwarded-For": "100.64.0.9"}
	refused := []struct {
		name   string
		caps   []string
		remote string
		hdr    map[string]string
	}{
		{"run:mut via serve", []string{"act", "run:mut"}, "127.0.0.1:1", serve},
		{"run:ro via serve", []string{"act", "run:ro"}, "127.0.0.1:1", serve},
		{"run:secret via serve", []string{"act", "run:secret"}, "127.0.0.1:1", serve},
		{"run:mut via serve, tagged node (no login)", []string{"act", "run:mut"}, "127.0.0.1:1", map[string]string{"X-Forwarded-For": "100.64.0.9"}},
		{"run:mut via funnel", []string{"act", "run:mut"}, "127.0.0.1:1", map[string]string{"Tailscale-Funnel-Request": "?1"}},
		{"run:mut from a tailnet peer", []string{"act", "run:mut"}, "100.64.0.9:1", nil},
		{"run:mut from a LAN peer (pf rdr)", []string{"act", "run:mut"}, "192.168.1.20:1", nil},
		{"Run:Mut spelled oddly via serve", []string{"act", " Run:Mut "}, "127.0.0.1:1", serve},
	}
	for _, c := range refused {
		rec := mint(c.caps, c.remote, c.hdr)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "laptop only") {
			t.Errorf("%s: got %d %s, want 403 laptop only", c.name, rec.Code, rec.Body.String())
		}
	}
	if n := len(s.tokens.list()); n != 0 {
		t.Fatalf("refused mints created %d tokens", n)
	}
	logb, _ := os.ReadFile(filepath.Join(state, "deck-remote-tower-actions.log"))
	if got := strings.Count(string(logb), `"action":"devices/mint"`); got != len(refused) {
		t.Errorf("action log has %d mint refusals, want %d:\n%s", got, len(refused), logb)
	}
	if strings.Contains(string(logb), "shared") {
		t.Error("action log must not carry the token")
	}

	// Local: loopback, no serve headers.
	rec := mint([]string{"act", "run:mut"}, "127.0.0.1:1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("local run:mut mint: %d %s", rec.Code, rec.Body.String())
	}
	rec6 := mint([]string{"act", "run:ro"}, "[::1]:1", nil)
	if rec6.Code != http.StatusOK {
		t.Fatalf("local ::1 run:ro mint: %d %s", rec6.Code, rec6.Body.String())
	}
	// Caps without run:* still mint over serve with the shared token.
	if rec := mint([]string{"act", "doc"}, "127.0.0.1:1", serve); rec.Code != http.StatusOK {
		t.Errorf("act,doc mint via serve: %d %s", rec.Code, rec.Body.String())
	}
	// A device token cannot mint at all (authShared), local or not.
	var minted struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &minted)
	req := httptest.NewRequest("POST", "/api/rc/devices/mint", strings.NewReader(`{"name":"q","caps":["act","run:mut"]}`))
	req.Header.Set("Authorization", "Bearer "+minted.Token)
	req.RemoteAddr = "127.0.0.1:1"
	drec := httptest.NewRecorder()
	h.ServeHTTP(drec, req)
	if drec.Code != http.StatusForbidden {
		t.Errorf("device-token mint: %d, want 403", drec.Code)
	}

	// whoami reports the implied cap so the PWA's run:ro check holds for a run:mut token.
	wreq := httptest.NewRequest("GET", "/api/rc/devices/whoami", nil)
	wreq.Header.Set("Authorization", "Bearer "+minted.Token)
	wrec := httptest.NewRecorder()
	h.ServeHTTP(wrec, wreq)
	if !strings.Contains(wrec.Body.String(), `"run:ro"`) {
		t.Errorf("whoami for a run:mut token must list run:ro: %s", wrec.Body.String())
	}

	// The shared token never carries a run:* cap: it is not a device token, and the run gate
	// refuses it before any cap check.
	if _, ok := s.tokens.find("shared"); ok {
		t.Error("shared token must not be a device token")
	}
	_, reason := s.actorOK(&http.Request{RemoteAddr: "127.0.0.1:1", Header: http.Header{"Tailscale-User-Login": {"op@example.com"}}}, "shared")
	if !strings.Contains(reason, "shared token cannot") {
		t.Errorf("shared token through the act gate: %q", reason)
	}
}
