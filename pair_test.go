package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// offerPair asks for a pairing QR the way scripts/pair-phone.sh does: shared token, from the laptop.
func (e *pkEnv) offerPair(caps ...string) (code, url string) {
	e.t.Helper()
	w := e.req("POST", "/api/rc/devices/pair", e.shrd, map[string]any{"name": "iphone-test", "caps": caps}, local)
	if w.Code != http.StatusOK {
		e.t.Fatalf("pair offer: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Code, URL string
		ExpiresIn int
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	if out.ExpiresIn != int(pairTTL/time.Second) || !strings.HasSuffix(out.URL, "/#pair="+out.Code) || len(out.Code) < 40 {
		e.t.Fatalf("offer: %s", w.Body.String())
	}
	return out.Code, out.URL
}

func (e *pkEnv) redeem(code string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	return e.req("POST", "/api/rc/devices/pair/redeem", "", map[string]any{"code": code}, mod...)
}

func redeemed(t *testing.T, w *httptest.ResponseRecorder) (tok string, grant int) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Token        string
		PasskeyGrant int
		Caps         []string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Token, out.PasskeyGrant
}

func TestPairOfferIsLaptopOnly(t *testing.T) {
	e := pkServer(t)
	expectCode(t, "through serve", e.req("POST", "/api/rc/devices/pair", e.shrd, map[string]any{"caps": []string{"act"}}), 403, "laptop only")
	e.expectRefusedLog("devices/pair", "tailscale serve")
	expectCode(t, "device token", e.req("POST", "/api/rc/devices/pair", mintTok(t, e.s, "old-phone", "act"), map[string]any{}, local), 403, "admin token required")
	expectCode(t, "bad cap", e.req("POST", "/api/rc/devices/pair", e.shrd, map[string]any{"caps": []string{"run:mut"}}, local), 400, "needs act")
}

func TestPhoneEnrollmentRequiresPublicURL(t *testing.T) {
	for _, endpoint := range []string{"pair", "mint"} {
		t.Run(endpoint, func(t *testing.T) {
			e := pkServer(t)
			e.s.cfg.publicURL = ""
			body := map[string]any{"name": "phone", "caps": []string{"act", "run:mut"}}
			loopback := func(r *http.Request) { local(r); r.Host = "127.0.0.1:8765" }
			w := e.req("POST", "/api/rc/devices/"+endpoint, e.shrd, body, loopback)
			expectCode(t, "missing public origin", w, 409, "--public-url")
			if len(e.s.tokens.list()) != 0 || len(e.s.pairs.offers) != 0 {
				t.Error("missing origin created a token or offer")
			}
			e.s.cfg.publicURL = pkOrigin + "/"
			w = e.req("POST", "/api/rc/devices/"+endpoint, e.shrd, body, loopback)
			expectCode(t, "configured public origin", w, 200, pkOrigin+"/")
			if strings.Contains(w.Body.String(), "127.0.0.1") {
				t.Error("enrollment URL used listener origin")
			}
		})
	}
}

func TestPairRedeemMintsOnceAndEnrolsWithoutCode(t *testing.T) {
	e := pkServer(t)
	code, _ := e.offerPair("act", "run:ro", "run:mut")
	tok, grant := redeemed(t, e.redeem(code))
	if grant != int(pairGrantTTL/time.Second) {
		t.Fatalf("grant %d", grant)
	}
	dt, ok := e.s.tokens.find(tok)
	if !ok || dt.Name != "iphone-test" || !dt.hasCap("run:mut") {
		t.Fatalf("minted %+v ok=%v", dt, ok)
	}
	expectCode(t, "second redeem", e.redeem(code), 403, "already used")

	// The grant enrols one passkey with no code, once.
	a := newSoftAuth(t)
	w := e.begin(tok, "")
	if w.Code != http.StatusOK {
		t.Fatalf("begin with grant: %d %s", w.Code, w.Body.String())
	}
	if w = e.finish(tok, "", a.create(t, w.Body.Bytes())); w.Code != http.StatusOK {
		t.Fatalf("finish with grant: %d %s", w.Code, w.Body.String())
	}
	expectCode(t, "grant spent", e.begin(tok, ""), 403, "no pairing grant")

	// An older act token never had a grant: an empty code does nothing and does not burn a live code.
	old := mintTok(t, e.s, "old-phone", "act")
	live := e.issueCode()
	for i := 0; i < 4; i++ {
		expectCode(t, "empty code, no grant", e.begin(old, ""), 403, "no pairing grant")
	}
	if w := e.begin(old, live); w.Code != http.StatusOK {
		t.Fatalf("live code after empty tries: %d %s", w.Code, w.Body.String())
	}
}

func TestPairExpiryAndGrantExpiry(t *testing.T) {
	e := pkServer(t)
	e.s.pairs.now = func() time.Time { return e.clock }
	code, _ := e.offerPair("act")
	e.clock = e.clock.Add(pairTTL + time.Second)
	expectCode(t, "expired offer", e.redeem(code), 403, "expired")

	code, _ = e.offerPair("act")
	tok, _ := redeemed(t, e.redeem(code))
	e.clock = e.clock.Add(pairGrantTTL + time.Second)
	expectCode(t, "expired grant", e.begin(tok, ""), 403, "grant expired")
}

func TestPairRedeemIdentity(t *testing.T) {
	e := pkServer(t)
	code, _ := e.offerPair("act")
	actCode := code
	other := func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "someone@example.com") }
	expectCode(t, "act offer, not the operator", e.redeem(code, other), 403, "not the operator")
	noID := func(r *http.Request) { r.Header.Del("Tailscale-User-Login") }
	expectCode(t, "tagged node", e.redeem(code, noID), 403, "Tailscale-User-Login")
	lan := func(r *http.Request) { r.RemoteAddr = "192.168.1.20:5000" }
	expectCode(t, "LAN peer", e.redeem(code, lan), 403, "loopback")
	funnel := func(r *http.Request) { r.Header.Set("Tailscale-Funnel-Request", "?1") }
	expectCode(t, "funnel", e.redeem(code, funnel), 403, "Funnel")
	// Every refusal above left the offer for the operator's phone.
	redeemed(t, e.redeem(code))

	// A read-only offer accepts any tailnet user.
	code, _ = e.offerPair()
	if tok, grant := redeemed(t, e.redeem(code, other)); grant != 0 || tok == "" {
		t.Fatalf("read-only pair: grant %d", grant)
	}
	expectCode(t, "cross-site", e.redeem("x", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }), 403, "cross-origin")
	expectCode(t, "garbage", e.redeem("not-a-code"), 403, "unknown")
	e.expectRefusedLog("devices/pair/redeem", "unknown")
	for _, m := range actionLog(t) {
		if b, _ := json.Marshal(m); strings.Contains(string(b), code) || strings.Contains(string(b), actCode) {
			t.Fatalf("action log carries a pairing code: %s", b)
		}
	}
}

