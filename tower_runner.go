package main

// Runner mode v1, deck-remote side (tower reports/runbang-runner-mode/interface.md, design §10/§10.1:
// RM1a one Face ID per elevation, RM4a the gate is a nonce socket, RM5a idle 15 min / hard 2 h).
//
// A runner is a sequence ask compiled from a runner file; `run_bang.py verify` gives it a top-level
// `runner` block. The phone elevates once (Face ID, plus the Vault arm when needs_arm) and then
// answers the runner loop's gates one by one. deck-remote never runs anything itself: every call
// is tower's `python3 -I <root>/scripts/run_bang.py runner …` with the minimal env (tower_run.go).
//
//   POST /api/rc/tower/runner/challenge {ask, rev[, cred]}   passkey options; the 60 s single-use
//        challenge is bound to (ask, rev, device-token id, the runner file's sha256). Refused before
//        any Face ID when the ask is not a runner, seq.sig != ok, rev is stale, the runner
//        needs_arm and seq.arm.state != armed, or an elevation is already live.
//   POST /api/rc/tower/runner/start {ask, rev, assertion}    the assertion (same rules as the line
//        run, passkeys.go spendAssertion) → `runner start --ask --expect-rev --via phone --device
//        <token id> --login <ts login> --auth {method,cred,challenge}`
//   GET  /api/rc/tower/runner/gate?elevation=                → `runner gate --pending` (the nonce
//        travels only here, never in verify, which run:ro tokens read)
//   POST /api/rc/tower/runner/gate {elevation, nonce, decision, word?}   → `runner gate …`
//   POST /api/rc/tower/runner/end {elevation}                → `runner end --reason operator`
//
// All behind authAct plus the device token's run:mut AND run:prog. One action-log line per call
// (runner/challenge, runner/assert, runner/start, runner/gate-poll, runner/gate, runner/end); the
// nonce is logged as nonce8, the retyped word never.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// runnerCaps: a runner may hold mut and prog steps; one elevation covers both (RM-1 a).
var runnerCaps = []string{"run:mut", "run:prog"}

var (
	runnerIDRe    = regexp.MustCompile(`^[!-~]{1,200}$`)           // ask ids: printable ASCII, no space
	runnerElevRe  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`) // <ask>:elev:<rev8>:<stamp>.<pid>; the structure is not parsed
	runnerNonceRe = regexp.MustCompile(`^[0-9a-f]{16,128}$`)       // the interface pins no length: hex, 64..512 bits
	runnerWordRe  = regexp.MustCompile(`^[\x20-\x7e]{1,64}$`)
	runnerDecide  = map[string]bool{"run": true, "skip": true, "quit": true, "retry": true}
)

func runnerIDOK(id string, max int) bool {
	return len(id) <= max && runnerIDRe.MatchString(id) && !strings.HasPrefix(id, "-")
}

func runnerElevOK(id string) bool {
	return runnerElevRe.MatchString(id) && !strings.HasPrefix(id, "-")
}

// runnerBlock is the part of verify's runner block deck-remote decides on; the PWA gets it whole.
type runnerBlock struct {
	Source struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"source"`
	PreludeLines int  `json:"prelude_lines"`
	NeedsArm     bool `json:"needs_arm"`
	Elevation    struct {
		State  string `json:"state"`
		ID     string `json:"id"`
		Device string `json:"device"`
	} `json:"elevation"`
}

// runner is verify's runner block, nil for any other ask.
func (v *runVerify) runner() *runnerBlock {
	if len(v.Runner) == 0 || string(v.Runner) == "null" {
		return nil
	}
	var rb runnerBlock
	if json.Unmarshal(v.Runner, &rb) != nil {
		return nil
	}
	return &rb
}

// seqRevSig is seq.rev and seq.sig ("" when verify has no sequence block).
func (v *runVerify) seqRevSig() (string, string) {
	var sq struct {
		Rev string `json:"rev"`
		Sig string `json:"sig"`
	}
	if len(v.Seq) > 0 {
		_ = json.Unmarshal(v.Seq, &sq)
	}
	return sq.Rev, sq.Sig
}

// runnerRefusal is a refusal before any Face ID or runner exec: code, reason and the body extras.
type runnerRefusal struct {
	code   int
	reason string
	extra  map[string]any
}

