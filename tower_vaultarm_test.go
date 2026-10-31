package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Run-bang V-2 (docs/design/run-bang-vault-sign.md §6, deck-remote): run:prog / prof:* caps, the
// arm gate before any challenge, the arm endpoint, the CSP's Vault origin and the callback page.

var (
	shaProg = strings.Repeat("c", 64)                                                          // item 3: a sequence step tagged prog
	armSig  = "vault:v1:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("A", 64))) // the shape Vault prints
)

const (
	armRev    = "50b7656d9999e475"
	armVault  = "https://vault.example.ts.net"
	armExpect = "run-bang-arm/v2\n"
)

// armRunner wraps pkRunner: verify answers for a sequence ask whose item 3 is a phone-ok prog
// step, with seq.arm.state = state; `seq arm` is recorded and answered with armRC/armOut.
type armRunner struct {
	*pkRunner
	mu      sync.Mutex
	state   string
	rev     string
	verifys [][]string
	arms    [][]string
	armRC   int
	armOut  string
}

func (a *armRunner) exec(ctx context.Context, argv []string) (int, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch argv[3] {
	case "seq":
		a.arms = append(a.arms, argv)
		if a.armOut != "" {
			return a.armRC, a.armOut
		}
		return 0, `{"id":"ask1:arm:50b7656d:20261023T000000Z.1","armed":true,"seq_rev":"` + a.rev + `","exp":1792720983,"device":"x","version":"1"}` + "\n"
	case "verify":
		a.verifys = append(a.verifys, argv)
		_, out := a.pkRunner.exec(ctx, argv)
		var v map[string]any
		_ = json.Unmarshal([]byte(out), &v)
		yes := true
		armed := a.state == "armed"
		why := any(nil)
		if !armed {
			why = "arm-" + a.state
		}
		v["items"] = append(v["items"].([]any), map[string]any{"index": 3, "sha256": shaProg, "class": "prog", "phone_ok": &yes, "sig": "ok",
			"runnable": armed, "why": why, "cwd": "/Users/op", "timeout": 120, "exp": a.pkRunner.exp, "kid": "k1", "line": "pwd", "seq_pos": 3})
		dev := argvFlag(argv, "--device")
		seq := map[string]any{"v": 1, "title": "t", "rev": a.rev, "sig": "ok", "pointer": 3, "state": "ready",
			"steps": []any{map[string]any{"index": 3, "title": "p", "tag": "prog"}}, "arm": map[string]any{"state": a.state, "id": nil}}
		if dev != "" {
			seq["arm_payload"] = armExpect + `"ask1"` + "\n" + a.rev + "\n4\n" + strings.Repeat("d", 64) + "\n1792720983\n\"" + dev + "\"\npower\n"
		}
		v["seq"] = seq
		return 0, jsonStr(v) + "\n"
	}
	return a.pkRunner.exec(ctx, argv)
}

func armServer(t *testing.T) (*pkEnv, *armRunner) {
	e := pkServer(t)
	ar := &armRunner{pkRunner: e.run, state: "missing", rev: armRev}
	e.s.towerExec = ar.exec
	e.s.cfg.vaultURL = armVault
	e.h = e.s.routes()
	return e, ar
}

func (e *pkEnv) postArm(tok string, body map[string]any) *httptest.ResponseRecorder {
	return e.req("POST", "/api/rc/tower/run/arm", tok, body)
}

func armBody() map[string]any {
	return map[string]any{"ask": "ask1", "seq_rev": armRev, "exp": 1792720983, "sig": armSig}
}

func tokID(t *testing.T, s *server, tok string) string {
	dt, ok := s.tokens.find(tok)
	if !ok {
		t.Fatal("unknown token")
	}
	return dt.ID
}

