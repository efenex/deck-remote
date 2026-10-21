package main

// Run-bang phase 1, deck-remote side (docs/design/run-bang.md §3, §4.3, §9 D3).
//
// The phone may run an open ask's signed `! ` line of class ro. deck-remote never executes the
// line itself: the ONLY executor is tower's `python3 -I <root>/scripts/run_bang.py run …`, which
// re-reads the journal, re-verifies the signature, re-classifies and journals running/done. Here
// we gate in front of it:
//
//   POST /api/rc/tower/run {ask, item, sha256}   authAct + run:ro + class/sha check via
//        `run_bang.py verify` → `run_bang.py run … --via phone --device <token name> --login <ts login>`
//   GET  /api/rc/tower/run/verify?ask=            the same dry run, for the confirm sheet (no log line)
//   GET  /api/rc/tower/run/stream?ask=&item=&id=  text/event-stream of the run's log through the
//        runner's redactor (`run_bang.py log`), ending with a `done` event; read with fetch (the
//        Authorization header, never ?token=), so a run's output never rides the shared SSE hub
//        that every read-only token subscribes to
//   GET  /api/rc/tower/run/log?id=                unfold: the whole redacted log (1 MB, runner-capped)
//
// One action-log line per run attempt (allowed or refused) and per unfold. The token and the
// signing key never reach argv or the runner's env.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	runBangPath      = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" // deck-remote's launchd PATH
	runStreamCap     = 64 << 10                                                         // live stream cap (§4.3)
	runStreamPoll    = time.Second
	runStreamMaxWait = 35 * time.Minute // the longest timeout /ask may set (30 min) plus the runner's kill grace
)

// runHints are the re-sign hints for the two refusals the operator fixes by re-signing
// (tower scripts/runbang/sign.py HINTS; "<id>" is the ask id).
// Phase 2 adds the runner's own three (runner.py HINTS). These are the fallback for verify, which
// does not emit a hint per item; a run refusal carries the runner's hint and that one wins.
var runHints = map[string]string{
	"sig-v1":           "re-sign as v2: run_bang.py resign --ask <id>",
	"sig-stale-params": "cwd or timeout changed after signing: re-sign (journal.py add --kind ask ... --sign, or run_bang.py resign)",
	"laptop-only":      "laptop only (phase 2, F8a/F10a): copy the line and run it on the laptop",
	"params-changed":   "the signed cwd/timeout changed since the sheet loaded (re-signed?): reload and confirm again",
	"still-running":    "the previous attempt's process group is still alive: wait for it, or stop it on the laptop",
}

func runHint(reason, ask string) string {
	return strings.ReplaceAll(runHints[reason], "<id>", ask)
}

var runSHARe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// runBangEnv is the whole environment the runner gets: a fixed PATH, HOME and LANG. Nothing of
// deck-remote's own env (DECK_REMOTE_*, tokens, RUN_BANG_KEY, which would redirect the key).
func runBangEnv() []string {
	home, _ := os.UserHomeDir()
	return []string{"PATH=" + runBangPath, "HOME=" + home, "LANG=en_US.UTF-8"}
}

func (s *server) runBangArgv(args ...string) []string {
	return append([]string{"python3", "-I", filepath.Join(s.cfg.towerRootDir(), "scripts", "run_bang.py")}, args...)
}

// runBang execs run_bang.py with the minimal env and returns (exit code, stdout). Not bound to
// the request: `run` detaches its supervisor and returns at once, and a phone that drops the
// connection mid-call must not kill the runner between its checks and its journal write.
func (s *server) runBang(argv []string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), towerExecTimeout)
	defer cancel()
	if s.towerExec != nil {
		return s.towerExec(ctx, argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.cfg.towerRootDir()
	cmd.Env = runBangEnv()
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		return -1, err.Error()
	}
	return 0, string(out)
}