// runnerOK is the runner-side gate shared by the challenge and the start: an open runner ask at
// revision rev, signed, armed when it needs to be, and not already elevated. dt is the caller.
func runnerOK(v *runVerify, rev string, dt deviceToken) (*runnerBlock, *runnerRefusal) {
	if v.Status != "open" {
		return nil, &runnerRefusal{http.StatusNotFound, "ask is no longer open", map[string]any{"refused": "not-open"}}
	}
	rb := v.runner()
	if rb == nil || !runSHARe.MatchString(rb.Source.SHA256) {
		return nil, &runnerRefusal{http.StatusNotFound, "not a runner ask (verify has no runner block)", map[string]any{"refused": "not-a-runner"}}
	}
	seqRev, sig := v.seqRevSig()
	if sig != "ok" {
		return rb, &runnerRefusal{http.StatusForbidden, "the sequence signature is " + sig + ": re-sign it on the laptop", map[string]any{"refused": "sig-" + sig}}
	}
	if seqRev != rev {
		return rb, &runnerRefusal{http.StatusConflict, "the runner was re-signed since the sheet loaded (rev " + seqRev + "): reload", map[string]any{"refused": "seq-rev-changed", "stale": true}}
	}
	if rb.NeedsArm {
		if st := v.armState(); st != "armed" {
			return rb, &runnerRefusal{http.StatusForbidden, "this runner needs a Vault-signed arm of its revision: arm-" + st,
				map[string]any{"refused": "arm-" + st, "needsArm": true, "hint": runHint("arm-"+st, v.Ask)}}
		}
	}
	if rb.Elevation.State == "elevated" {
		extra := map[string]any{"refused": "already-elevated"}
		if rb.Elevation.Device == dt.ID {
			extra["elevation"] = rb.Elevation.ID // this phone's own: the sheet resumes it
		}
		return rb, &runnerRefusal{http.StatusConflict, "runner mode is already on for this runner", extra}
	}
	return rb, nil
}

// runnerActor: authAct already passed; the device token must carry run:mut and run:prog.
func (s *server) runnerActor(r *http.Request) (deviceToken, string) {
	dt, ok := s.tokens.find(bearerHeaderOnly(r))
	if !ok {
		return dt, "device token required"
	}
	for _, c := range runnerCaps {
		if !dt.hasCap(c) {
			return dt, "device token " + dt.Name + " lacks " + c + " (runner mode needs run:mut and run:prog)"
		}
	}
	return dt, ""
}

// runnerRefuser returns the handler's refuse: one action-log line, a JSON error body with extras.
func (s *server) runnerRefuser(w http.ResponseWriter, entry map[string]any) func(int, string, map[string]any) {
	return func(code int, reason string, extra map[string]any) {
		entry["reason"], entry["code"] = reason, code
		if extra != nil && extra["refused"] != nil {
			entry["refused"] = extra["refused"]
		}
		s.towerLog(entry)
		body := map[string]any{"error": reason}
		for k, v := range extra {
			body[k] = v
		}
		writeJSON(w, code, body)
	}
}

// runnerResult is the runner CLI's last stdout line, kept raw for passthrough.
type runnerResult struct {
	rc  int
	raw json.RawMessage
	m   map[string]any
	out string
}

func (s *server) runnerExec(args ...string) runnerResult {
	rc, out := s.runBang(s.runBangArgv(append([]string{"runner"}, args...)...))
	res := runnerResult{rc: rc, out: out}
	if ln := lastLine(out); json.Unmarshal([]byte(ln), &res.m) == nil && res.m != nil {
		res.raw = json.RawMessage(ln)
	}
	return res
}

func (r runnerResult) str(k string) string {
	v, _ := r.m[k].(string)
	return v
}

// fail answers a non-zero exit: 3 = the runner refused (403, 409 for a moved revision, refused and
// hint passed through), 2 = usage (400), else 502. scrub is removed from any text (the word).
func (r runnerResult) fail(refuse func(int, string, map[string]any), what, scrub string) {
	clean := func(t string) string {
		if scrub != "" {
			t = strings.ReplaceAll(t, scrub, "***")
		}
		return oneLineGo(t, 160)
	}
	switch r.rc {
	case 3:
		code := http.StatusForbidden
		if r.str("refused") == "seq-rev-changed" {
			code = http.StatusConflict
		}
		extra := map[string]any{"refused": r.str("refused")}
		if h := r.str("hint"); h != "" {
			extra["hint"] = clean(h)
		}
		refuse(code, "runner refused: "+r.str("refused"), extra)
	case 2:
		refuse(http.StatusBadRequest, "run_bang.py runner "+what+": "+clean(r.str("error")), nil)
	default:
		refuse(http.StatusBadGateway, fmt.Sprintf("run_bang.py runner %s rc=%d: %s", what, r.rc, clean(r.str("error")+" "+r.out)), nil)
	}
}

