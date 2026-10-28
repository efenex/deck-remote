package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// run-bang phase 2 #5 (run-bang-phase2-review.md §5 W2): passkeys against a software
// authenticator. go-webauthn ships no virtual authenticator, so softAuth below builds the
// attestation and assertion bytes itself (ES256, "none" attestation, as an iPhone sends).

const (
	pkOrigin   = "https://deck.example.ts.net"
	pkRPID     = "deck.example.ts.net"
	pkOperator = "op@example.com"
)

var (
	shaPhone  = strings.Repeat("b", 64) // item 1: mut, phone-ok
	shaLaptop = strings.Repeat("e", 64) // item 2: mut, laptop only
)

func TestPasskeyStartupPreservesCredentialsOnTokenLoadFailure(t *testing.T) {
	for _, failure := range []string{"missing", "malformed", "wrong-field-type", "unreadable"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			h := newPasskeyHub(dir)
			h.recs = []passkeyRecord{{Device: "fixture-device", Created: 123}}
			if err := h.saveLocked(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "deck-remote-tokens.json")
			switch failure {
			case "malformed":
				if err := os.WriteFile(path, []byte("["), 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong-field-type":
				if err := os.WriteFile(path, []byte(`[{"id":"fixture-device","hash":"fixture","caps":"act"}]`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				// Reading a directory fails even when tests run with elevated privileges.
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			h.startup(newTokenStore(dir))
			if len(h.recs) != 1 || len(newPasskeyHub(dir).recs) != 1 {
				t.Fatal("startup erased credentials after token load failure")
			}
			if failure == "unreadable" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte(`[{"id":"fixture-device","hash":"fixture"}]`), 0o600); err != nil {
				t.Fatal(err)
			}
			h.startup(newTokenStore(dir))
			if len(h.recs) != 1 {
				t.Fatal("credential lost after token file repair")
			}
			if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
				t.Fatal(err)
			}
			h.startup(newTokenStore(dir))
			if len(h.recs) != 0 || len(newPasskeyHub(dir).recs) != 0 {
				t.Fatal("valid empty token store did not prune credentials")
			}
		})
	}
}

// softAuth is a platform authenticator in software. Per call the test controls what a real one
// would not let it: the origin in clientData, the UV bit and the sign counter.
type softAuth struct {
	key    *ecdsa.PrivateKey
	id     []byte
	rpID   string
	origin string
	count  uint32
	uv     bool
}

func newSoftAuth(t *testing.T) *softAuth {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &softAuth{key: k, id: id, rpID: pkRPID, origin: pkOrigin, uv: true}
}

const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (a *softAuth) authData(attested []byte) []byte {
	rp := sha256.Sum256([]byte(a.rpID))
	flags := byte(flagUP | flagBE | flagBS) // iCloud Keychain passkeys are backup eligible and backed up
	if a.uv {
		flags |= flagUV
	}
	if attested != nil {
		flags |= flagAT
	}
	out := append([]byte{}, rp[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, a.count)
	return append(out, attested...)
}

func (a *softAuth) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	return b
}

// create answers navigator.credentials.create for the server's creation options.
func (a *softAuth) create(t *testing.T, creation []byte) json.RawMessage {
	t.Helper()
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(creation, &opts); err != nil || opts.PublicKey.Challenge == "" {
		t.Fatalf("creation options: %s", creation)
	}
	cose, err := webauthncbor.Marshal(map[int64]any{1: int64(2), 3: int64(-7), -1: int64(1),
		-2: a.key.X.FillBytes(make([]byte, 32)), -3: a.key.Y.FillBytes(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	attested := make([]byte, 16) // aaguid: zero, as with "none" attestation
	attested = binary.BigEndian.AppendUint16(attested, uint16(len(a.id)))
	attested = append(append(attested, a.id...), cose...)
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(attested)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"id": b64u(a.id), "rawId": b64u(a.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response":               map[string]any{"clientDataJSON": b64u(a.clientData("webauthn.create", opts.PublicKey.Challenge)), "attestationObject": b64u(att), "transports": []string{"internal", "hybrid"}},
		"clientExtensionResults": map[string]any{},
	})
	return b
}

// get answers navigator.credentials.get for the challenge endpoint's options.
func (a *softAuth) get(t *testing.T, options json.RawMessage) json.RawMessage {
	t.Helper()
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &opts); err != nil || opts.PublicKey.Challenge == "" {
		t.Fatalf("assertion options: %s", options)
	}
	ad := a.authData(nil)
	cd := a.clientData("webauthn.get", opts.PublicKey.Challenge)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"id": b64u(a.id), "rawId": b64u(a.id), "type": "public-key", "authenticatorAttachment": "platform",
		"response":               map[string]any{"clientDataJSON": b64u(cd), "authenticatorData": b64u(ad), "signature": b64u(sig)},
		"clientExtensionResults": map[string]any{},
	})
	return b
}