// runItem is one `! ` item of `run_bang.py verify --ask`.
type runItem struct {
	Index    int            `json:"index"`
	SHA256   string         `json:"sha256"`
	Class    string         `json:"class"`
	Sig      string         `json:"sig"`
	Runnable bool           `json:"runnable"`
	Why      string         `json:"why"`
	Run      map[string]any `json:"run"`
	Cwd      string         `json:"cwd,omitempty"`
	Timeout  int            `json:"timeout,omitempty"`
	// Phase 2 (tower runner.verify_ask): the signed exp and kid, and the phone sub-class. PhoneOK
	// nil (an older runner) reads as laptop only.
	PhoneOK    *bool  `json:"phone_ok,omitempty"`
	LaptopOnly any    `json:"laptop_only,omitempty"`
	Exp        int64  `json:"exp,omitempty"`
	Kid        string `json:"kid,omitempty"`
}

type runVerify struct {
	Ask    string    `json:"ask"`
	Status string    `json:"status"`
	AskTS  any       `json:"ask_ts,omitempty"` // proposal time (F11: re-signing cannot make it look fresh)
	SigTS  any       `json:"sig_ts,omitempty"` // last signature time
	Items  []runItem `json:"items"`
}

func (s *server) runVerify(ask string) (*runVerify, error) {
	rc, out := s.runBang(s.runBangArgv("verify", "--ask", ask))
	var v runVerify
	if err := json.Unmarshal([]byte(lastLine(out)), &v); err != nil || rc != 0 {
		return nil, fmt.Errorf("run_bang.py verify rc=%d: %s", rc, oneLineGo(out, 160))
	}
	return &v, nil
}