func TestPairRedeemRateLimited(t *testing.T) {
	e := pkServer(t)
	for i := 0; i < passkeyRateMax; i++ {
		code, _ := e.offerPair()
		redeemed(t, e.redeem(code))
	}
	code, _ := e.offerPair()
	expectCode(t, "rate", e.redeem(code), 429, "too many")
	e.clock = e.clock.Add(time.Minute)
	redeemed(t, e.redeem(code)) // throttling must leave the unspent offer usable
}

func TestPairRedeemRejectedTrafficCannotExhaustOperatorBudget(t *testing.T) {
	for _, scenario := range []string{"junk", "wrong-identity", "direct-peer"} {
		t.Run(scenario, func(t *testing.T) {
			e := pkServer(t)
			code, _ := e.offerPair("act")
			for i := 0; i <= passkeyRateMax; i++ {
				switch scenario {
				case "junk":
					e.redeem("not-a-code")
				case "wrong-identity":
					e.redeem(code, func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "other@example.com") })
				case "direct-peer":
					e.redeem(code, local, func(r *http.Request) { r.RemoteAddr = "192.168.1.20:5000" })
				}
			}
			if len(e.s.tokens.list()) != 0 {
				t.Fatal("refused traffic minted a token")
			}
			redeemed(t, e.redeem(code))
		})
	}
}