// pkRunner stubs `run_bang.py verify`; exp moves when the test re-signs.
type pkRunner struct {
	mu         sync.Mutex
	exp        int64
	sigTS      string
	verifyN    int
	preW4      bool       // the runner's phone gate is still {ro}: mut lines are class:mut, not runnable
	runs       [][]string // argv of every `run`
	runRC      int
	runOut     string
	laptopItem bool // item 1's phone_ok (false = laptop only)
}

func (p *pkRunner) exec(ctx context.Context, argv []string) (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if argv[3] == "run" {
		p.runs = append(p.runs, argv)
		if p.runOut != "" {
			return p.runRC, p.runOut
		}
		return 0, `{"id":"ask1:1:bbbbbbbb","class":"mut","log":"state/runs/x.log","pid":42,"timeout":900}` + "\n"
	}
	if argv[3] != "verify" {
		return 1, `{"error":"unexpected"}`
	}
	p.verifyN++
	yes, no := !p.laptopItem, false
	mutRunnable, mutWhy := !p.preW4, any(nil)
	if p.preW4 {
		mutWhy = "class:mut"
	}
	items := []map[string]any{
		{"index": 0, "sha256": shaRo, "class": "ro", "phone_ok": true, "sig": "ok", "runnable": true, "cwd": "/Users/op", "timeout": 120, "exp": p.exp, "kid": "k1"},
		{"index": 1, "sha256": shaPhone, "class": "mut", "phone_ok": &yes, "sig": "ok", "runnable": mutRunnable, "why": mutWhy, "cwd": "/Users/op/src", "timeout": 900, "exp": p.exp, "kid": "k1"},
		{"index": 2, "sha256": shaLaptop, "class": "mut", "phone_ok": &no, "laptop_only": "file-driven", "sig": "ok", "runnable": false, "why": "laptop-only", "cwd": "/Users/op", "timeout": 900, "exp": p.exp, "kid": "k1"},
	}
	return 0, jsonStr(map[string]any{"ask": "ask1", "status": "open", "ask_ts": "2026-10-21T20:00:00Z", "sig_ts": p.sigTS, "items": items}) + "\n"
}

func (p *pkRunner) resign() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exp += 3600
	p.sigTS = "2026-10-21T23:00:00Z"
}

type pkEnv struct {
	t     *testing.T
	s     *server
	h     http.Handler
	run   *pkRunner
	clock time.Time
	state string
	shrd  string
}

func pkServer(t *testing.T) *pkEnv {
	t.Helper()
	s, _ := towerTestServer(t, pkOperator, towerFixture)
	s.cfg.publicURL = pkOrigin + "/"
	s.cfg.towerRoot = t.TempDir()
	run := &pkRunner{exp: 1_900_000_000, sigTS: "2026-10-21T20:00:00Z"}
	s.towerExec = run.exec
	e := &pkEnv{t: t, s: s, h: s.routes(), run: run, clock: time.Now(), state: os.Getenv("DECK_REMOTE_STATE_DIR"), shrd: "shared-secret"}
	s.passkeys.now = func() time.Time { return e.clock }
	return e
}