// Caps: prog maps to run:prog, run:mut never implies run:prog, run:prog carries only run:ro,
// prof:* need act and are minted from the laptop only.
func TestProgCapsAndMint(t *testing.T) {
	if runCapFor("prog") != "run:prog" || passkeyClasses["prog"] != "run:prog" {
		t.Fatal("prog must map to run:prog and need a passkey")
	}
	mut := deviceToken{Caps: []string{"act", "run:mut"}}
	if mut.hasCap("run:prog") || mut.hasCap("prof:power") {
		t.Fatal("run:mut must not imply run:prog or a profile")
	}
	prog := deviceToken{Caps: []string{"act", "run:prog"}}
	if !prog.hasCap("run:ro") || prog.hasCap("run:mut") || prog.hasCap("prof:power") {
		t.Fatalf("run:prog implies exactly run:ro: %v", prog.effectiveCaps())
	}
	store := newTokenStore(t.TempDir())
	if _, _, err := store.mint("x", "prof:power"); err == nil {
		t.Error("prof:power without act must fail")
	}
	if _, _, err := store.mint("x", "act", "prof:root"); err == nil {
		t.Error("an unknown profile must fail")
	}
	e, _ := armServer(t)
	serve := func(r *http.Request) {}
	for _, caps := range [][]string{{"act", "prof:power"}, {"act", "prof:guarded"}, {"act", "run:prog"}, {"act", " PROF:Power "}} {
		w := e.req("POST", "/api/rc/devices/mint", e.shrd, map[string]any{"name": "p", "caps": caps}, serve)
		expectCode(t, "mint via serve "+strings.Join(caps, ","), w, 403, "laptop only")
	}
	w := e.req("POST", "/api/rc/devices/mint", e.shrd, map[string]any{"name": "p", "caps": []string{"act", "run:prog", "prof:power"}}, local)
	expectCode(t, "local mint", w, 200, `"prof:power"`)
}

// The arm gate sits before the challenge: no Face ID for an unarmed prog step, a run without an
// assertion says needsArm, a run:mut token never runs prog, and an armed step runs with Face ID.
func TestProgArmGateBeforeChallenge(t *testing.T) {
	e, ar := armServer(t)
	prog := mintTok(t, e.s, "phone", "act", "run:prog")
	mut := mintTok(t, e.s, "phone-mut", "act", "run:mut")
	a, b := newSoftAuth(t), newSoftAuth(t)
	e.enrol(a, prog)
	e.enrol(b, mut)

	for _, st := range []string{"missing", "expired", "tampered", "revoked", "key-unknown", "exp-too-far"} {
		ar.state = st
		expectCode(t, "challenge, arm "+st, e.challenge(prog, 3, shaProg), 403, "arm-"+st)
		e.expectRefusedLog("run/challenge", "arm-"+st)
		w := e.postRunMut(prog, 3, shaProg, nil)
		expectCode(t, "run, arm "+st, w, 403, `"needsArm":true`)
	}
	ar.state = "armed"
	expectCode(t, "run:mut on a prog step (challenge)", e.challenge(mut, 3, shaProg), 403, "lacks run:prog")
	expectCode(t, "run:mut on a prog step (run)", e.postRunMut(mut, 3, shaProg, nil), 403, "lacks run:prog")
	expectCode(t, "run:prog on a mut line", e.challenge(prog, 1, shaPhone), 403, "lacks run:mut")
	expectCode(t, "armed, no assertion", e.postRunMut(prog, 3, shaProg, nil), 403, "needs a passkey assertion")
	if n := len(e.run.runs); n != 0 {
		t.Fatalf("refused prog attempts reached the runner %d times", n)
	}
	w := e.postRunMut(prog, 3, shaProg, e.faceID(a, prog, 3, shaProg))
	expectCode(t, "armed prog run", w, 202, `"ok":true`)
	if len(e.run.runs) != 1 || argvFlag(e.run.runs[0], "--auth") == "" {
		t.Fatalf("runs: %q", e.run.runs)
	}
	// Every verify names the calling token's id, so seq.arm_payload is this device's string.
	for _, v := range ar.verifys {
		if d := argvFlag(v, "--device"); d != tokID(t, e.s, prog) && d != tokID(t, e.s, mut) {
			t.Fatalf("verify without the device id: %q", v)
		}
	}
}