func (v *runVerify) item(i int) *runItem {
	for k := range v.Items {
		if v.Items[k].Index == i {
			return &v.Items[k]
		}
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// runCapOK: authAct already passed; the device token must also carry run:ro (phase 1).
func (s *server) runCapOK(r *http.Request) (deviceToken, string) {
	dt, ok := s.tokens.find(bearerHeaderOnly(r))
	if !ok {
		return dt, "device token required"
	}
	if !dt.hasCap(runCapFor("ro")) {
		return dt, "device token " + dt.Name + " lacks the run:ro capability"
	}
	return dt, ""
}

type towerRunReq struct {
	Ask       string          `json:"ask"`
	Item      *int            `json:"item"`
	SHA256    string          `json:"sha256"`
	Assertion json.RawMessage `json:"assertion,omitempty"` // WebAuthn assertion for a class that needs one (passkeys.go)
}

// POST /api/rc/tower/run — see the file comment. Gated by authAct.
func (s *server) handleTowerRun(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "run", "login": tsLogin(r), "allowed": false}
	refuse := func(code int, reason string, extra map[string]any) {
		entry["reason"], entry["code"] = reason, code
		s.towerLog(entry)
		body := map[string]any{"error": reason}
		for k, v := range extra {
			body[k] = v
		}
		writeJSON(w, code, body)
	}
	dt, why := s.runCapOK(r)
	entry["device"] = dt.Name
	var req towerRunReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, passkeyBodyMax)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "bad json", nil)
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
		refuse(http.StatusBadRequest, "need ask, item >= 0 and sha256 (64 lowercase hex)", nil)
		return
	}
	if why != "" {
		refuse(http.StatusForbidden, why, nil)
		return
	}
	s.towerActMu.Lock()
	defer s.towerActMu.Unlock()
	v, err := s.runVerify(req.Ask)
	if err != nil {
		refuse(http.StatusBadGateway, err.Error(), nil)
		return
	}
	it := v.item(*req.Item)
	if it == nil {
		refuse(http.StatusNotFound, "no such `! ` item in the open ask", nil)
		return
	}
	entry["class"] = it.Class
	if it.SHA256 != req.SHA256 {
		refuse(http.StatusConflict, "the line changed since this tab loaded (stale hash): refresh", map[string]any{"stale": true})
		return
	}
	if runCapFor(it.Class) == "" {
		refuse(http.StatusForbidden, "class "+it.Class+" is copy-only on the phone (needs phase 2)", nil)
		return
	}
	if !dt.hasCap(runCapFor(it.Class)) {
		refuse(http.StatusForbidden, "device token "+dt.Name+" lacks "+runCapFor(it.Class), nil)
		return
	}
	_, needsPasskey := passkeyClasses[it.Class]
	if needsPasskey && (it.PhoneOK == nil || !*it.PhoneOK) {
		refuse(http.StatusForbidden, "laptop only", map[string]any{"refused": "laptop-only", "hint": runHint("laptop-only", req.Ask)})
		return
	}
	if !it.Runnable {
		refuse(http.StatusForbidden, "runner says not runnable: "+it.Why, map[string]any{"refused": it.Why, "hint": runHint(it.Why, req.Ask)})
		return
	}
	argv := []string{"run", "--ask", req.Ask, "--item", strconv.Itoa(*req.Item), "--sha", req.SHA256,
		"--via", "phone", "--device", dt.Name, "--login", tsLogin(r)}
	if needsPasskey {
		// One Face ID per line (F2 = b): the assertion is checked before the dup mark so a refused
		// one never holds it. The expected cwd/timeout are the ones the challenge bound (what the
		// sheet showed), never re-read from this verify.
		if len(req.Assertion) == 0 {
			refuse(http.StatusForbidden, "class "+it.Class+" needs a passkey assertion (Face ID)", map[string]any{"needsAssertion": true})
			return
		}
		rcpt, code, why := s.verifyRunAssertion(r, dt, req.Ask, *req.Item, req.SHA256, req.Assertion, v)
		if why != "" {
			entry["outcome"] = "assertion-refused"
			refuse(code, why, map[string]any{"assertion": "refused"})
			return
		}
		auth, _ := json.Marshal(map[string]string{"method": rcpt.Method, "cred": rcpt.Cred, "challenge": rcpt.Challenge})
		entry["cred"], entry["challenge"] = rcpt.Cred, rcpt.Challenge
		argv = append(argv, "--expect-cwd", rcpt.Cwd, "--expect-timeout", strconv.Itoa(rcpt.Timeout), "--auth", string(auth))
	}
	dupKey := fmt.Sprintf("run\x00%d\x00%s", *req.Item, req.SHA256)
	if s.towerDup(req.Ask, dupKey) {
		refuse(http.StatusTooManyRequests, "duplicate tap: this line was just started", nil)
		return
	}
	rc, out := s.runBang(s.runBangArgv(argv...))
	var res struct {
		ID      string `json:"id"`
		Class   string `json:"class"`
		Timeout int    `json:"timeout"`
		Refused string `json:"refused"`
		Hint    string `json:"hint"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal([]byte(lastLine(out)), &res)
	entry["id"] = res.ID
	switch {
	case rc == 0 && res.ID != "":
		entry["allowed"], entry["outcome"] = true, "started"
		s.towerLog(entry)
		s.towerM.invalidate()
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "id": res.ID, "class": res.Class, "timeout": res.Timeout})
	case rc == 3:
		s.towerUndup(req.Ask, dupKey)
		code := http.StatusForbidden
		if res.Refused == "wrong-sha" {
			code = http.StatusConflict
		}
		entry["outcome"] = "runner-refused"
		refuse(code, "runner refused: "+res.Refused, map[string]any{"refused": res.Refused, "hint": res.Hint, "id": res.ID})
	default:
		s.towerUndup(req.Ask, dupKey)
		entry["rc"] = rc
		refuse(http.StatusBadGateway, fmt.Sprintf("run_bang.py run rc=%d: %s", rc, oneLineGo(res.Error+" "+out, 160)), nil)
	}
}

// GET /api/rc/tower/run/verify?ask= — the runner's dry run for the confirm sheet (class, sig,
// runnable, why, cwd, timeout, latest run), plus the re-sign hint per item. Gated by authAct.
func (s *server) handleTowerRunVerify(w http.ResponseWriter, r *http.Request) {
	if _, why := s.runCapOK(r); why != "" {
		httpError(w, http.StatusForbidden, why)
		return
	}
	ask := strings.TrimSpace(r.URL.Query().Get("ask"))
	if ask == "" || len(ask) > 200 || strings.HasPrefix(ask, "-") {
		httpError(w, http.StatusBadRequest, "ask required")
		return
	}
	v, err := s.runVerify(ask)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	items := make([]map[string]any, 0, len(v.Items))
	for _, it := range v.Items {
		b, _ := json.Marshal(it)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if h := runHint(it.Why, ask); h != "" {
			m["hint"] = h
		} else if h := runHint(it.Sig, ask); h != "" {
			m["hint"] = h
		}
		// phone_ok / laptop_only are the runner's phone sub-class, passed through as is;
		// phone_cap says whether this phase gives the class a phone cap at all.
		m["phone_cap"] = runCapFor(it.Class) != ""
		_, m["passkey"] = passkeyClasses[it.Class]
		items = append(items, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ask": v.Ask, "status": v.Status, "ask_ts": v.AskTS, "sig_ts": v.SigTS, "items": items})
}

// runIDOK: a run id is "<ask>:<item>:<sha8>[:<attempt>]" (tower runs.py); the stream is only for
// the ask/item the caller names, and the id goes to argv (no shell) so only its shape matters.
func runIDOK(id, ask string, item int) bool {
	if len(id) > 300 || strings.ContainsAny(id, "\x00\n\r") || strings.HasPrefix(ask, "-") {
		return false
	}
	return strings.HasPrefix(id, ask+":"+strconv.Itoa(item)+":")
}

// GET /api/rc/tower/run/stream?ask=&item=&id= — see the file comment. Gated by authAct.
func (s *server) handleTowerRunStream(w http.ResponseWriter, r *http.Request) {
	if _, why := s.runCapOK(r); why != "" {
		httpError(w, http.StatusForbidden, why)
		return
	}
	q := r.URL.Query()
	ask, id := strings.TrimSpace(q.Get("ask")), q.Get("id")
	item, err := strconv.Atoi(q.Get("item"))
	if ask == "" || err != nil || !runIDOK(id, ask, item) {
		httpError(w, http.StatusBadRequest, "need ask, item and that item's run id")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(ev string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
		fl.Flush()
	}
	journal := filepath.Join(s.cfg.towerRootDir(), "journal.jsonl")
	sent, bytes, capped, stamp := 0, 0, false, ""
	// emit sends the log lines not sent yet. While the run is live the last line may still be
	// growing, so it is held back until the run is done.
	emit := func(final bool) {
		rc, out := s.runBang(s.runBangArgv("log", id))
		if rc != 0 {
			return
		}
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if out == "" || out == "\n" {
			lines = nil
		}
		end := len(lines)
		if !final && end > 0 {
			end--
		}
		for ; sent < end; sent++ {
			if capped {
				continue
			}
			if bytes+len(lines[sent])+1 > runStreamCap {
				capped = true
				send("capped", map[string]any{"bytes": runStreamCap, "note": "live stream capped at 64 kB: unfold for the rest"})
				continue
			}
			bytes += len(lines[sent]) + 1
			send("line", lines[sent])
		}
	}
	deadline := time.Now().Add(runStreamMaxWait)
	tick := time.NewTicker(runStreamPoll)
	defer tick.Stop()
	for {
		if st := fileStamp(journal); st != stamp {
			stamp = st
			if v, err := s.runVerify(ask); err == nil {
				if it := v.item(item); it != nil && it.Run != nil && it.Run["id"] == id {
					if state, _ := it.Run["state"].(string); state == "done" || state == "refused" {
						emit(true)
						send("done", it.Run)
						return
					}
				}
			}
		}
		emit(false)
		if time.Now().After(deadline) {
			send("gone", map[string]any{"note": "stopped watching; the run's result lands in the journal"})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// GET /api/rc/tower/run/log?id= — unfold (§4.4): the whole log through the runner's redactor.
// Never --raw from the phone. Gated by authAct; one action-log line per unfold.
func (s *server) handleTowerRunLog(w http.ResponseWriter, r *http.Request) {
	dt, why := s.runCapOK(r)
	id := r.URL.Query().Get("id")
	entry := map[string]any{"action": "run-log", "device": dt.Name, "login": tsLogin(r), "id": id, "allowed": why == ""}
	if why != "" {
		entry["reason"] = why
		s.towerLog(entry)
		httpError(w, http.StatusForbidden, why)
		return
	}
	if id == "" || len(id) > 300 || strings.ContainsAny(id, "\x00\n\r") || strings.HasPrefix(id, "-") {
		entry["allowed"], entry["reason"] = false, "bad id"
		s.towerLog(entry)
		httpError(w, http.StatusBadRequest, "bad run id")
		return
	}
	rc, out := s.runBang(s.runBangArgv("log", id))
	entry["rc"] = rc
	s.towerLog(entry)
	if rc != 0 {
		httpError(w, http.StatusNotFound, "no log for that run")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out))
}