// req sends through the full route table as `tailscale serve` would relay it: loopback peer,
// the operator's identity, the bearer in the Authorization header.
func (e *pkEnv) req(method, path, tok string, body any, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	var rd *strings.Reader
	switch b := body.(type) {
	case nil:
		rd = strings.NewReader("")
	case string:
		rd = strings.NewReader(b)
	default:
		rd = strings.NewReader(jsonStr(b))
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set("Content-Type", "application/json")
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	r.Header.Set("Tailscale-User-Login", pkOperator)
	r.Header.Set("X-Forwarded-For", "100.64.0.7")
	r.RemoteAddr = "127.0.0.1:4242"
	for _, m := range mod {
		m(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// local strips what serve stamps: a client on the laptop itself (the TUI).
func local(r *http.Request) {
	r.Header.Del("Tailscale-User-Login")
	r.Header.Del("X-Forwarded-For")
}

func (e *pkEnv) codePath() string { return filepath.Join(e.state, enrolCodeFile) }

// issueCode asks for a code the way the TUI does and reads it from the file.
func (e *pkEnv) issueCode() string {
	e.t.Helper()
	w := e.req("POST", "/api/rc/passkey/enrol-code", e.shrd, nil, local)
	if w.Code != http.StatusOK {
		e.t.Fatalf("enrol-code: %d %s", w.Code, w.Body.String())
	}
	b, err := os.ReadFile(e.codePath())
	if err != nil {
		e.t.Fatal(err)
	}
	code := strings.TrimSpace(string(b))
	if len(code) != 6 {
		e.t.Fatalf("code file holds %q", code)
	}
	if strings.Contains(w.Body.String(), code) {
		e.t.Fatalf("enrol-code response carries the code: %s", w.Body.String())
	}
	return code
}

func (e *pkEnv) begin(tok, code string) *httptest.ResponseRecorder {
	return e.req("POST", "/api/rc/passkey/register/begin", tok, map[string]any{"code": code})
}

func (e *pkEnv) finish(tok, code string, cred json.RawMessage) *httptest.ResponseRecorder {
	return e.req("POST", "/api/rc/passkey/register/finish", tok, map[string]any{"code": code, "credential": cred})
}

// enrol runs the whole ceremony for tok with a fresh code.
func (e *pkEnv) enrol(a *softAuth, tok string) {
	e.t.Helper()
	code := e.issueCode()
	w := e.begin(tok, code)
	if w.Code != http.StatusOK {
		e.t.Fatalf("register/begin: %d %s", w.Code, w.Body.String())
	}
	w = e.finish(tok, code, a.create(e.t, w.Body.Bytes()))
	if w.Code != http.StatusOK {
		e.t.Fatalf("register/finish: %d %s", w.Code, w.Body.String())
	}
}

func (e *pkEnv) challenge(tok string, item int, sha string) *httptest.ResponseRecorder {
	return e.req("POST", "/api/rc/tower/run/challenge", tok, map[string]any{"ask": "ask1", "item": item, "sha256": sha})
}

func (e *pkEnv) options(w *httptest.ResponseRecorder) json.RawMessage {
	e.t.Helper()
	if w.Code != http.StatusOK {
		e.t.Fatalf("challenge: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Options json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	return out.Options
}

// assert is the call W3's run handler makes, on a request shaped like the phone's.
func (e *pkEnv) assert(tok string, item int, sha string, assertion json.RawMessage) (passkeyReceipt, int, string) {
	dt, ok := e.s.tokens.find(tok)
	if !ok {
		e.t.Fatal("assert: unknown token")
	}
	r := httptest.NewRequest("POST", "/api/rc/tower/run", nil)
	r.Header.Set("Tailscale-User-Login", pkOperator)
	return e.s.verifyRunAssertion(r, dt, "ask1", item, sha, assertion, nil)
}

func (e *pkEnv) lastLog(action string) map[string]any {
	e.t.Helper()
	var last map[string]any
	for _, m := range actionLog(e.t) {
		if m["action"] == action {
			last = m
		}
	}
	if last == nil {
		e.t.Fatalf("no action-log line for %s", action)
	}
	return last
}

func (e *pkEnv) expectRefusedLog(action, reason string) {
	e.t.Helper()
	m := e.lastLog(action)
	if m["allowed"] != false || !strings.Contains(m["reason"].(string), reason) {
		e.t.Errorf("%s log: %v, want refused with %q", action, m, reason)
	}
}

func expectCode(t *testing.T, name string, w *httptest.ResponseRecorder, code int, body string) {
	t.Helper()
	if w.Code != code || !strings.Contains(w.Body.String(), body) {
		t.Errorf("%s: got %d %s, want %d containing %q", name, w.Code, w.Body.String(), code, body)
	}
}

// F9a: the code is issued only to a client on the laptop, lands in a 0600 file, never in the
// response or the action log, and is single use.
func TestEnrolCodeLocalFileOnly(t *testing.T) {
	e := pkServer(t)
	act := mintTok(t, e.s, "phone", "act", "run:mut")

	// Every remote shape is refused and nothing is written.
	expectCode(t, "via serve (identity header)", e.req("POST", "/api/rc/passkey/enrol-code", act, nil), 403, "laptop only")
	expectCode(t, "via serve, shared token", e.req("POST", "/api/rc/passkey/enrol-code", e.shrd, nil), 403, "laptop only")
	expectCode(t, "relay without identity (X-Forwarded-For)", e.req("POST", "/api/rc/passkey/enrol-code", e.shrd, nil, func(r *http.Request) { r.Header.Del("Tailscale-User-Login") }), 403, "X-Forwarded-For")
	expectCode(t, "LAN peer", e.req("POST", "/api/rc/passkey/enrol-code", e.shrd, nil, local, func(r *http.Request) { r.RemoteAddr = "192.168.1.20:5000" }), 403, "loopback")
	expectCode(t, "no token", e.req("POST", "/api/rc/passkey/enrol-code", "", nil, local), 401, "")
	if _, err := os.Stat(e.codePath()); !os.IsNotExist(err) {
		t.Fatalf("a refused request wrote the code file: %v", err)
	}
	e.expectRefusedLog("passkey/enrol-code", "loopback")

	code := e.issueCode()
	st, err := os.Stat(e.codePath())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("code file mode: %v %v", st.Mode(), err)
	}
	logBytes, _ := os.ReadFile(filepath.Join(e.state, "deck-remote-tower-actions.log"))
	if strings.Contains(string(logBytes), code) {
		t.Fatal("the action log carries the enrol code")
	}

	// A new request replaces the code: the old one is dead.
	code2 := e.issueCode()
	if code2 != code {
		expectCode(t, "replaced code", e.begin(act, code), 403, "wrong enrol code")
	}
	// Single use: after a successful enrolment the file is gone and the code no longer works.
	a := newSoftAuth(t)
	w := e.begin(act, code2)
	expectCode(t, "begin", w, 200, "publicKey")
	expectCode(t, "finish", e.finish(act, code2, a.create(t, w.Body.Bytes())), 200, `"ok":true`)
	if _, err := os.Stat(e.codePath()); !os.IsNotExist(err) {
		t.Fatal("the code file outlived its use")
	}
	expectCode(t, "spent code", e.begin(act, code2), 403, "no enrol code is live")

	// Short-lived: past 5 minutes the code is void and its file removed.
	code3 := e.issueCode()
	e.clock = e.clock.Add(enrolCodeTTL + time.Second)
	expectCode(t, "expired code", e.begin(act, code3), 403, "expired")
	if _, err := os.Stat(e.codePath()); !os.IsNotExist(err) {
		t.Fatal("expired code file still on disk")
	}
	logBytes, _ = os.ReadFile(filepath.Join(e.state, "deck-remote-tower-actions.log"))
	for _, c := range []string{code, code2, code3} {
		if strings.Contains(string(logBytes), c) {
			t.Fatal("the action log carries an enrol code")
		}
	}
}

// 3 wrong codes void it: the 4th attempt fails even with the right code.
func TestEnrolCodeLockout(t *testing.T) {
	e := pkServer(t)
	act := mintTok(t, e.s, "phone", "act", "run:mut")
	code := e.issueCode()
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	expectCode(t, "wrong 1", e.begin(act, wrong), 403, "wrong enrol code")
	expectCode(t, "wrong 2", e.finish(act, wrong, json.RawMessage(`{}`)), 403, "wrong enrol code")
	expectCode(t, "wrong 3", e.begin(act, wrong), 403, "now void")
	expectCode(t, "4th, right code", e.begin(act, code), 403, "no enrol code is live")
	if _, err := os.Stat(e.codePath()); !os.IsNotExist(err) {
		t.Fatal("a void code's file is still on disk")
	}
	e.expectRefusedLog("passkey/register/begin", "no enrol code")
}

// Register needs the act chain (act cap, operator identity through serve, header-only bearer)
// on top of the code; the code alone does nothing.
func TestPasskeyRegisterNeedsActChain(t *testing.T) {
	e := pkServer(t)
	ro := mintTok(t, e.s, "ro-phone")
	act := mintTok(t, e.s, "phone", "act", "run:mut")
	code := e.issueCode()
	expectCode(t, "shared token", e.begin(e.shrd, code), 403, "shared token cannot")
	expectCode(t, "read-only token", e.begin(ro, code), 403, "lacks the act capability")
	expectCode(t, "no identity", e.req("POST", "/api/rc/passkey/register/begin", act, map[string]any{"code": code}, local), 403, "Tailscale-User-Login")
	expectCode(t, "?token= on a POST", e.req("POST", "/api/rc/passkey/register/begin?token="+act, "", map[string]any{"code": code}), 403, "device token required")
	expectCode(t, "?token= on the challenge", e.req("POST", "/api/rc/tower/run/challenge?token="+act, "", map[string]any{"ask": "ask1", "item": 1, "sha256": shaPhone}), 403, "device token required")
	// None of those counted against the code: authAct refused before the handler saw it.
	w := e.begin(act, code)
	expectCode(t, "act phone", w, 200, `"userVerification":"required"`)
	if !strings.Contains(w.Body.String(), `"authenticatorAttachment":"platform"`) || !strings.Contains(w.Body.String(), `"rp":{"name":"deck-remote","id":"`+pkRPID+`"}`) {
		t.Errorf("creation options: %s", w.Body.String())
	}
}

// Registration ceremony checks: UV must be set, the origin must be the tailnet URL, and a code
// bound to one phone cannot be finished by another.
func TestPasskeyRegisterCeremonyRefusals(t *testing.T) {
	e := pkServer(t)
	act := mintTok(t, e.s, "phone", "act", "run:mut")
	act2 := mintTok(t, e.s, "phone-2", "act", "run:mut")

	code := e.issueCode()
	a := newSoftAuth(t)
	a.uv = false
	w := e.begin(act, code)
	expectCode(t, "UV false", e.finish(act, code, a.create(t, w.Body.Bytes())), 403, "registration refused")
	e.expectRefusedLog("passkey/register/finish", "registration refused")

	b := newSoftAuth(t)
	b.origin = "https://blackbird.lan:8443"
	w = e.begin(act, code)
	expectCode(t, "LAN origin", e.finish(act, code, b.create(t, w.Body.Bytes())), 403, "registration refused")

	// The code is bound to the first phone that presented it correctly.
	expectCode(t, "second phone, same code", e.begin(act2, code), 403, "wrong enrol code")
	if n := len(e.s.passkeys.recs); n != 0 {
		t.Fatalf("%d credentials stored after refusals", n)
	}
	st, err := os.Stat(filepath.Join(e.state, passkeyStoreFile))
	if err == nil {
		t.Fatalf("store written without a credential: %v", st.Mode())
	}
}

// The assertion matrix: zero counters accepted, decreasing refused, wrong origin, UV false,
// spent, expired, another line, another token's credential, re-signed since the challenge:
// each refused and action-logged. A good one returns a receipt and stores the counter.
func TestPasskeyAssertionMatrix(t *testing.T) {
	e := pkServer(t)
	actA := mintTok(t, e.s, "phone-a", "act", "run:mut")
	actB := mintTok(t, e.s, "phone-b", "act", "run:mut")
	a, b := newSoftAuth(t), newSoftAuth(t)
	e.enrol(a, actA)
	e.enrol(b, actB)
	st, err := os.Stat(filepath.Join(e.state, passkeyStoreFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("store mode: %v %v", st, err)
	}

	ok := func(name string) passkeyReceipt {
		t.Helper()
		rc, code, why := e.assert(actA, 1, shaPhone, a.get(t, e.options(e.challenge(actA, 1, shaPhone))))
		if why != "" || code != 200 || rc.Method != "passkey" || rc.Cred != credID8(a.id) || len(rc.Challenge) != 8 {
			t.Fatalf("%s: %d %q %+v", name, code, why, rc)
		}
		return rc
	}
	refused := func(name string, gotCode int, why string, wantCode int, want string) {
		t.Helper()
		defer func() { e.clock = e.clock.Add(time.Minute) }() // a fresh rate-limit window per case
		if gotCode != wantCode || !strings.Contains(why, want) {
			t.Errorf("%s: got %d %q, want %d containing %q", name, gotCode, why, wantCode, want)
		}
		e.expectRefusedLog("run/assert", want)
	}

	// iCloud Keychain: the counter is always 0. 0 → 0 twice is fine.
	rc := ok("zero counter")
	if rc.Cwd != "/Users/op/src" || rc.Timeout != 900 {
		t.Errorf("receipt params: %+v", rc)
	}
	ok("zero counter again")
	m := e.lastLog("run/assert")
	if m["allowed"] != true || m["cred"] != credID8(a.id) || m["challenge"] != rc.Challenge && len(m["challenge"].(string)) != 8 {
		t.Errorf("success log: %v", m)
	}
	// A counting authenticator: up is fine, equal or down is a clone.
	a.count = 5
	ok("counter 5")
	a.count = 3
	_, code, why := e.assert(actA, 1, shaPhone, a.get(t, e.options(e.challenge(actA, 1, shaPhone))))
	refused("decreasing counter", code, why, 403, "sign count did not advance")
	a.count = 5
	_, code, why = e.assert(actA, 1, shaPhone, a.get(t, e.options(e.challenge(actA, 1, shaPhone))))
	refused("equal counter", code, why, 403, "sign count did not advance")
	a.count = 6
	ok("counter 6")

	// Wrong origin (the LAN fallback) and UV false.
	a.count++
	a.origin = "https://blackbird.lan:8443"
	_, code, why = e.assert(actA, 1, shaPhone, a.get(t, e.options(e.challenge(actA, 1, shaPhone))))
	refused("wrong origin", code, why, 403, "assertion refused")
	a.origin = pkOrigin
	a.uv = false
	_, code, why = e.assert(actA, 1, shaPhone, a.get(t, e.options(e.challenge(actA, 1, shaPhone))))
	refused("UV false", code, why, 403, "assertion refused")
	a.uv = true

	// Spent: the same assertion twice.
	a.count++
	asr := a.get(t, e.options(e.challenge(actA, 1, shaPhone)))
	if _, _, why := e.assert(actA, 1, shaPhone, asr); why != "" {
		t.Fatalf("first use: %s", why)
	}
	_, code, why = e.assert(actA, 1, shaPhone, asr)
	refused("spent", code, why, 403, "unknown or already used challenge")

	// Expired: 61 s after the challenge.
	a.count++
	asr = a.get(t, e.options(e.challenge(actA, 1, shaPhone)))
	e.clock = e.clock.Add(passkeyChallengeTTL + time.Second)
	_, code, why = e.assert(actA, 1, shaPhone, asr)
	refused("expired", code, why, 403, "challenge expired")
	e.clock = e.clock.Add(time.Minute) // fresh rate-limit window

	// Another (ask, item, sha): the challenge names one line.
	asr = a.get(t, e.options(e.challenge(actA, 1, shaPhone)))
	_, code, why = e.assert(actA, 0, shaRo, asr)
	refused("other item", code, why, 403, "another line")
	asr = a.get(t, e.options(e.challenge(actA, 1, shaPhone)))
	_, code, why = e.assert(actA, 1, shaLaptop, asr)
	refused("other sha", code, why, 403, "another line")

	// Bearer A with phone B's passkey: B's credential is not A's. Both ways the ceremony
	// composes: A's challenge answered by B, and B's challenge presented under A's bearer.
	asr = b.get(t, e.options(e.challenge(actA, 1, shaPhone)))
	_, code, why = e.assert(actA, 1, shaPhone, asr)
	refused("A's challenge, B's passkey", code, why, 403, "not the one the challenge named")
	asr = b.get(t, e.options(e.challenge(actB, 1, shaPhone)))
	_, code, why = e.assert(actA, 1, shaPhone, asr)
	refused("B's challenge under A's bearer", code, why, 403, "another device token")

	// Re-signed between challenge and assertion (F11 tick): the fingerprint moved.
	a.count++
	asr = a.get(t, e.options(e.challenge(actA, 1, shaPhone)))
	e.run.resign()
	_, code, why = e.assert(actA, 1, shaPhone, asr)
	refused("re-signed since the challenge", code, why, 409, "re-signed since the challenge")
	ok("after re-sign, a fresh challenge works")

	// The stored counter is the last accepted one, on disk.
	raw, _ := os.ReadFile(filepath.Join(e.state, passkeyStoreFile))
	var recs []passkeyRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if string(r.Cred.ID) == string(a.id) && r.Cred.Authenticator.SignCount != a.count {
			t.Errorf("stored sign count %d, want %d", r.Cred.Authenticator.SignCount, a.count)
		}
	}
	// No log line carries a challenge or an assertion.
	logBytes, _ := os.ReadFile(filepath.Join(e.state, "deck-remote-tower-actions.log"))
	if strings.Contains(string(logBytes), "clientDataJSON") || strings.Contains(string(logBytes), "signature") {
		t.Error("the action log carries assertion material")
	}
}

// The challenge endpoint's gates: run:mut cap, class needing a passkey, phone-ok, signature,
// sha, an enrolled passkey; and phase-2 runnability stays off (runCapFor is ro-only).
func TestRunChallengeGates(t *testing.T) {
	e := pkServer(t)
	actOnly := mintTok(t, e.s, "act-only", "act")
	ro := mintTok(t, e.s, "run-ro", "act", "run:ro")
	mut := mintTok(t, e.s, "phone", "act", "run:mut")
	expectCode(t, "act without run:mut", e.challenge(actOnly, 1, shaPhone), 403, "lacks the run:mut capability")
	expectCode(t, "run:ro", e.challenge(ro, 1, shaPhone), 403, "lacks the run:mut capability")
	expectCode(t, "no passkey yet", e.challenge(mut, 1, shaPhone), 403, "no passkey enrolled")
	e.enrol(newSoftAuth(t), mut)
	expectCode(t, "ro line", e.challenge(mut, 0, shaRo), 403, "no passkey run for class ro")
	expectCode(t, "laptop-only line", e.challenge(mut, 2, shaLaptop), 403, "laptop only")
	expectCode(t, "stale sha", e.challenge(mut, 1, shaLaptop), 409, "sha mismatch")
	expectCode(t, "no such item", e.challenge(mut, 7, shaPhone), 404, "no such")
	expectCode(t, "shared token", e.challenge(e.shrd, 1, shaPhone), 403, "shared token cannot")
	w := e.challenge(mut, 1, shaPhone)
	expectCode(t, "phone-ok mut line", w, 200, `"userVerification":"required"`)
	if !strings.Contains(w.Body.String(), `"cwd":"/Users/op/src"`) || !strings.Contains(w.Body.String(), `"expiresIn":60`) {
		t.Errorf("challenge body: %s", w.Body.String())
	}
	m := e.lastLog("run/challenge")
	if m["allowed"] != true || m["class"] != "mut" || len(m["challenge"].(string)) != 8 || m["cred"] == nil {
		t.Errorf("challenge log: %v", m)
	}
	if runCapFor("secret") != "" {
		t.Fatal("secret is phase 3")
	}
	// Before tower's gate flip (W4) the runner says class:mut: no challenge, so no Face ID.
	e.run.preW4 = true
	expectCode(t, "pre-W4 mut line", e.challenge(mut, 1, shaPhone), 403, "runner says not runnable: class:mut")
}

// 10 challenges a minute per token; the 11th is 429 and logged; the window slides.
func TestRunChallengeRateLimit(t *testing.T) {
	e := pkServer(t)
	mut := mintTok(t, e.s, "phone", "act", "run:mut")
	other := mintTok(t, e.s, "phone-2", "act", "run:mut")
	e.enrol(newSoftAuth(t), mut)
	e.enrol(newSoftAuth(t), other)
	for i := 0; i < passkeyRateMax; i++ {
		expectCode(t, "challenge", e.challenge(mut, 1, shaPhone), 200, "options")
	}
	expectCode(t, "11th", e.challenge(mut, 1, shaPhone), 429, "too many")
	e.expectRefusedLog("run/challenge", "too many")
	expectCode(t, "another token has its own bucket", e.challenge(other, 1, shaPhone), 200, "options")
	e.clock = e.clock.Add(time.Minute)
	expectCode(t, "a minute later", e.challenge(mut, 1, shaPhone), 200, "options")

	// The finish bucket (assertions and register/finish) has its own 10/min.
	e.clock = e.clock.Add(time.Minute)
	for i := 0; i < passkeyRateMax; i++ {
		if _, code, _ := e.assert(mut, 1, shaPhone, json.RawMessage(`{}`)); code != http.StatusBadRequest {
			t.Fatalf("garbage assertion %d: %d", i, code)
		}
	}
	if _, code, why := e.assert(mut, 1, shaPhone, json.RawMessage(`{}`)); code != http.StatusTooManyRequests {
		t.Errorf("11th assertion: %d %q", code, why)
	}
	e.expectRefusedLog("run/assert", "too many")
	expectCode(t, "register/finish shares the finish bucket", e.finish(mut, "123456", json.RawMessage(`{}`)), 429, "too many")
}

// Revoking the device token removes its passkeys (on disk) and voids its open challenges; the
// other phone's passkey stays. A restart prunes credentials of a token revoked elsewhere.
func TestDeviceRevokeSaveFailureIsRetryable(t *testing.T) {
	for _, by := range []string{"id", "name"} {
		t.Run(by, func(t *testing.T) {
			e := pkServer(t)
			first := mintTok(t, e.s, "first", "act")
			tok := mintTok(t, e.s, "target", "act", "run:mut")
			last := mintTok(t, e.s, "last", "act")
			e.enrol(newSoftAuth(t), tok)
			dt, _ := e.s.tokens.find(tok)
			key := dt.ID
			if by == "name" {
				key = dt.Name
			}
			path := e.s.tokens.path
			// Force rename to fail without depending on file modes or the temp writer.
			e.s.tokens.path = t.TempDir()
			w := e.req("POST", "/api/rc/devices/revoke", e.shrd, map[string]any{by: key}, local)
			expectCode(t, "failed revoke", w, 500, "revoke failed")
			e.s.tokens.path = path
			for _, raw := range []string{first, tok, last} {
				if !e.s.tokens.verify(raw) {
					t.Error("failed revoke changed in-memory tokens")
				}
				if !newTokenStore(e.state).verify(raw) {
					t.Error("failed revoke changed persisted tokens")
				}
			}
			if len(e.s.passkeys.recs) != 1 || len(newPasskeyHub(e.state).recs) != 1 {
				t.Error("failed token revoke removed passkeys")
			}
			expectCode(t, "retry", e.req("POST", "/api/rc/devices/revoke", e.shrd, map[string]any{by: key}, local), 200, `"ok":true`)
			if e.s.tokens.verify(tok) || newTokenStore(e.state).verify(tok) {
				t.Error("revoked token survived retry/restart")
			}
			if len(e.s.passkeys.recs) != 0 || len(newPasskeyHub(e.state).recs) != 0 {
				t.Error("successful retry kept passkeys")
			}
			if !e.s.tokens.verify(first) || !e.s.tokens.verify(last) {
				t.Error("retry removed another token")
			}
		})
	}
}

func TestPasskeyRevokeCascade(t *testing.T) {
	e := pkServer(t)
	mut := mintTok(t, e.s, "phone", "act", "run:mut")
	keep := mintTok(t, e.s, "phone-2", "act", "run:mut")
	a := newSoftAuth(t)
	e.enrol(a, mut)
	e.enrol(newSoftAuth(t), keep)
	asr := a.get(t, e.options(e.challenge(mut, 1, shaPhone)))
	dt, _ := e.s.tokens.find(mut)

	w := e.req("POST", "/api/rc/devices/revoke", e.shrd, map[string]any{"id": dt.ID}, local)
	expectCode(t, "revoke", w, 200, `"ok":true`)
	raw, _ := os.ReadFile(filepath.Join(e.state, passkeyStoreFile))
	var recs []passkeyRecord
	_ = json.Unmarshal(raw, &recs)
	if len(recs) != 1 || recs[0].Device == dt.ID {
		t.Fatalf("store after revoke: %d records %+v", len(recs), recs)
	}
	if m := e.lastLog("devices/revoke"); m["passkeys"] != float64(1) {
		t.Errorf("revoke log: %v", m)
	}
	r := httptest.NewRequest("POST", "/api/rc/tower/run", nil)
	if _, _, why := e.s.verifyRunAssertion(r, dt, "ask1", 1, shaPhone, asr, nil); !strings.Contains(why, "unknown or already used") {
		t.Errorf("revoked token's open challenge: %q", why)
	}

	// A token removed behind the server's back: the next start prunes its credentials.
	dt2, _ := e.s.tokens.find(keep)
	if _, err := e.s.tokens.revoke(dt2.ID); err != nil {
		t.Fatal(err)
	}
	s2 := testServer(t, e.s.cfg)
	s2.passkeys.startup(s2.tokens)
	if n := len(s2.passkeys.recs); n != 0 {
		t.Fatalf("restart kept %d orphaned credentials", n)
	}
}

// Without --public-url there is no relying party: every passkey endpoint refuses, clearly.
func TestPasskeysOffWithoutPublicURL(t *testing.T) {
	e := pkServer(t)
	e.s.cfg.publicURL = ""
	mut := mintTok(t, e.s, "phone", "act", "run:mut")
	expectCode(t, "enrol-code", e.req("POST", "/api/rc/passkey/enrol-code", e.shrd, nil, local), 409, "--public-url")
	expectCode(t, "begin", e.begin(mut, "123456"), 409, "--public-url")
	expectCode(t, "challenge", e.challenge(mut, 1, shaPhone), 409, "--public-url")
	e.s.cfg.publicURL = "http://deck.example.ts.net"
	expectCode(t, "plain http", e.req("POST", "/api/rc/passkey/enrol-code", e.shrd, nil, local), 409, "https origin")
	e.s.cfg.publicURL = "http://127.0.0.1:8765"
	expectCode(t, "plain http to an IP", e.req("POST", "/api/rc/passkey/enrol-code", e.shrd, nil, local), 409, "https origin")
	e.s.cfg.publicURL = "http://localhost:8765"
	if wa, why := e.s.webAuthn(); why != "" || wa.Config.RPID != "localhost" || wa.Config.RPOrigins[0] != "http://localhost:8765" {
		t.Fatalf("localhost (the e2e origin): %q", why)
	}
}
