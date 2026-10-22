package main

// Phone pairing by QR, scanned inside the PWA.
//
// Before this, setting up a phone meant minting a device token on the laptop, putting the raw
// token URL in a QR, opening it in Safari (whose storage is not the home-screen app's), re-adding
// the app, then typing a 6-digit passkey enrol code. Now:
//
//   POST /api/rc/devices/pair         shared token + laptop only (localClient) + {name, caps}.
//        Answers with a single-use pairing code (128 bits, pairTTL) and the URL to put in a QR:
//        <origin>/#pair=<code>. The code is in the fragment, which never reaches a server or a log.
//        Nothing is minted yet.
//   POST /api/rc/devices/pair/redeem  no bearer + {code}. The phone has no token yet: the code is
//        the credential. Spends the offer, mints the device token with the offer's name and caps,
//        and answers with the raw token, once. An act-cap token also gets a pairing grant: one
//        passkey enrol without a code within pairGrantTTL (passkeys.go checkEnrol).
//
// Why the grant may replace the enrol code (F9a): the code proves someone on the laptop asked for
// this enrolment, and the QR proves the same thing, with 128 bits instead of 20. The grant binds to
// the token the redeem just minted, so a stolen older bearer still needs a code. The residual is
// the same as for the code. Whoever reads the QR off the laptop screen inside pairTTL and redeems
// it first gets the token and the grant. The operator's own scan then fails with "already used",
// which is the signal to revoke. Before this, the QR held a raw token that never expired.
//
// Redeem must come from the laptop itself or through tailscale serve with a user identity (not a
// tagged node, not Funnel); for an offer with act caps that identity must be the operator, the same
// rule the act chain applies on every later request. Offers live in memory: a restart voids them.

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	pairTTL      = 3 * time.Minute
	pairGrantTTL = 5 * time.Minute
	pairMaxLive  = 4 // live offers; the oldest is dropped beyond this
)

type pairOffer struct {
	hash string // hashToken(code)
	id8  string // first 8 hex of the hash, for logs
	name string
	caps []string
	exp  time.Time
}

type pairHub struct {
	mu     sync.Mutex
	now    func() time.Time
	offers []pairOffer
}

func newPairHub() *pairHub { return &pairHub{now: time.Now} }

// offer stores a new offer and returns its raw code.
func (p *pairHub) offer(name string, caps []string) (string, pairOffer, error) {
	code, err := genSecret() // 32 bytes; the QR stays small at ~43 chars
	if err != nil {
		return "", pairOffer{}, err
	}
	h := hashToken(code)
	o := pairOffer{hash: h, id8: h[:8], name: name, caps: caps, exp: p.now().Add(pairTTL)}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked()
	p.offers = append(p.offers, o)
	if len(p.offers) > pairMaxLive {
		p.offers = p.offers[len(p.offers)-pairMaxLive:]
	}
	return code, o, nil
}

func (p *pairHub) pruneLocked() {
	now := p.now()
	live := p.offers[:0]
	for _, o := range p.offers {
		if now.Before(o.exp) {
			live = append(live, o)
		}
	}
	p.offers = live
}

// take spends the offer matching code. Every candidate is compared (constant time, no early
// return). An expired offer is reported as expired, not as unknown.
func (p *pairHub) take(code string) (pairOffer, string) {
	code = strings.TrimSpace(code)
	if code == "" {
		return pairOffer{}, "no pairing code"
	}
	want := hashToken(code)
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := -1
	for i := range p.offers {
		if subtle.ConstantTimeCompare([]byte(p.offers[i].hash), []byte(want)) == 1 {
			idx = i
		}
	}
	if idx < 0 {
		p.pruneLocked()
		return pairOffer{}, "unknown or already used pairing code: make a new QR on the laptop"
	}
	o := p.offers[idx]
	p.offers = append(p.offers[:idx], p.offers[idx+1:]...)
	if !p.now().Before(o.exp) {
		return o, "pairing code expired: make a new QR on the laptop"
	}
	return o, ""
}

