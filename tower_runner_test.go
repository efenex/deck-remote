package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Runner mode v1 (tower reports/runbang-runner-mode/interface.md): the five runner endpoints.
// runnerStub wraps armRunner: verify gains the runner block, `runner …` is recorded and answered
// per subcommand.

const (
	runnerSrc   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	runnerNonce = "0123456789abcdef0123456789abcdef"
	runnerWord  = "orchid-delete"
	runnerElev  = "ask1:elev:50b7656d:20261103T010203Z"
)

type runnerStub struct {
	*armRunner
	mu        sync.Mutex
	noRunner  bool
	needsArm  bool
	sig       string // seq.sig ("" = ok)
	src       string
	elevState string
	elevDev   string
	calls     [][]string
	rc        map[string]int
	out       map[string]string
}

func (st *runnerStub) exec(ctx context.Context, argv []string) (int, string) {
	switch argv[3] {
	case "runner":
		st.mu.Lock()
		defer st.mu.Unlock()
		st.calls = append(st.calls, argv)
		sub := argv[4]
		if o, ok := st.out[sub]; ok {
			return st.rc[sub], o
		}
		switch sub {
		case "start":
			return 0, `{"id":"` + runnerElev + `","tmux":"rb-50b7656d","exp":1792720983,"idle_exp":1792714683}` + "\n"
		case "gate":
			if argvFlag(argv, "--nonce") != "" {
				return 0, `{"ok":true,"decision":"` + argvFlag(argv, "--decision") + `","step":3}` + "\n"
			}
			return 0, `{"pending":true,"step":3,"attempt":1,"kind":"word","nonce":"` + runnerNonce + `","title":"p","class":"prog","word_len":13}` + "\n"
		case "end":
			return 0, `{"ok":true}` + "\n"
		}
		return 2, `{"error":"unexpected"}`
	case "verify":
		_, out := st.armRunner.exec(ctx, argv)
		st.mu.Lock()
		defer st.mu.Unlock()
		var v map[string]any
		_ = json.Unmarshal([]byte(out), &v)
		if st.sig != "" {
			v["seq"].(map[string]any)["sig"] = st.sig
		}
		if !st.noRunner {
			v["runner"] = map[string]any{
				"source": map[string]any{"path": "/Users/op/runners/a5.sh", "sha256": st.src}, "prelude_lines": 12, "needs_arm": st.needsArm,
				"phone_steps": []int{0, 1, 3}, "laptop_steps": []int{2},
				"elevation": map[string]any{"state": st.elevState, "id": runnerElev, "device": st.elevDev, "exp": 1792720983},
				"gate":      map[string]any{"pending": true, "step": 3, "kind": "word"},
			}
		}
		return 0, jsonStr(v) + "\n"
	}
	return st.armRunner.exec(ctx, argv)
}

func (st *runnerStub) runnerCalls() [][]string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([][]string{}, st.calls...)
}

func runnerServer(t *testing.T) (*pkEnv, *runnerStub) {
	e, ar := armServer(t)
	st := &runnerStub{armRunner: ar, src: runnerSrc, elevState: "none", rc: map[string]int{}, out: map[string]string{}}
	e.s.towerExec = st.exec
	e.h = e.s.routes()
	return e, st
}

func runnerTok(t *testing.T, e *pkEnv) string {
	return mintTok(t, e.s, "phone", "act", "run:mut", "run:prog")
}

func (e *pkEnv) runnerChallenge(tok, rev string) *httptest.ResponseRecorder {
	return e.req("POST", "/api/rc/tower/runner/challenge", tok, map[string]any{"ask": "ask1", "rev": rev})
}

func (e *pkEnv) runnerFaceID(a *softAuth, tok string) json.RawMessage {
	e.t.Helper()
	a.count++
	return a.get(e.t, e.options(e.runnerChallenge(tok, armRev)))
}

func (e *pkEnv) runnerStart(tok string, assertion json.RawMessage) *httptest.ResponseRecorder {
	body := map[string]any{"ask": "ask1", "rev": armRev}
	if assertion != nil {
		body["assertion"] = assertion
	}
	return e.req("POST", "/api/rc/tower/runner/start", tok, body)
}

func (e *pkEnv) logLines(action string) []map[string]any {
	var out []map[string]any
	for _, m := range actionLog(e.t) {
		if m["action"] == action {
			out = append(out, m)
		}
	}
	return out
}

