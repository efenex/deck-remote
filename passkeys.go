package main

// Run-bang phase 2 #5: passkeys (docs/design/run-bang.md §3.4, run-bang-phase2-review.md §2.1,
// §5 W2, ruling F9a).
//
// A mutating `! ` line from the phone needs one WebAuthn assertion with user verification (Face
// ID) per line. This file is the credential side; the run endpoint that consumes an assertion is
// W3 (tower_run.go), so nothing here makes a mut line runnable: runCapFor stays ro-only.
//
//   POST /api/rc/passkey/enrol-code        laptop only (loopback, no serve identity: localClient).
//        Writes a 6-digit, 5-minute, single-use code to <state>/deck-remote-enrol-code (0600) and
//        answers with the path only: the code never crosses the network and never reaches a log.
//   POST /api/rc/passkey/register/begin    authAct + {code} → creation options (platform, UV required)
//   POST /api/rc/passkey/register/finish   authAct + {code, credential} → stored credential
//        The code is checked at begin and at finish; 3 wrong codes void it (file removed).
//   GET  /api/rc/passkey/list              authAct: this device token's credentials (id8, created)
//   POST /api/rc/tower/run/challenge       authAct + run:mut + {ask, item, sha256[, cred]} → request
//        options; the 60 s single-use challenge is bound to (ask, item, sha, class, signature
//        fingerprint, device-token id, credential id)
//   verifyRunAssertion                     W3's run handler calls it: spends the challenge, checks
//        origin, rpIdHash, UV, signature, sign count (clone semantics), that the credential belongs
//        to the presenting device token, and that the line's signature did not change since.
//
// Credentials live in <state>/deck-remote-passkeys.json (0600), one webauthn.User per device
// token; revoking the token deletes its credentials. Codes, challenges and assertions live in
// memory: a restart voids them. Every issue, refusal and success is one action-log line carrying
// credential and challenge ids (8 chars), never the code, a challenge or an assertion.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	enrolCodeFile       = "deck-remote-enrol-code"
	passkeyStoreFile    = "deck-remote-passkeys.json"
	enrolCodeTTL        = 5 * time.Minute
	enrolCodeMaxFails   = 3
	passkeyRegTTL       = 2 * time.Minute
	passkeyChallengeTTL = 60 * time.Second
	passkeyRateMax      = 10 // per device token per minute, per bucket (enrol, challenge, finish)
	passkeyBodyMax      = 64 << 10
)

// passkeyRecord is one stored credential. Cred is the library's record (public key, sign count,
// flags: BackupEligible must stay stable across logins, so the whole record is kept).
type passkeyRecord struct {
	Device  string              `json:"device"` // device-token id
	Created int64               `json:"created"`
	Cred    webauthn.Credential `json:"credential"`
}