type runnerChallengeReq struct {
	Ask  string `json:"ask"`
	Rev  string `json:"rev"`
	Cred string `json:"cred"`
}

// POST /api/rc/tower/runner/challenge — see the file comment. Gated by authAct.
func (s *server) handleRunnerChallenge(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "runner/challenge", "allowed": false, "login": tsLogin(r)}
	refuse := s.runnerRefuser(w, entry)
	dt, why := s.runnerActor(r)
	entry["device"] = dt.Name
	var req runnerChallengeReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "bad json", nil)
		return
	}
	entry["ask"], entry["rev"] = req.Ask, req.Rev
	if !runnerIDOK(req.Ask, 200) || !armRevRe.MatchString(req.Rev) {
		refuse(http.StatusBadRequest, "need ask (printable id) and rev (16 hex)", nil)
		return
	}
	if why != "" {
		refuse(http.StatusForbidden, why, nil)
		return
	}
	wa, why := s.webAuthn()
	if why != "" {
		refuse(http.StatusConflict, why, nil)
		return
	}
	if !s.passkeys.allow("challenge:" + dt.ID) {
		refuse(http.StatusTooManyRequests, "too many passkey requests; wait a minute", nil)
		return
	}
	v, err := s.runVerify(req.Ask, dt.ID)
	if err != nil {
		refuse(http.StatusBadGateway, err.Error(), nil)
		return
	}
	rb, bad := runnerOK(v, req.Rev, dt)
	if rb != nil {
		entry["sha8"] = rb.Source.SHA256[:min(8, len(rb.Source.SHA256))]
	}
	if bad != nil {
		refuse(bad.code, bad.reason, bad.extra)
		return
	}
	assertion, session, code, why := s.beginPasskey(wa, dt, req.Cred, entry)
	if why != "" {
		refuse(code, why, nil)
		return
	}
	ch := s.passkeys.storeChallenge(session.Challenge, runChallenge{
		id: challengeID8(session.Challenge), kind: "runner", ask: req.Ask, item: -1, rev: req.Rev, runnerSHA: rb.Source.SHA256,
		device: dt.ID, session: *session,
	})
	entry["allowed"], entry["challenge"] = true, ch.id
	s.towerLog(entry)
	writeJSON(w, http.StatusOK, map[string]any{
		"options": assertion, "challenge": ch.id, "expiresIn": int(passkeyChallengeTTL / time.Second),
		"rev": req.Rev, "source": map[string]any{"path": rb.Source.Path, "sha256": rb.Source.SHA256},
	})
}

type runnerStartReq struct {
	Ask       string          `json:"ask"`
	Rev       string          `json:"rev"`
	Assertion json.RawMessage `json:"assertion"`
}