func wantArgv(t *testing.T, e *pkEnv, got []string, tail ...string) {
	t.Helper()
	want := append([]string{"python3", "-I", filepath.Join(e.s.cfg.towerRootDir(), "scripts", "run_bang.py"), "runner"}, tail...)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
}

// The whole path: challenge → Face ID → start; exact argv (device = token id, --auth names cred
// and challenge, no token anywhere); one log line per call.
func TestRunnerStartHappy(t *testing.T) {
	for _, armed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ro-mut-only", true: "needs-arm-armed"}[armed], func(t *testing.T) {
			e, st := runnerServer(t)
			st.needsArm = armed
			if armed {
				st.state = "armed"
			}
			tok := runnerTok(t, e)
			a := newSoftAuth(t)
			e.enrol(a, tok)
			ch := e.runnerChallenge(tok, armRev)
			expectCode(t, "challenge", ch, 200, `"sha256":"`+runnerSrc+`"`)
			a.count++
			w := e.runnerStart(tok, a.get(t, e.options(ch)))
			expectCode(t, "start", w, 202, `"elevation":"`+runnerElev+`"`)
			calls := st.runnerCalls()
			if len(calls) != 1 {
				t.Fatalf("runner calls: %q", calls)
			}
			var auth map[string]string
			if err := json.Unmarshal([]byte(argvFlag(calls[0], "--auth")), &auth); err != nil || auth["method"] != "passkey" || auth["cred"] != credID8(a.id) || len(auth["challenge"]) != 8 || len(auth) != 3 {
				t.Fatalf("--auth %q", argvFlag(calls[0], "--auth"))
			}
			wantArgv(t, e, calls[0], "start", "--ask", "ask1", "--expect-rev", armRev, "--via", "phone", "--device", tokID(t, e.s, tok),
				"--login", pkOperator, "--auth", argvFlag(calls[0], "--auth"))
			if strings.Contains(strings.Join(calls[0], " "), tok) {
				t.Fatal("token reached argv")
			}
			for _, act := range []string{"runner/challenge", "runner/assert", "runner/start"} {
				if l := e.logLines(act); len(l) != 1 || l[0]["allowed"] != true {
					t.Fatalf("%s log: %v", act, l)
				}
			}
			if l := e.logLines("runner/start")[0]; l["challenge"] != auth["challenge"] || l["sha8"] != runnerSrc[:8] || l["elevation"] != runnerElev {
				t.Fatalf("start log: %v", l)
			}
		})
	}
}

// Every refusal before Face ID: no challenge is issued and the runner CLI is never called.
func TestRunnerChallengeRefusals(t *testing.T) {
	e, st := runnerServer(t)
	tok := runnerTok(t, e)
	a := newSoftAuth(t)
	e.enrol(a, tok)
	for _, c := range [][]string{{"act"}, {"act", "run:mut"}, {"act", "run:prog"}, {"act", "run:ro"}} {
		other := mintTok(t, e.s, "t-"+strings.Join(c, "-"), c...)
		expectCode(t, "caps "+strings.Join(c, ","), e.runnerChallenge(other, armRev), 403, "lacks run:")
	}
	expectCode(t, "bad rev", e.runnerChallenge(tok, "zz"), 400, "need ask")
	expectCode(t, "dash ask", e.req("POST", "/api/rc/tower/runner/challenge", tok, map[string]any{"ask": "-x", "rev": armRev}), 400, "need ask")
	expectCode(t, "space in ask", e.req("POST", "/api/rc/tower/runner/challenge", tok, map[string]any{"ask": "a b", "rev": armRev}), 400, "need ask")
	expectCode(t, "bad json", e.req("POST", "/api/rc/tower/runner/challenge", tok, "{"), 400, "bad json")
	expectCode(t, "stale rev", e.runnerChallenge(tok, "0000000000000000"), 409, "seq-rev-changed")

	st.noRunner = true
	expectCode(t, "not a runner", e.runnerChallenge(tok, armRev), 404, "not-a-runner")
	st.noRunner = false
	st.sig = "tampered"
	expectCode(t, "seq sig", e.runnerChallenge(tok, armRev), 403, "sig-tampered")
	st.sig = ""
	st.needsArm = true
	w := e.runnerChallenge(tok, armRev)
	expectCode(t, "unarmed", w, 403, `"refused":"arm-missing"`)
	expectCode(t, "unarmed needsArm", w, 403, `"needsArm":true`)
	st.state = "expired"
	expectCode(t, "arm expired", e.runnerChallenge(tok, armRev), 403, "arm-expired")
	st.needsArm = false
	st.elevState, st.elevDev = "elevated", "someone-else"
	w = e.runnerChallenge(tok, armRev)
	expectCode(t, "elevated elsewhere", w, 409, "already-elevated")
	if strings.Contains(w.Body.String(), runnerElev) {
		t.Fatalf("another device's elevation id leaked: %s", w.Body.String())
	}
	st.elevDev = tokID(t, e.s, tok)
	expectCode(t, "own elevation: resume", e.runnerChallenge(tok, armRev), 409, `"elevation":"`+runnerElev+`"`)

	if n := len(st.runnerCalls()); n != 0 {
		t.Fatalf("runner CLI called %d times", n)
	}
	e.s.passkeys.mu.Lock()
	n := len(e.s.passkeys.chals)
	e.s.passkeys.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d challenges issued by refusals", n)
	}
	for _, l := range e.logLines("runner/challenge") {
		if l["allowed"] != false {
			t.Fatalf("refusal logged allowed: %v", l)
		}
	}
}