// passkeyUser adapts a device token to webauthn.User: the user handle is the token id, so the
// library's allow-list and ownership checks enforce "the credential belongs to this token".
type passkeyUser struct {
	dt    deviceToken
	creds []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte                         { return []byte(u.dt.ID) }
func (u passkeyUser) WebAuthnName() string                       { return u.dt.Name }
func (u passkeyUser) WebAuthnDisplayName() string                { return u.dt.Name + " (deck-remote)" }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

type enrolCode struct {
	hash   string // hashToken(code)
	exp    time.Time
	fails  int
	device string // bound to the first device token that presents it correctly
}

type pendingReg struct {
	session webauthn.SessionData
	exp     time.Time
}

// runChallenge is what one assertion may authorise: exactly this line, as signed when issued,
// for this device token and credential.
type runChallenge struct {
	id      string // challenge id8 (sha256 of the challenge), for logs and the journal's auth field
	ask     string
	item    int
	sha     string
	class   string
	fp      string // runSigFingerprint at issue
	cwd     string
	timeout int
	device  string // device-token id
	cred    []byte
	session webauthn.SessionData
	exp     time.Time
}

// passkeyReceipt is what a verified assertion hands the run endpoint (W3): --expect-cwd/-timeout
// come from the challenge, --auth from cred and challenge.
type passkeyReceipt struct {
	Method    string `json:"method"`
	Cred      string `json:"cred"`
	Challenge string `json:"challenge"`
	Cwd       string `json:"-"`
	Timeout   int    `json:"-"`
}

type passkeyHub struct {
	dir string
	now func() time.Time

	mu    sync.Mutex
	recs  []passkeyRecord
	code  *enrolCode
	regs  map[string]pendingReg   // device-token id → registration in flight
	chals map[string]runChallenge // challenge (base64url) → binding
	hits  map[string][]time.Time  // rate-limit bucket → recent hits
}

func newPasskeyHub(dir string) *passkeyHub {
	h := &passkeyHub{dir: dir, now: time.Now, regs: map[string]pendingReg{}, chals: map[string]runChallenge{}, hits: map[string][]time.Time{}}
	if b, err := os.ReadFile(filepath.Join(dir, passkeyStoreFile)); err == nil {
		_ = json.Unmarshal(b, &h.recs)
	}
	// A code file left by an earlier process is dead (codes live in memory); do not leave it lying around.
	_ = os.Remove(filepath.Join(dir, enrolCodeFile))
	return h
}

func (h *passkeyHub) saveLocked() error {
	b, err := json.MarshalIndent(h.recs, "", "  ")
	if err != nil {
		return err
	}
	return writeFile0600(filepath.Join(h.dir, passkeyStoreFile), b)
}

// writeFile0600 writes via a fresh 0600 temp file in the same dir and renames it over path, so the
// target is never readable by others, not even for a moment, whatever mode an old file had.
func writeFile0600(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// credsLocked is the device token's credentials. Caller holds mu.
func (h *passkeyHub) credsLocked(device string) []webauthn.Credential {
	var out []webauthn.Credential
	for _, r := range h.recs {
		if r.Device == device {
			out = append(out, r.Cred)
		}
	}
	return out
}

// prune drops credentials whose device token no longer exists (a revoke by an older binary, or
// a hand-edited token file). Called once at startup.
func (h *passkeyHub) prune(ts *tokenStore) {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.recs[:0]
	for _, r := range h.recs {
		if _, ok := ts.byID(r.Device); ok {
			kept = append(kept, r)
		}
	}
	if len(kept) != len(h.recs) {
		h.recs = kept
		_ = h.saveLocked()
	}
}

// revokeDevice is the revoke cascade: the token's credentials, its registration in flight and its
// open challenges go with it. Returns the number of credentials removed.
func (h *passkeyHub) revokeDevice(device string) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := make([]passkeyRecord, 0, len(h.recs))
	for _, r := range h.recs {
		if r.Device != device {
			kept = append(kept, r)
		}
	}
	n := len(h.recs) - len(kept)
	delete(h.regs, device)
	for k, c := range h.chals {
		if c.device == device {
			delete(h.chals, k)
		}
	}
	if h.code != nil && h.code.device == device {
		h.voidCodeLocked()
	}
	if n == 0 {
		return 0, nil
	}
	h.recs = kept
	return n, h.saveLocked()
}

// allow is a sliding one-minute window per bucket; a refused call is not counted.
func (h *passkeyHub) allow(bucket string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	recent := h.hits[bucket][:0]
	for _, t := range h.hits[bucket] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= passkeyRateMax {
		h.hits[bucket] = recent
		return false
	}
	h.hits[bucket] = append(recent, now)
	return true
}

// --- enrol code (F9a) ---------------------------------------------------------

func (h *passkeyHub) codePath() string { return filepath.Join(h.dir, enrolCodeFile) }

// voidCodeLocked forgets the code and removes its file. Caller holds mu.
func (h *passkeyHub) voidCodeLocked() {
	h.code = nil
	_ = os.Remove(h.codePath())
}

// issueCode writes a fresh code to the 0600 file, replacing any live one. Returns whether it
// replaced one. The code itself is never returned.
func (h *passkeyHub) issueCode() (replaced bool, err error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return false, err
	}
	code := fmt.Sprintf("%06d", n.Int64())
	h.mu.Lock()
	defer h.mu.Unlock()
	replaced = h.code != nil && h.now().Before(h.code.exp)
	h.code = nil
	if err := writeFile0600(h.codePath(), []byte(code+"\n")); err != nil {
		_ = os.Remove(h.codePath())
		return replaced, err
	}
	c := &enrolCode{hash: hashToken(code), exp: h.now().Add(enrolCodeTTL)}
	h.code = c
	time.AfterFunc(enrolCodeTTL, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.code == c { // still the same, unused code: expire its file
			h.voidCodeLocked()
		}
	})
	return replaced, nil
}