// POST /api/rc/tower/runner/start — see the file comment. Gated by authAct.
func (s *server) handleRunnerStart(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "runner/start", "allowed": false, "login": tsLogin(r)}
	refuse := s.runnerRefuser(w, entry)
	dt, why := s.runnerActor(r)
	entry["device"] = dt.Name
	var req runnerStartReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, passkeyBodyMax)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "bad json", nil)
		return
	}
	entry["ask"], entry["rev"] = req.Ask, req.Rev
	if !runnerIDOK(req.Ask, 200) || !armRevRe.MatchString(req.Rev) {
		refuse(http.StatusBadRequest, "need ask (printable id) and rev (16 hex)", nil)
		return
	}
	if why != "" {
		refuse(http.StatusForbidden, why, nil)
		return
	}
	if len(req.Assertion) == 0 {
		refuse(http.StatusForbidden, "runner mode needs a passkey assertion (Face ID)", map[string]any{"needsAssertion": true})
		return
	}
	s.towerActMu.Lock()
	defer s.towerActMu.Unlock()
	v, err := s.runVerify(req.Ask, dt.ID)
	if err != nil {
		refuse(http.StatusBadGateway, err.Error(), nil)
		return
	}
	rb, bad := runnerOK(v, req.Rev, dt)
	if rb != nil {
		entry["sha8"] = rb.Source.SHA256[:min(8, len(rb.Source.SHA256))]
	}
	if bad != nil {
		refuse(bad.code, bad.reason, bad.extra)
		return
	}
	aEntry := map[string]any{"action": "runner/assert", "allowed": false, "device": dt.Name, "login": tsLogin(r), "ask": req.Ask, "rev": req.Rev}
	bind := func(ch runChallenge) string {
		if ch.kind != "runner" || ch.ask != req.Ask || ch.rev != req.Rev {
			return "the challenge is for another line or runner"
		}
		return ""
	}
	recheck := func(ch runChallenge) (int, string) {
		if ch.runnerSHA != rb.Source.SHA256 {
			return http.StatusConflict, "re-signed since the challenge: try again"
		}
		return 0, ""
	}
	rcpt, code, why := s.spendAssertion(r, dt, req.Assertion, aEntry, bind, recheck)
	if why != "" {
		entry["outcome"] = "assertion-refused"
		refuse(code, why, map[string]any{"assertion": "refused"})
		return
	}
	entry["cred"], entry["challenge"] = rcpt.Cred, rcpt.Challenge
	dupKey := "runner-start\x00" + req.Rev
	if s.towerDup(req.Ask, dupKey) {
		refuse(http.StatusTooManyRequests, "duplicate tap: runner mode was just started", nil)
		return
	}
	auth, _ := json.Marshal(map[string]string{"method": rcpt.Method, "cred": rcpt.Cred, "challenge": rcpt.Challenge})
	res := s.runnerExec("start", "--ask", req.Ask, "--expect-rev", req.Rev, "--via", "phone", "--device", dt.ID,
		"--login", tsLogin(r), "--auth", string(auth))
	id := res.str("id")
	entry["elevation"] = id
	if res.rc != 0 || id == "" {
		s.towerUndup(req.Ask, dupKey)
		entry["outcome"], entry["rc"] = "runner-refused", res.rc
		if res.rc == 0 {
			res.rc = -1 // exit 0 without an elevation id is not a start
		}
		res.fail(refuse, "start", "")
		return
	}
	entry["allowed"], entry["outcome"] = true, "elevated"
	s.towerLog(entry)
	s.towerM.invalidate()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "elevation": id, "tmux": res.m["tmux"], "exp": res.m["exp"], "idle_exp": res.m["idle_exp"]})
}

// GET /api/rc/tower/runner/gate?elevation= — the pending gate (with its nonce) or the running
// step, for this device's elevation. Gated by authAct.
// runnerPollSeen: (device id, elevation) → the last logged poll summary (handleRunnerGatePoll).
var runnerPollSeen sync.Map

func (s *server) handleRunnerGatePoll(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "runner/gate-poll", "allowed": false, "login": tsLogin(r)}
	refuse := s.runnerRefuser(w, entry)
	dt, why := s.runnerActor(r)
	entry["device"] = dt.Name
	elev := r.URL.Query().Get("elevation")
	entry["elevation"] = elev
	if !runnerElevOK(elev) {
		entry["elevation"] = oneLineGo(elev, 80)
		refuse(http.StatusBadRequest, "need elevation (id of [A-Za-z0-9._:-], up to 256)", nil)
		return
	}
	if why != "" {
		refuse(http.StatusForbidden, why, nil)
		return
	}
	res := s.runnerExec("gate", "--elevation", elev, "--device", dt.ID, "--pending")
	if res.rc != 0 || res.raw == nil {
		if res.rc == 0 {
			res.rc = -1
		}
		res.fail(refuse, "gate", "")
		return
	}
	entry["allowed"], entry["pending"] = true, res.m["pending"]
	if st, ok := res.m["step"]; ok {
		entry["step"] = st
	}
	if k := res.str("kind"); k != "" {
		entry["kind"] = k
	}
	if run, ok := res.m["running"].(map[string]any); ok {
		entry["running"] = run["step"] // a step is running: the PWA streams run id running.id
	}
	if n := res.str("nonce"); n != "" {
		entry["nonce8"] = n[:min(8, len(n))]
	}
	// The PWA polls every 2 s while elevated: log an allowed poll only when what it saw changed (a new gate, a
	// step started or finished), not ~1800 identical lines an hour. Refusals are always logged (runnerRefuser).
	seen := fmt.Sprintf("%v|%v|%v|%v", entry["pending"], entry["step"], entry["nonce8"], entry["running"])
	if prev, ok := runnerPollSeen.Swap(dt.ID+"\x00"+elev, seen); !ok || prev.(string) != seen {
		s.towerLog(entry)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res.raw)
}

