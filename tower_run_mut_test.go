package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// run-bang phase 2 #6 (run-bang-phase2-review.md §5 W3): POST /api/rc/tower/run for a mut line.
// pkServer's stub runner reports item 1 (mut, phone-ok) runnable, as tower will after the W4 gate
// flip; preW4 = true is today's runner.

func (e *pkEnv) postRunMut(tok string, item int, sha string, assertion json.RawMessage) *httptest.ResponseRecorder {
	body := map[string]any{"ask": "ask1", "item": item, "sha256": sha}
	if assertion != nil {
		body["assertion"] = assertion
	}
	return e.req("POST", "/api/rc/tower/run", tok, body)
}

// faceID: challenge → the software authenticator's assertion, as the sheet does.
func (e *pkEnv) faceID(a *softAuth, tok string, item int, sha string) json.RawMessage {
	e.t.Helper()
	a.count++
	return a.get(e.t, e.options(e.challenge(tok, item, sha)))
}

func (e *pkEnv) runLines() []map[string]any {
	var out []map[string]any
	for _, m := range actionLog(e.t) {
		if m["action"] == "run" {
			out = append(out, m)
		}
	}
	return out
}

func argvFlag(argv []string, flag string) string {
	for i := range argv {
		if argv[i] == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// The whole mut path: Face ID → run, argv carries the challenge's cwd/timeout and --auth naming
// the credential and challenge; the token never reaches argv; one "run" line, allowed.
func TestRunMutWithAssertion(t *testing.T) {
	e := pkServer(t)
	tok := mintTok(t, e.s, "phone", "act", "run:mut")
	a := newSoftAuth(t)
	e.enrol(a, tok)
	w := e.postRunMut(tok, 1, shaPhone, e.faceID(a, tok, 1, shaPhone))
	expectCode(t, "mut run", w, 202, `"id":"ask1:1:bbbbbbbb"`)
	if len(e.run.runs) != 1 {
		t.Fatalf("runner called %d times", len(e.run.runs))
	}
	argv := e.run.runs[0]
	if argvFlag(argv, "--expect-cwd") != "/Users/op/src" || argvFlag(argv, "--expect-timeout") != "900" || argvFlag(argv, "--via") != "phone" {
		t.Fatalf("argv: %q", argv)
	}
	var auth map[string]string
	if err := json.Unmarshal([]byte(argvFlag(argv, "--auth")), &auth); err != nil || auth["method"] != "passkey" || auth["cred"] != credID8(a.id) || len(auth["challenge"]) != 8 || len(auth) != 3 {
		t.Fatalf("--auth %q: %v", argvFlag(argv, "--auth"), err)
	}
	if strings.Contains(strings.Join(argv, " "), tok) {
		t.Fatal("the token reached argv")
	}
	lines := e.runLines()
	if len(lines) != 1 || lines[0]["allowed"] != true || lines[0]["cred"] != auth["cred"] || lines[0]["challenge"] != auth["challenge"] {
		t.Fatalf("run log: %v", lines)
	}
	if m := e.lastLog("run/assert"); m["allowed"] != true {
		t.Fatalf("assert log: %v", m)
	}
}

func TestRunMutWithMultiplePasskeys(t *testing.T) {
	for _, selected := range []int{0, 1} {
		t.Run([]string{"original", "reenrolled"}[selected], func(t *testing.T) {
			e := pkServer(t)
			tok := mintTok(t, e.s, "phone", "act", "run:mut")
			auths := []*softAuth{newSoftAuth(t), newSoftAuth(t)}
			for _, a := range auths {
				e.enrol(a, tok)
			}
			opts := e.options(e.challenge(tok, 1, shaPhone)) // browser sends no cred
			var out struct {
				PublicKey struct{ AllowCredentials []struct{ ID string } }
			}
			if err := json.Unmarshal(opts, &out); err != nil {
				t.Fatal(err)
			}
			if len(out.PublicKey.AllowCredentials) != 2 {
				t.Fatalf("expected both enrolled credentials: %s", opts)
			}
			for i, c := range out.PublicKey.AllowCredentials {
				if c.ID != b64u(auths[i].id) {
					t.Fatal("challenge includes the wrong credential")
				}
			}
			w := e.postRunMut(tok, 1, shaPhone, auths[selected].get(t, opts))
			expectCode(t, "mut with selected passkey", w, 202, `"id":"ask1:1:bbbbbbbb"`)
			if len(e.run.runs) != 1 {
				t.Fatal("mut run did not reach runner once")
			}
			var auth passkeyReceipt
			if err := json.Unmarshal([]byte(argvFlag(e.run.runs[0], "--auth")), &auth); err != nil {
				t.Fatal(err)
			}
			if auth.Cred != credID8(auths[selected].id) {
				t.Fatal("receipt did not identify signing credential")
			}
		})
	}
}

func TestPasskeyChallengeCredentialAllowList(t *testing.T) {
	for _, scenario := range []string{"other-device", "enrolled-later", "explicit-selection"} {
		t.Run(scenario, func(t *testing.T) {
			e := pkServer(t)
			tok := mintTok(t, e.s, "phone", "act", "run:mut")
			a, b := newSoftAuth(t), newSoftAuth(t)
			e.enrol(a, tok)
			var opts json.RawMessage
			switch scenario {
			case "other-device":
				e.enrol(b, mintTok(t, e.s, "other", "act", "run:mut"))
				opts = e.options(e.challenge(tok, 1, shaPhone))
			case "enrolled-later":
				opts = e.options(e.challenge(tok, 1, shaPhone))
				e.enrol(b, tok)
			case "explicit-selection":
				e.enrol(b, tok)
				opts = e.options(e.req("POST", "/api/rc/tower/run/challenge", tok, map[string]any{"ask": "ask1", "item": 1, "sha256": shaPhone, "cred": b64u(a.id)}))
			}
			if _, code, why := e.assert(tok, 1, shaPhone, b.get(t, opts)); code != 403 || why == "" {
				t.Fatalf("unlisted credential accepted: %d %s", code, why)
			}
			if _, code, why := e.assert(tok, 1, shaPhone, a.get(t, opts)); code != 403 || !strings.Contains(why, "already used") {
				t.Fatalf("refused assertion did not spend challenge: %d %s", code, why)
			}
		})
	}
}

// cap × class for the run endpoint with passkeys in play, and every mut refusal comes before
// the runner and before the dup mark.
func TestRunMutRefusals(t *testing.T) {
	e := pkServer(t)
	actOnly := mintTok(t, e.s, "act-only", "act")
	ro := mintTok(t, e.s, "run-ro", "act", "run:ro")
	mut := mintTok(t, e.s, "phone", "act", "run:mut")
	other := mintTok(t, e.s, "phone-2", "act", "run:mut")
	a, b := newSoftAuth(t), newSoftAuth(t)
	e.enrol(a, mut)
	e.enrol(b, other)

	expectCode(t, "act only", e.postRunMut(actOnly, 1, shaPhone, nil), 403, "lacks the run:ro")
	expectCode(t, "run:ro on a mut line", e.postRunMut(ro, 1, shaPhone, nil), 403, "lacks run:mut")
	expectCode(t, "shared token", e.postRunMut(e.shrd, 1, shaPhone, nil), 403, "shared token cannot")
	expectCode(t, "mut without assertion", e.postRunMut(mut, 1, shaPhone, nil), 403, "needs a passkey assertion")
	expectCode(t, "laptop-only line", e.postRunMut(mut, 2, shaLaptop, e.faceID(a, mut, 1, shaPhone)), 403, "laptop only")
	expectCode(t, "another phone's passkey under this bearer", e.postRunMut(mut, 1, shaPhone, e.faceID(b, other, 1, shaPhone)), 403, "another device token")
	expectCode(t, "garbage assertion", e.postRunMut(mut, 1, shaPhone, json.RawMessage(`{"id":"x"}`)), 400, "bad assertion")

	asr := e.faceID(a, mut, 1, shaPhone)
	e.run.resign()
	expectCode(t, "re-signed since the challenge", e.postRunMut(mut, 1, shaPhone, asr), 409, "re-signed since the challenge")

	e.run.laptopItem = true
	expectCode(t, "phone_ok false", e.postRunMut(mut, 1, shaPhone, nil), 403, "laptop only")
	e.run.laptopItem = false
	e.run.preW4 = true
	expectCode(t, "pre-W4: the runner's gate", e.postRunMut(mut, 1, shaPhone, nil), 403, "runner says not runnable: class:mut")
	if n := len(e.run.runs); n != 0 {
		t.Fatalf("a refused mut attempt reached the runner %d times", n)
	}
	// One "run" line per attempt that reached the handler (the shared token is refused by
	// authAct, whose line names the path), each refused.
	lines := e.runLines()
	if len(lines) != 9 {
		t.Fatalf("%d run lines: %v", len(lines), lines)
	}
	for _, m := range lines {
		if m["allowed"] != false {
			t.Fatalf("refusal logged as allowed: %v", m)
		}
	}
}

// A refused assertion never holds the dup mark; a started run does (429 within 10 s, the P1
// code), and the second tap's Face ID does not reach the runner.
func TestRunMutDupWindow(t *testing.T) {
	e := pkServer(t)
	tok := mintTok(t, e.s, "phone", "act", "run:mut")
	a := newSoftAuth(t)
	e.enrol(a, tok)
	expectCode(t, "refused assertion", e.postRunMut(tok, 1, shaPhone, json.RawMessage(`{}`)), 400, "bad assertion")
	expectCode(t, "first", e.postRunMut(tok, 1, shaPhone, e.faceID(a, tok, 1, shaPhone)), 202, "")
	expectCode(t, "double tap", e.postRunMut(tok, 1, shaPhone, e.faceID(a, tok, 1, shaPhone)), 429, "duplicate tap")
	if n := len(e.run.runs); n != 1 {
		t.Fatalf("runner called %d times", n)
	}
}

// The runner's own refusal (here params-changed) and its hint reach the phone; the dup mark is
// released so a reload and a fresh Face ID can try again.
func TestRunMutRunnerRefusalHint(t *testing.T) {
	e := pkServer(t)
	tok := mintTok(t, e.s, "phone", "act", "run:mut")
	a := newSoftAuth(t)
	e.enrol(a, tok)
	e.run.runRC, e.run.runOut = 3, `{"id":"ask1:1:bbbbbbbb:refused:x","refused":"params-changed","hint":"reload and confirm again"}`+"\n"
	expectCode(t, "params-changed", e.postRunMut(tok, 1, shaPhone, e.faceID(a, tok, 1, shaPhone)), 403, `"hint":"reload and confirm again"`)
	e.run.runRC, e.run.runOut = 0, ""
	expectCode(t, "retry", e.postRunMut(tok, 1, shaPhone, e.faceID(a, tok, 1, shaPhone)), 202, "")
}

// The sheet's verify: the runner's phone_ok passes through untouched, phone_cap and passkey say
// what this phase needs, the ages and the runner's hints are there.
func TestRunVerifyPassesPhoneSubclass(t *testing.T) {
	e := pkServer(t)
	tok := mintTok(t, e.s, "phone", "act", "run:mut")
	w := e.req("GET", "/api/rc/tower/run/verify?ask=ask1", tok, nil)
	var v struct {
		AskTS string           `json:"ask_ts"`
		SigTS string           `json:"sig_ts"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || len(v.Items) != 3 {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
	if v.AskTS == "" || v.SigTS == "" {
		t.Errorf("ages missing: %s", w.Body.String())
	}
	laptop := v.Items[2]
	if laptop["phone_ok"] != false || laptop["phone_cap"] != true || laptop["passkey"] != true || laptop["hint"] == nil {
		t.Errorf("laptop-only item: %v", laptop)
	}
	if mut := v.Items[1]; mut["phone_ok"] != true || mut["exp"] == nil || mut["kid"] != "k1" {
		t.Errorf("mut item: %v", mut)
	}
	if ro := v.Items[0]; ro["passkey"] != false || ro["phone_cap"] != true {
		t.Errorf("ro item: %v", ro)
	}
	who := e.req("GET", "/api/rc/tower/whoami", tok, nil)
	if !strings.Contains(who.Body.String(), `"run:ro"`) {
		t.Errorf("tower whoami must show the implied run:ro: %s", who.Body.String())
	}
}