// checkCode tests a presented code for a device token. A wrong code counts toward the lockout;
// the third failure voids the code. consume spends a correct code. Returns "" when accepted.
func (h *passkeyHub) checkCode(presented, device string, consume bool) string {
	presented = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, presented)
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.code
	if c == nil {
		return "no enrol code is live: ask the laptop for one (POST /api/rc/passkey/enrol-code)"
	}
	if !h.now().Before(c.exp) {
		h.voidCodeLocked()
		return "enrol code expired"
	}
	ok := len(presented) == 6 && subtle.ConstantTimeCompare([]byte(hashToken(presented)), []byte(c.hash)) == 1
	if ok && c.device != "" && c.device != device {
		ok = false // a correct code from a second device: someone else saw it
	}
	if !ok {
		c.fails++
		if c.fails >= enrolCodeMaxFails {
			h.voidCodeLocked()
			return "wrong enrol code; the code is now void (3 failures)"
		}
		return "wrong enrol code"
	}
	c.device = device
	if consume {
		h.voidCodeLocked()
	}
	return ""
}

// --- relying party ---------------------------------------------------------------

// webAuthn builds the relying party from --public-url: rpId is its hostname and its origin is the
// only one accepted, so the LAN fallback (another origin) can never complete a ceremony.
func (s *server) webAuthn() (*webauthn.WebAuthn, string) {
	raw := strings.TrimRight(strings.TrimSpace(s.cfg.publicURL), "/")
	if raw == "" {
		return nil, "passkeys disabled: --public-url (DECK_REMOTE_PUBLIC_URL) not set"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") {
		return nil, "passkeys disabled: --public-url must be an https origin"
	}
	origin := u.Scheme + "://" + u.Host
	w, err := webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: "deck-remote",
		RPOrigins:     []string{origin},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyChallengeTTL, TimeoutUVD: passkeyChallengeTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: passkeyRegTTL, TimeoutUVD: passkeyRegTTL},
		},
	})
	if err != nil {
		return nil, "passkeys disabled: " + err.Error()
	}
	return w, ""
}

func credID8(id []byte) string {
	s := base64.RawURLEncoding.EncodeToString(id)
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func challengeID8(challenge string) string {
	sum := sha256.Sum256([]byte(challenge))
	return hex.EncodeToString(sum[:4])
}

// webauthnWhy is a short, log-safe reason from a library error (no challenge or key bytes).
func webauthnWhy(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		if pe.Details != "" {
			return oneLineGo(pe.Details, 160)
		}
		return oneLineGo(pe.Type, 160)
	}
	return oneLineGo(err.Error(), 160)
}

// --- handlers ---------------------------------------------------------------------

// passkeyActor is the device token behind an authAct-gated request (authAct already proved it).
func (s *server) passkeyActor(w http.ResponseWriter, r *http.Request) (deviceToken, bool) {
	dt, ok := s.tokens.find(bearerHeaderOnly(r))
	if !ok {
		httpError(w, http.StatusForbidden, "device token required")
	}
	return dt, ok
}

// POST /api/rc/passkey/enrol-code. Behind s.auth; additionally only for a client on this machine
// that did not come through tailscale serve (F9a): serve relays are loopback peers too, so authAct
// would let a stolen act bearer on the tailnet mint a code for itself.
func (s *server) handleEnrolCode(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "passkey/enrol-code", "allowed": false, "login": tsLogin(r)}
	if why := localClient(r); why != "" {
		entry["reason"] = why
		s.towerLog(entry)
		httpError(w, http.StatusForbidden, "the enrol code is issued on the laptop only: "+why)
		return
	}
	if _, why := s.webAuthn(); why != "" {
		entry["reason"] = why
		s.towerLog(entry)
		httpError(w, http.StatusConflict, why)
		return
	}
	replaced, err := s.passkeys.issueCode()
	if err != nil {
		entry["reason"] = "write failed"
		s.towerLog(entry)
		httpError(w, http.StatusInternalServerError, "could not write the enrol code file")
		return
	}
	entry["allowed"], entry["replaced"] = true, replaced
	s.towerLog(entry)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": s.passkeys.codePath(), "expiresIn": int(enrolCodeTTL / time.Second), "replaced": replaced})
}