// originURL is the PWA origin a phone should use: --public-url when set, else the request's
// forwarded host/proto (tailscale serve sets these), else Host.
func (s *server) originURL(r *http.Request) string {
	if base := strings.TrimRight(s.cfg.publicURL, "/"); base != "" {
		return base
	}
	scheme := "https"
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	} else if r.TLS == nil && r.Header.Get("X-Forwarded-For") == "" {
		scheme = "http"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// POST /api/rc/devices/pair {name, caps}. Behind authShared; laptop only for every offer, since
// redeeming one may enrol a passkey without a code.
func (s *server) handlePairOffer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string   `json:"name"`
		Caps []string `json:"caps"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	entry := map[string]any{"action": "devices/pair", "allowed": false, "caps": body.Caps, "login": tsLogin(r)}
	if why := localClient(r); why != "" {
		entry["reason"] = why
		s.towerLog(entry)
		httpError(w, http.StatusForbidden, "pairing QRs are made on the laptop only: "+why)
		return
	}
	caps, err := normCaps(body.Caps)
	if err != nil {
		entry["reason"] = err.Error()
		s.towerLog(entry)
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "device"
	}
	code, o, err := s.pairs.offer(name, caps)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "offer failed")
		return
	}
	entry["allowed"], entry["pair"], entry["name"] = true, o.id8, name
	s.towerLog(entry)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "caps": caps, "code": code,
		"url":       s.originURL(r) + "/#pair=" + code,
		"expiresIn": int(pairTTL / time.Second),
	})
}

// pairRedeemOK is the identity rule for a redeem; "" when allowed.
func (s *server) pairRedeemOK(r *http.Request, caps []string) string {
	if localClient(r) == "" {
		return "" // a process on the laptop itself: it already runs as the operator
	}
	if ok, why := viaServeProxy(r); !ok {
		return why
	}
	if r.Header.Get("Tailscale-Funnel-Request") != "" {
		return "pairing is not offered through Funnel"
	}
	login := tsLogin(r)
	if login == "" {
		return "no Tailscale-User-Login header (a tagged node or not through tailscale serve)"
	}
	if containsFold(caps, "act") && s.cfg.towerOperator != "" && !strings.EqualFold(login, s.cfg.towerOperator) {
		return "Tailscale identity is not the operator"
	}
	return ""
}

// POST /api/rc/devices/pair/redeem {code}. No bearer: the code is the credential.
func (s *server) handlePairRedeem(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "devices/pair/redeem", "allowed": false, "login": tsLogin(r)}
	refuse := func(code int, reason string) {
		entry["reason"] = reason
		s.towerLog(entry)
		httpError(w, code, reason)
	}
	if !s.passkeys.allow("pair-redeem") {
		refuse(http.StatusTooManyRequests, "too many pairing attempts; wait a minute")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		refuse(http.StatusBadRequest, "bad json")
		return
	}
	// Check identity before spending the offer, so a refused request leaves it for the real phone.
	// The caps the rule needs are only known after a match, so peek: take, and put back on refusal.
	o, why := s.pairs.take(body.Code)
	if o.id8 != "" {
		entry["pair"], entry["name"] = o.id8, o.name
	}
	if why != "" {
		refuse(http.StatusForbidden, why)
		return
	}
	if why := s.pairRedeemOK(r, o.caps); why != "" {
		s.pairs.mu.Lock()
		s.pairs.offers = append(s.pairs.offers, o)
		s.pairs.mu.Unlock()
		refuse(http.StatusForbidden, why)
		return
	}
	dt, raw, err := s.tokens.mint(o.name, o.caps...)
	if err != nil {
		refuse(http.StatusInternalServerError, "mint failed")
		return
	}
	grant := 0
	if dt.hasCap("act") {
		if _, why := s.webAuthn(); why == "" {
			s.passkeys.grantPair(dt.ID)
			grant = int(pairGrantTTL / time.Second)
		}
	}
	entry["allowed"], entry["device"], entry["passkeyGrant"] = true, dt.Name, grant > 0
	s.towerLog(entry)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": dt.ID, "name": dt.Name, "caps": dt.effectiveCaps(), "token": raw,
		"passkeyGrant": grant,
	})
}