func TestRunnerStartRefusals(t *testing.T) {
	e, st := runnerServer(t)
	tok := runnerTok(t, e)
	other := mintTok(t, e.s, "phone-2", "act", "run:mut", "run:prog")
	mutOnly := mintTok(t, e.s, "mut-only", "act", "run:mut")
	a, b := newSoftAuth(t), newSoftAuth(t)
	e.enrol(a, tok)
	e.enrol(b, other)

	expectCode(t, "no cap", e.runnerStart(mutOnly, json.RawMessage(`{}`)), 403, "lacks run:prog")
	expectCode(t, "no assertion", e.runnerStart(tok, nil), 403, "needs a passkey assertion")
	expectCode(t, "bad rev", e.req("POST", "/api/rc/tower/runner/start", tok, map[string]any{"ask": "ask1", "rev": "ABC"}), 400, "need ask")
	expectCode(t, "garbage", e.runnerStart(tok, json.RawMessage(`{"id":"x"}`)), 400, "bad assertion")
	expectCode(t, "another phone's Face ID", e.runnerStart(tok, e.runnerFaceID(b, other)), 403, "another device token")

	stale := e.runnerFaceID(a, tok)
	e.clock = e.clock.Add(61 * time.Second)
	expectCode(t, "stale challenge", e.runnerStart(tok, stale), 403, "challenge expired")

	// a line-run challenge's assertion is not a runner elevation, and vice versa
	line := e.req("POST", "/api/rc/tower/run/challenge", tok, map[string]any{"ask": "ask1", "item": 1, "sha256": shaPhone})
	a.count++
	expectCode(t, "line challenge at runner/start", e.runnerStart(tok, a.get(t, e.options(line))), 403, "another line or runner")
	expectCode(t, "runner challenge at run", e.postRunMut(tok, 1, shaPhone, e.runnerFaceID(a, tok)), 403, "another line")

	asr := e.runnerFaceID(a, tok)
	st.src = strings.Repeat("f", 64) // the file was re-signed between the challenge and the start
	expectCode(t, "re-signed", e.runnerStart(tok, asr), 409, "re-signed since the challenge")
	st.src = runnerSrc

	asr = e.runnerFaceID(a, tok)
	st.needsArm = true // the arm lapsed between the challenge and the start: refused before the assertion
	expectCode(t, "arm lapsed", e.runnerStart(tok, asr), 403, "arm-missing")
	st.needsArm = false

	if n := len(st.runnerCalls()); n != 0 {
		t.Fatalf("refused starts reached the runner %d times", n)
	}
	// replay: a spent assertion never starts twice
	asr = e.runnerFaceID(a, tok)
	expectCode(t, "first", e.runnerStart(tok, asr), 202, "")
	expectCode(t, "replay", e.runnerStart(tok, asr), 403, "already used")
	expectCode(t, "double tap", e.runnerStart(tok, e.runnerFaceID(a, tok)), 429, "duplicate tap")
	if n := len(st.runnerCalls()); n != 1 {
		t.Fatalf("runner called %d times", n)
	}
}