// POST /api/rc/passkey/register/begin {code}. Behind authAct.
func (s *server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.passkeyActor(w, r)
	if !ok {
		return
	}
	entry := map[string]any{"action": "passkey/register/begin", "allowed": false, "device": dt.Name, "login": tsLogin(r)}
	refuse := func(code int, reason string) {
		entry["reason"] = reason
		s.towerLog(entry)
		httpError(w, code, reason)
	}
	wa, why := s.webAuthn()
	if why != "" {
		refuse(http.StatusConflict, why)
		return
	}
	if !s.passkeys.allow("enrol:" + dt.ID) {
		refuse(http.StatusTooManyRequests, "too many passkey requests; wait a minute")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		refuse(http.StatusBadRequest, "bad json")
		return
	}
	if why := s.passkeys.checkCode(body.Code, dt.ID, false); why != "" {
		refuse(http.StatusForbidden, why)
		return
	}
	h := s.passkeys
	h.mu.Lock()
	user := passkeyUser{dt: dt, creds: h.credsLocked(dt.ID)}
	h.mu.Unlock()
	var exclude []protocol.CredentialDescriptor
	for _, c := range user.creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, session, err := wa.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.Platform,
			ResidentKey:             protocol.ResidentKeyRequirementPreferred,
			UserVerification:        protocol.VerificationRequired,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithExclusions(exclude),
	)
	if err != nil {
		refuse(http.StatusInternalServerError, "registration options: "+webauthnWhy(err))
		return
	}
	h.mu.Lock()
	h.regs[dt.ID] = pendingReg{session: *session, exp: h.now().Add(passkeyRegTTL)}
	h.mu.Unlock()
	entry["allowed"] = true
	s.towerLog(entry)
	writeJSON(w, http.StatusOK, creation)
}