// run/verify passes the seq block through with the device id and the arm config; nothing secret.
func TestRunVerifySeqBlock(t *testing.T) {
	e, _ := armServer(t)
	power := mintTok(t, e.s, "phone", "act", "run:prog", "prof:power")
	plain := mintTok(t, e.s, "phone-2", "act", "run:prog")
	var v struct {
		Seq       map[string]any `json:"seq"`
		ArmDevice string         `json:"arm_device"`
		Arm       map[string]any `json:"arm"`
	}
	w := e.req("GET", "/api/rc/tower/run/verify?ask=ask1", power, nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
	id := tokID(t, e.s, power)
	if v.ArmDevice != id || !strings.Contains(v.Seq["arm_payload"].(string), "\n\""+id+"\"\npower\n") {
		t.Fatalf("arm_device %q, payload %q", v.ArmDevice, v.Seq["arm_payload"])
	}
	if v.Arm["enabled"] != true || v.Arm["can"] != true || v.Arm["vault"] != armVault || v.Arm["mount"] != "forgejo-oidc" ||
		v.Arm["role"] != "runbang-signer" || v.Arm["redirect"] != pkOrigin+"/vault/callback" || v.Arm["key"] != "runbang-prog" {
		t.Fatalf("arm config: %v", v.Arm)
	}
	v.Arm = nil
	w = e.req("GET", "/api/rc/tower/run/verify?ask=ask1", plain, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.Arm["can"] != false || !strings.Contains(v.Arm["can_why"].(string), "prof:power") {
		t.Fatalf("a token without prof:power: %v", v.Arm)
	}
	e.s.cfg.vaultURL = ""
	v.Arm = nil
	w = e.req("GET", "/api/rc/tower/run/verify?ask=ask1", power, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.Arm["enabled"] != false || v.Arm["vault"] != nil || !strings.Contains(v.Arm["why"].(string), "--vault-url") {
		t.Fatalf("vault unset: %v", v.Arm)
	}
}

// run/verify must carry items[].line (runner.py emits it): the PWA's armCheck re-hashes it, and a
// missing line made every prog step read "the line does not match its hash" (review P2-1).
func TestRunVerifyCarriesLine(t *testing.T) {
	e, _ := armServer(t)
	power := mintTok(t, e.s, "phone", "act", "run:prog", "prof:power")
	w := e.req("GET", "/api/rc/tower/run/verify?ask=ask1", power, nil)
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &raw) != nil {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
	for _, it := range raw.Items {
		if it["sha256"] == shaProg {
			if it["line"] != "pwd" {
				t.Fatalf("prog item line = %v, want pwd; keys %v", it["line"], it)
			}
			return
		}
	}
	t.Fatalf("no prog item in %s", w.Body.String())
}

// The arm endpoint: caps, config, shape, stale rev, then the runner decides; one line each.
func TestArmEndpoint(t *testing.T) {
	e, ar := armServer(t)
	power := mintTok(t, e.s, "phone", "act", "run:prog", "prof:power")
	progOnly := mintTok(t, e.s, "phone-2", "act", "run:prog")
	guarded := mintTok(t, e.s, "phone-3", "act", "run:prog", "prof:guarded")
	mutPower := mintTok(t, e.s, "phone-4", "act", "run:mut", "prof:power")

	expectCode(t, "run:prog only", e.postArm(progOnly, armBody()), 403, "needs run:prog and prof:power")
	expectCode(t, "guarded never takes a Vault arm (Q-V1)", e.postArm(guarded, armBody()), 403, "needs run:prog and prof:power")
	expectCode(t, "run:mut + prof:power", e.postArm(mutPower, armBody()), 403, "needs run:prog and prof:power")
	expectCode(t, "shared token", e.postArm(e.shrd, armBody()), 403, "")
	for name, mod := range map[string]func(map[string]any){
		"bad rev":         func(b map[string]any) { b["seq_rev"] = "XYZ" },
		"no exp":          func(b map[string]any) { delete(b, "exp") },
		"sig shape":       func(b map[string]any) { b["sig"] = "vault:v1:short" },
		"sig as a flag":   func(b map[string]any) { b["sig"] = "--yes" },
		"ask as a flag":   func(b map[string]any) { b["ask"] = "--revoke" },
		"version differs": func(b map[string]any) { b["version"] = "2" },
	} {
		b := armBody()
		mod(b)
		expectCode(t, name, e.postArm(power, b), 400, "need ask")
	}
	ar.rev = "0123456789abcdef"
	expectCode(t, "patched since the sheet", e.postArm(power, armBody()), 409, `"stale":true`)
	ar.rev = armRev
	if len(ar.arms) != 0 {
		t.Fatalf("refusals reached seq arm: %q", ar.arms)
	}

	w := e.postArm(power, armBody())
	expectCode(t, "arm", w, 200, `"ok":true`)
	if len(ar.arms) != 1 {
		t.Fatalf("seq arm calls: %q", ar.arms)
	}
	argv := ar.arms[0]
	want := map[string]string{"--ask": "ask1", "--via": "phone", "--device": tokID(t, e.s, power), "--login": pkOperator, "--sig": armSig, "--exp": "1792720983"}
	for f, v := range want {
		if argvFlag(argv, f) != v {
			t.Errorf("argv %s = %q, want %q (%q)", f, argvFlag(argv, f), v, argv)
		}
	}
	if argv[3] != "seq" || argv[4] != "arm" || strings.Contains(strings.Join(argv, " "), power) {
		t.Fatalf("argv %q", argv)
	}
	m := e.lastLog("run/arm")
	if m["allowed"] != true || m["device_id"] != tokID(t, e.s, power) || m["sig8"] != "QUFBQUFB" || m["version"] != "1" || m["seq_rev"] != armRev {
		t.Fatalf("run/arm line: %v", m)
	}

	ar.armRC, ar.armOut = 3, `{"id":"ask1:arm:50b7656d:x:refused","refused":"arm-tampered"}`+"\n"
	expectCode(t, "runner refuses", e.postArm(power, armBody()), 403, `"refused":"arm-tampered"`)
	if m := e.lastLog("run/arm-refused"); m["refused"] != "arm-tampered" || m["outcome"] != "runner-refused" {
		t.Fatalf("run/arm-refused line: %v", m)
	}
	ar.armRC, ar.armOut = 1, `{"error":"boom"}`+"\n"
	expectCode(t, "runner error", e.postArm(power, armBody()), 502, "rc=1")

	e.s.cfg.vaultURL = ""
	expectCode(t, "vault unset", e.postArm(power, armBody()), 409, "--vault-url")
	e.s.cfg.vaultURL, e.s.cfg.publicURL = armVault, ""
	expectCode(t, "public url unset", e.postArm(power, armBody()), 409, "--public-url")

	for _, l := range actionLog(t) {
		if b, _ := json.Marshal(l); strings.Contains(string(b), power) || strings.Contains(string(b), armSig) {
			t.Fatalf("action log carries a token or the whole signature: %s", b)
		}
	}
}

// --vault-url: exactly one https origin joins connect-src; anything else adds nothing.
func TestVaultOriginAndCSP(t *testing.T) {
	for in, want := range map[string]string{
		"https://vault.example.ts.net":       "https://vault.example.ts.net",
		"https://vault.example.ts.net/":      "https://vault.example.ts.net",
		" https://vault.example.ts.net:8200": "https://vault.example.ts.net:8200",
		"":                                   "",
		"http://vault.example.ts.net":        "",
		"https://vault.example.ts.net/v1":    "",
		"https://*.ts.net":                   "",
		"https://u:p@vault.example.ts.net":   "",
		"https://vault.example.ts.net?x=1":   "",
		"https://a.ts.net https://b.ts.net":  "",
		"https://vault.example.ts.net; x":    "",
	} {
		if got := vaultOrigin(in); got != want {
			t.Errorf("vaultOrigin(%q) = %q, want %q", in, got, want)
		}
	}
	s := testServer(t, config{token: "tok", homeDir: t.TempDir(), webDir: "web", vaultURL: armVault})
	h := s.routes()
	for _, path := range []string{"/", "/vault/callback?state=s&code=c"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "deck.example.ts.net"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		csp := w.Header().Get("Content-Security-Policy")
		connect := strings.Split(strings.Split(csp, "connect-src ")[1], ";")[0]
		if connect != "'self' wss://deck.example.ts.net ws://deck.example.ts.net "+armVault {
			t.Errorf("%s: connect-src %q", path, connect)
		}
		if strings.Contains(csp, "*") {
			t.Errorf("%s: wildcard in CSP %q", path, csp)
		}
	}
	r := httptest.NewRequest("GET", "/vault/callback?state=s&code=c", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(w.Body.String(), `src="/vault-callback.js"`) {
		t.Fatalf("callback page: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if p := cspPolicy("deck.example.ts.net", ""); strings.Contains(p, "vault") {
		t.Fatalf("no vault configured, yet: %q", p)
	}
}