// exit 3 → 403 with refused/hint, dup mark released; seq-rev-changed → 409.
func TestRunnerStartRunnerRefusal(t *testing.T) {
	e, st := runnerServer(t)
	tok := runnerTok(t, e)
	a := newSoftAuth(t)
	e.enrol(a, tok)
	st.rc["start"], st.out["start"] = 3, `{"refused":"already-elevated","hint":"end it first"}`+"\n"
	w := e.runnerStart(tok, e.runnerFaceID(a, tok))
	expectCode(t, "refused", w, 403, `"refused":"already-elevated"`)
	expectCode(t, "hint", w, 403, `"hint":"end it first"`)
	st.out["start"] = `{"refused":"seq-rev-changed"}` + "\n"
	expectCode(t, "rev moved", e.runnerStart(tok, e.runnerFaceID(a, tok)), 409, "seq-rev-changed")
	st.rc["start"], st.out["start"] = 1, "Traceback\n"
	expectCode(t, "crash", e.runnerStart(tok, e.runnerFaceID(a, tok)), 502, "rc=1")
	delete(st.out, "start")
	expectCode(t, "retry after refusals", e.runnerStart(tok, e.runnerFaceID(a, tok)), 202, "")
}

func TestRunnerGatePoll(t *testing.T) {
	e, st := runnerServer(t)
	tok := runnerTok(t, e)
	w := e.req("GET", "/api/rc/tower/runner/gate?elevation="+runnerElev, tok, nil)
	expectCode(t, "poll", w, 200, `"nonce":"`+runnerNonce+`"`)
	wantArgv(t, e, st.runnerCalls()[0], "gate", "--elevation", runnerElev, "--device", tokID(t, e.s, tok), "--pending")
	l := e.logLines("runner/gate-poll")
	if len(l) != 1 || l[0]["nonce8"] != runnerNonce[:8] || l[0]["allowed"] != true {
		t.Fatalf("poll log: %v", l)
	}
	st.out["gate"] = `{"pending":false,"running":{"step":2,"id":"ask1:2:aaaaaaaa:r50b7656d"}}` + "\n"
	expectCode(t, "running", e.req("GET", "/api/rc/tower/runner/gate?elevation="+runnerElev, tok, nil), 200, `"id":"ask1:2:aaaaaaaa:r50b7656d"`)
	st.rc["gate"], st.out["gate"] = 3, `{"refused":"wrong-device"}`+"\n"
	expectCode(t, "wrong device", e.req("GET", "/api/rc/tower/runner/gate?elevation="+runnerElev, tok, nil), 403, `"refused":"wrong-device"`)
	expectCode(t, "bad id", e.req("GET", "/api/rc/tower/runner/gate?elevation=-x", tok, nil), 400, "need elevation")
	expectCode(t, "no id", e.req("GET", "/api/rc/tower/runner/gate", tok, nil), 400, "need elevation")
	expectCode(t, "no cap", e.req("GET", "/api/rc/tower/runner/gate?elevation="+runnerElev, mintTok(t, e.s, "ro", "act", "run:ro"), nil), 403, "lacks run:mut")
	if n := len(st.runnerCalls()); n != 3 {
		t.Fatalf("runner calls %d", n)
	}
}

func (e *pkEnv) gate(tok string, body map[string]any) *httptest.ResponseRecorder {
	return e.req("POST", "/api/rc/tower/runner/gate", tok, body)
}