// POST /api/rc/passkey/register/finish {code, credential}. Behind authAct.
func (s *server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.passkeyActor(w, r)
	if !ok {
		return
	}
	entry := map[string]any{"action": "passkey/register/finish", "allowed": false, "device": dt.Name, "login": tsLogin(r)}
	refuse := func(code int, reason string) {
		entry["reason"] = reason
		s.towerLog(entry)
		httpError(w, code, reason)
	}
	wa, why := s.webAuthn()
	if why != "" {
		refuse(http.StatusConflict, why)
		return
	}
	if !s.passkeys.allow("finish:" + dt.ID) {
		refuse(http.StatusTooManyRequests, "too many passkey requests; wait a minute")
		return
	}
	var body struct {
		Code       string          `json:"code"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, passkeyBodyMax)).Decode(&body); err != nil {
		refuse(http.StatusBadRequest, "bad json")
		return
	}
	h := s.passkeys
	// The registration in flight is single use whatever happens next.
	h.mu.Lock()
	reg, had := h.regs[dt.ID]
	delete(h.regs, dt.ID)
	h.mu.Unlock()
	if why := h.checkCode(body.Code, dt.ID, false); why != "" {
		refuse(http.StatusForbidden, why)
		return
	}
	if !had {
		refuse(http.StatusConflict, "no registration in flight for this device: start again")
		return
	}
	if !h.now().Before(reg.exp) {
		refuse(http.StatusForbidden, "registration expired: start again")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body.Credential)
	if err != nil {
		refuse(http.StatusBadRequest, "bad credential: "+webauthnWhy(err))
		return
	}
	h.mu.Lock()
	user := passkeyUser{dt: dt, creds: h.credsLocked(dt.ID)}
	h.mu.Unlock()
	cred, err := wa.CreateCredential(user, reg.session, parsed)
	if err != nil {
		refuse(http.StatusForbidden, "registration refused: "+webauthnWhy(err))
		return
	}
	if !cred.Flags.UserVerified { // the library checks it under UV required; keep the rule visible here
		refuse(http.StatusForbidden, "registration refused: user verification (Face ID) did not happen")
		return
	}
	entry["cred"] = credID8(cred.ID)
	// Spend the code only now: a ceremony that failed above may be retried with the same code.
	if why := h.checkCode(body.Code, dt.ID, true); why != "" {
		refuse(http.StatusForbidden, why)
		return
	}
	h.mu.Lock()
	for _, rec := range h.recs {
		if bytes.Equal(rec.Cred.ID, cred.ID) {
			h.mu.Unlock()
			refuse(http.StatusConflict, "credential already enrolled")
			return
		}
	}
	h.recs = append(h.recs, passkeyRecord{Device: dt.ID, Created: h.now().Unix(), Cred: *cred})
	err = h.saveLocked()
	if err != nil {
		h.recs = h.recs[:len(h.recs)-1]
	}
	h.mu.Unlock()
	if err != nil {
		refuse(http.StatusInternalServerError, "could not store the credential")
		return
	}
	entry["allowed"] = true
	s.towerLog(entry)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cred": credID8(cred.ID)})
}

// GET /api/rc/passkey/list: the calling device token's credentials. Behind authAct.
func (s *server) handlePasskeyList(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.passkeyActor(w, r)
	if !ok {
		return
	}
	_, why := s.webAuthn()
	h := s.passkeys
	h.mu.Lock()
	out := []map[string]any{}
	for _, rec := range h.recs {
		if rec.Device == dt.ID {
			out = append(out, map[string]any{"cred": credID8(rec.Cred.ID), "id": base64.RawURLEncoding.EncodeToString(rec.Cred.ID), "created": rec.Created})
		}
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"passkeys": out, "enabled": why == "", "why": why})
}

// runSigFingerprint stands in for the signature MAC, which `run_bang.py verify` does not emit. The
// v2 MAC is a function of (ask, index, sha, exp, cwd, timeout) under kid, so a hash over those
// plus class and the signature time moves exactly when a re-sign (or a param change) would move
// the MAC.
func runSigFingerprint(ask string, it *runItem, sigTS any) string {
	b, _ := json.Marshal([]any{ask, it.Index, it.SHA256, it.Class, it.Sig, it.Cwd, it.Timeout, it.Exp, it.Kid, sigTS})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// passkeyClasses are the classes whose phone run needs an assertion. secret stays phase 3,
// destructive and interactive never run from the phone.
var passkeyClasses = map[string]string{"mut": "run:mut"}

// runLineOK is the line-side gate shared by the challenge and the assertion: the item exists with
// this sha, its class needs an assertion, its signature verifies and the runner calls it phone-ok.
func runLineOK(v *runVerify, item int, sha string) (*runItem, int, string) {
	it := v.item(item)
	if it == nil {
		return nil, http.StatusNotFound, "no such `! ` item in the open ask"
	}
	if it.SHA256 != sha {
		return it, http.StatusConflict, "the line changed since the sheet was drawn (sha mismatch)"
	}
	if _, ok := passkeyClasses[it.Class]; !ok {
		return it, http.StatusForbidden, "no passkey run for class " + it.Class
	}
	if it.Sig != "ok" {
		return it, http.StatusForbidden, "signature: " + it.Sig
	}
	if it.PhoneOK == nil || !*it.PhoneOK {
		return it, http.StatusForbidden, "laptop only"
	}
	return it, 0, ""
}

type runChallengeReq struct {
	Ask    string `json:"ask"`
	Item   *int   `json:"item"`
	SHA256 string `json:"sha256"`
	Cred   string `json:"cred"` // base64url credential id; optional when the token has exactly one
}

// POST /api/rc/tower/run/challenge. Behind authAct.
func (s *server) handleRunChallenge(w http.ResponseWriter, r *http.Request) {
	dt, ok := s.passkeyActor(w, r)
	if !ok {
		return
	}
	entry := map[string]any{"action": "run/challenge", "allowed": false, "device": dt.Name, "login": tsLogin(r)}
	refuse := func(code int, reason string) {
		entry["reason"], entry["code"] = reason, code
		s.towerLog(entry)
		httpError(w, code, reason)
	}
	var req runChallengeReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "bad json")
		return
	}
	req.Ask = strings.TrimSpace(req.Ask)
	entry["ask"] = req.Ask
	if req.Item != nil {
		entry["item"] = *req.Item
	}
	if len(req.SHA256) >= 8 {
		entry["sha8"] = req.SHA256[:8]
	}
	if req.Ask == "" || len(req.Ask) > 200 || strings.HasPrefix(req.Ask, "-") || req.Item == nil || *req.Item < 0 || !runSHARe.MatchString(req.SHA256) {
		refuse(http.StatusBadRequest, "need ask, item >= 0 and sha256 (64 lowercase hex)")
		return
	}
	wa, why := s.webAuthn()
	if why != "" {
		refuse(http.StatusConflict, why)
		return
	}
	if !dt.hasCap("run:mut") {
		refuse(http.StatusForbidden, "device token "+dt.Name+" lacks the run:mut capability")
		return
	}
	if !s.passkeys.allow("challenge:" + dt.ID) {
		refuse(http.StatusTooManyRequests, "too many passkey requests; wait a minute")
		return
	}
	v, err := s.runVerify(req.Ask)
	if err != nil {
		refuse(http.StatusBadGateway, err.Error())
		return
	}
	it, code, why := runLineOK(v, *req.Item, req.SHA256)
	if it != nil {
		entry["class"] = it.Class
	}
	if why != "" {
		refuse(code, why)
		return
	}
	h := s.passkeys
	h.mu.Lock()
	creds := h.credsLocked(dt.ID)
	h.mu.Unlock()
	var pick *webauthn.Credential
	for i := range creds {
		if req.Cred == "" && len(creds) == 1 || req.Cred != "" && base64.RawURLEncoding.EncodeToString(creds[i].ID) == req.Cred {
			pick = &creds[i]
		}
	}
	if pick == nil {
		if len(creds) == 0 {
			refuse(http.StatusForbidden, "no passkey enrolled for this device: enrol in Settings with a code from the laptop")
		} else {
			refuse(http.StatusBadRequest, "name the passkey (cred) to use")
		}
		return
	}
	entry["cred"] = credID8(pick.ID)
	assertion, session, err := wa.BeginLogin(passkeyUser{dt: dt, creds: creds},
		webauthn.WithAllowedCredentials([]protocol.CredentialDescriptor{pick.Descriptor()}),
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		refuse(http.StatusInternalServerError, "assertion options: "+webauthnWhy(err))
		return
	}
	ch := runChallenge{
		id: challengeID8(session.Challenge), ask: req.Ask, item: *req.Item, sha: req.SHA256, class: it.Class,
		fp: runSigFingerprint(req.Ask, it, v.SigTS), cwd: it.Cwd, timeout: it.Timeout,
		device: dt.ID, cred: pick.ID, session: *session, exp: h.now().Add(passkeyChallengeTTL),
	}
	h.mu.Lock()
	now := h.now()
	for k, c := range h.chals {
		if !now.Before(c.exp) {
			delete(h.chals, k)
		}
	}
	h.chals[session.Challenge] = ch
	h.mu.Unlock()
	entry["allowed"], entry["challenge"] = true, ch.id
	s.towerLog(entry)
	writeJSON(w, http.StatusOK, map[string]any{
		"options": assertion, "challenge": ch.id, "expiresIn": int(passkeyChallengeTTL / time.Second),
		"cwd": it.Cwd, "timeout": it.Timeout, "exp": it.Exp, "kid": it.Kid,
	})
}

// verifyRunAssertion checks one assertion for (ask, item, sha) presented by dt and spends its
// challenge whatever the outcome. On success the credential's sign count is stored and the
// receipt names the credential and challenge for the run's journal record. Every outcome is one
// action-log line. Returns (receipt, HTTP status, reason); reason "" means verified.
// W3's run endpoint calls this for a mut line before it execs the runner.
func (s *server) verifyRunAssertion(r *http.Request, dt deviceToken, ask string, item int, sha string, assertion []byte) (passkeyReceipt, int, string) {
	entry := map[string]any{"action": "run/assert", "allowed": false, "device": dt.Name, "login": tsLogin(r), "ask": ask, "item": item}
	if len(sha) >= 8 {
		entry["sha8"] = sha[:8]
	}
	refuse := func(code int, reason string) (passkeyReceipt, int, string) {
		entry["reason"], entry["code"] = reason, code
		s.towerLog(entry)
		return passkeyReceipt{}, code, reason
	}
	h := s.passkeys
	if !h.allow("finish:" + dt.ID) {
		return refuse(http.StatusTooManyRequests, "too many passkey requests; wait a minute")
	}
	wa, why := s.webAuthn()
	if why != "" {
		return refuse(http.StatusConflict, why)
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(assertion)
	if err != nil {
		return refuse(http.StatusBadRequest, "bad assertion: "+webauthnWhy(err))
	}
	entry["cred"] = credID8(parsed.RawID)
	h.mu.Lock()
	key := parsed.Response.CollectedClientData.Challenge
	ch, found := h.chals[key]
	delete(h.chals, key) // single use: a presented challenge is spent whatever the outcome
	now := h.now()
	h.mu.Unlock()
	if !found {
		return refuse(http.StatusForbidden, "unknown or already used challenge")
	}
	entry["challenge"] = ch.id
	if !now.Before(ch.exp) {
		return refuse(http.StatusForbidden, "challenge expired")
	}
	if ch.device != dt.ID {
		return refuse(http.StatusForbidden, "the challenge was issued to another device token")
	}
	if ch.ask != ask || ch.item != item || ch.sha != sha {
		return refuse(http.StatusForbidden, "the challenge is for another line")
	}
	if !bytes.Equal(parsed.RawID, ch.cred) {
		return refuse(http.StatusForbidden, "the assertion's credential is not the one the challenge named")
	}
	if cur, ok := s.tokens.byID(dt.ID); !ok || !cur.hasCap("run:mut") {
		return refuse(http.StatusForbidden, "device token revoked or lacks run:mut")
	}
	h.mu.Lock()
	creds := h.credsLocked(dt.ID)
	h.mu.Unlock()
	cred, err := wa.ValidateLogin(passkeyUser{dt: dt, creds: creds}, ch.session, parsed)
	if err != nil {
		return refuse(http.StatusForbidden, "assertion refused: "+webauthnWhy(err))
	}
	if cred.Authenticator.CloneWarning {
		// Clone semantics (review §2.1): 0 → 0 is fine (iCloud Keychain passkeys never count);
		// otherwise the counter must grow.
		return refuse(http.StatusForbidden, "sign count did not advance: possible cloned authenticator")
	}
	if !parsed.Response.AuthenticatorData.Flags.HasUserVerified() {
		return refuse(http.StatusForbidden, "user verification (Face ID) did not happen")
	}
	v, err := s.runVerify(ask)
	if err != nil {
		return refuse(http.StatusBadGateway, err.Error())
	}
	it, code, why := runLineOK(v, item, sha)
	if why != "" {
		return refuse(code, why)
	}
	if it.Class != ch.class || runSigFingerprint(ask, it, v.SigTS) != ch.fp {
		return refuse(http.StatusConflict, "re-signed since the challenge: try again")
	}
	h.mu.Lock()
	stored := false
	for i := range h.recs {
		if h.recs[i].Device == dt.ID && bytes.Equal(h.recs[i].Cred.ID, cred.ID) {
			h.recs[i].Cred.Authenticator.SignCount = cred.Authenticator.SignCount
			h.recs[i].Cred.Flags = cred.Flags
			stored = h.saveLocked() == nil
		}
	}
	h.mu.Unlock()
	if !stored {
		return refuse(http.StatusInternalServerError, "could not store the sign count")
	}
	entry["allowed"] = true
	delete(entry, "code")
	s.towerLog(entry)
	return passkeyReceipt{Method: "passkey", Cred: credID8(cred.ID), Challenge: ch.id, Cwd: ch.cwd, Timeout: ch.timeout}, http.StatusOK, ""
}