type runnerGateReq struct {
	Elevation string `json:"elevation"`
	Nonce     string `json:"nonce"`
	Decision  string `json:"decision"`
	Word      string `json:"word,omitempty"`
}

// POST /api/rc/tower/runner/gate — one decision for the pending gate. Gated by authAct.
func (s *server) handleRunnerGate(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "runner/gate", "allowed": false, "login": tsLogin(r)}
	refuse := s.runnerRefuser(w, entry)
	dt, why := s.runnerActor(r)
	entry["device"] = dt.Name
	var req runnerGateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "bad json", nil)
		return
	}
	entry["elevation"], entry["decision"] = oneLineGo(req.Elevation, 300), oneLineGo(req.Decision, 16)
	if len(req.Nonce) >= 8 {
		entry["nonce8"] = oneLineGo(req.Nonce[:8], 8)
	}
	if req.Word != "" {
		entry["word_len"] = len(req.Word)
	}
	switch {
	case !runnerElevOK(req.Elevation) || !runnerNonceRe.MatchString(req.Nonce) || !runnerDecide[req.Decision]:
		refuse(http.StatusBadRequest, "need elevation (id of [A-Za-z0-9._:-], up to 256), nonce (hex) and decision (run|skip|quit|retry)", nil)
		return
	case req.Word != "" && (!runnerWordRe.MatchString(req.Word) || req.Decision != "run"):
		refuse(http.StatusBadRequest, "word: up to 64 printable characters, only with decision run", nil)
		return
	case why != "":
		refuse(http.StatusForbidden, why, nil)
		return
	}
	dupKey := "gate\x00" + req.Nonce
	if s.towerDup(req.Elevation, dupKey) {
		refuse(http.StatusTooManyRequests, "duplicate tap: this gate was just answered", nil)
		return
	}
	s.towerActMu.Lock()
	defer s.towerActMu.Unlock()
	args := []string{"gate", "--elevation", req.Elevation, "--device", dt.ID, "--nonce", req.Nonce, "--decision", req.Decision}
	if req.Word != "" {
		args = append(args, "--word="+req.Word) // = form: a word starting with '-' stays a value
	}
	res := s.runnerExec(args...)
	if res.rc != 0 || res.m["ok"] != true {
		s.towerUndup(req.Elevation, dupKey)
		entry["rc"] = res.rc
		if res.rc == 0 {
			res.rc = -1
		}
		res.fail(refuse, "gate", req.Word)
		return
	}
	entry["allowed"], entry["step"] = true, res.m["step"]
	s.towerLog(entry)
	s.towerM.invalidate()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "decision": res.m["decision"], "step": res.m["step"]})
}

// POST /api/rc/tower/runner/end {elevation} — the phone's "End runner mode". Gated by authAct.
func (s *server) handleRunnerEnd(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "runner/end", "allowed": false, "login": tsLogin(r)}
	refuse := s.runnerRefuser(w, entry)
	dt, why := s.runnerActor(r)
	entry["device"] = dt.Name
	var req struct {
		Elevation string `json:"elevation"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "bad json", nil)
		return
	}
	entry["elevation"] = oneLineGo(req.Elevation, 300)
	if !runnerElevOK(req.Elevation) {
		refuse(http.StatusBadRequest, "need elevation (id of [A-Za-z0-9._:-], up to 256)", nil)
		return
	}
	if why != "" {
		refuse(http.StatusForbidden, why, nil)
		return
	}
	s.towerActMu.Lock()
	defer s.towerActMu.Unlock()
	res := s.runnerExec("end", "--elevation", req.Elevation, "--device", dt.ID, "--reason", "operator")
	if res.rc != 0 {
		entry["rc"] = res.rc
		res.fail(refuse, "end", "")
		return
	}
	entry["allowed"] = true
	s.towerLog(entry)
	s.towerM.invalidate()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