func TestRunnerGateDecide(t *testing.T) {
	e, st := runnerServer(t)
	tok := runnerTok(t, e)
	body := func(kv ...string) map[string]any {
		m := map[string]any{"elevation": runnerElev, "nonce": runnerNonce, "decision": "run"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	expectCode(t, "word gate", e.gate(tok, body("word", runnerWord)), 200, `"step":3`)
	wantArgv(t, e, st.runnerCalls()[0], "gate", "--elevation", runnerElev, "--device", tokID(t, e.s, tok), "--nonce", runnerNonce, "--decision", "run", "--word="+runnerWord)
	expectCode(t, "double tap", e.gate(tok, body("word", runnerWord)), 429, "duplicate tap")

	for name, b := range map[string]map[string]any{
		"bad decision":   body("decision", "yes"),
		"nonce not hex":  body("nonce", strings.Repeat("Z", 32)),
		"nonce short":    body("nonce", "abcd"),
		"dash elevation": body("elevation", "-x"),
		"long word":      body("word", strings.Repeat("w", 65)),
		"word newline":   body("word", "a\nb"),
		"word with skip": body("decision", "skip", "word", runnerWord),
	} {
		expectCode(t, name, e.gate(tok, b), 400, "")
	}
	expectCode(t, "no cap", e.gate(mintTok(t, e.s, "mut", "act", "run:mut"), body("nonce", strings.Repeat("1", 32))), 403, "lacks run:prog")

	n2 := strings.Repeat("2", 32)
	st.rc["gate"], st.out["gate"] = 3, `{"refused":"word-mismatch"}`+"\n"
	expectCode(t, "mismatch", e.gate(tok, body("nonce", n2, "word", "orchid-delet")), 403, `"refused":"word-mismatch"`)
	delete(st.out, "gate")
	expectCode(t, "retry after refusal", e.gate(tok, body("nonce", n2, "decision", "skip")), 200, `"decision":"skip"`)
	if n := len(st.runnerCalls()); n != 3 {
		t.Fatalf("runner calls %d", n)
	}
	b, _ := json.Marshal(actionLog(t))
	if strings.Contains(string(b), runnerWord) || strings.Contains(string(b), "orchid-delet\"") || strings.Contains(string(b), runnerNonce) || strings.Contains(string(b), n2) {
		t.Fatalf("word or full nonce in the action log: %s", b)
	}
	if l := e.logLines("runner/gate"); len(l) != 12 || l[0]["nonce8"] != runnerNonce[:8] || l[0]["word_len"] != float64(len(runnerWord)) {
		t.Fatalf("gate log (%d): %v", len(l), l)
	}
}

func TestRunnerEnd(t *testing.T) {
	e, st := runnerServer(t)
	tok := runnerTok(t, e)
	expectCode(t, "end", e.req("POST", "/api/rc/tower/runner/end", tok, map[string]any{"elevation": runnerElev}), 200, `"ok":true`)
	wantArgv(t, e, st.runnerCalls()[0], "end", "--elevation", runnerElev, "--device", tokID(t, e.s, tok), "--reason", "operator")
	st.rc["end"], st.out["end"] = 3, `{"refused":"ended"}`+"\n"
	expectCode(t, "already ended", e.req("POST", "/api/rc/tower/runner/end", tok, map[string]any{"elevation": runnerElev}), 403, `"refused":"ended"`)
	expectCode(t, "bad id", e.req("POST", "/api/rc/tower/runner/end", tok, map[string]any{"elevation": ""}), 400, "need elevation")
	expectCode(t, "no cap", e.req("POST", "/api/rc/tower/runner/end", mintTok(t, e.s, "x", "act"), map[string]any{"elevation": runnerElev}), 403, "lacks run:mut")
	if l := e.logLines("runner/end"); len(l) != 4 || l[0]["allowed"] != true {
		t.Fatalf("end log: %v", l)
	}
}

// The sheet's data: run/verify passes the runner block through; a loose ask has none.
func TestRunnerVerifyPassthrough(t *testing.T) {
	e, st := runnerServer(t)
	tok := runnerTok(t, e)
	expectCode(t, "runner", e.req("GET", "/api/rc/tower/run/verify?ask=ask1", tok, nil), 200, `"prelude_lines":12`)
	st.noRunner = true
	if w := e.req("GET", "/api/rc/tower/run/verify?ask=ask1", tok, nil); strings.Contains(w.Body.String(), `"runner"`) {
		t.Fatalf("runner block on a non-runner: %s", w.Body.String())
	}
}

// Every runner route sits behind authAct: the shared token and a non-operator login are refused.
func TestRunnerRoutesBehindAuthAct(t *testing.T) {
	e, st := runnerServer(t)
	tok := runnerTok(t, e)
	notOp := func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "mallory@example.com") }
	for _, p := range [][2]string{{"POST", "/api/rc/tower/runner/challenge"}, {"POST", "/api/rc/tower/runner/start"},
		{"GET", "/api/rc/tower/runner/gate?elevation=" + runnerElev}, {"POST", "/api/rc/tower/runner/gate"}, {"POST", "/api/rc/tower/runner/end"}} {
		expectCode(t, p[1]+" shared", e.req(p[0], p[1], e.shrd, map[string]any{}), 403, "")
		expectCode(t, p[1]+" not operator", e.req(p[0], p[1], tok, map[string]any{}, notOp), 403, "not the operator")
	}
	if n := len(st.runnerCalls()); n != 0 {
		t.Fatalf("runner called %d times", n)
	}
}
