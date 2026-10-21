package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// run-bang phase 1, deck-remote side: §9 D1 (caps) and D3 (run endpoints) test matrix.

var (
	shaRo    = strings.Repeat("a", 64)
	shaMut   = strings.Repeat("b", 64)
	shaV1    = strings.Repeat("c", 64)
	shaStale = strings.Repeat("d", 64)
)

// verifyFixture is what `run_bang.py verify --ask ask1` prints for the stub runner.
func verifyFixture(run map[string]any) string {
	items := []map[string]any{
		{"index": 0, "sha256": shaRo, "class": "ro", "sig": "ok", "runnable": true, "why": nil, "run": run, "cwd": "/Users/op", "timeout": 120},
		{"index": 1, "sha256": shaMut, "class": "mut", "sig": "ok", "runnable": false, "why": "class:mut", "run": nil, "cwd": "/Users/op", "timeout": 900},
		{"index": 3, "sha256": shaV1, "class": "ro", "sig": "sig-v1", "runnable": false, "why": "sig-v1", "run": nil},
		{"index": 4, "sha256": shaStale, "class": "ro", "sig": "sig-stale-params", "runnable": false, "why": "sig-stale-params", "run": nil},
	}
	return jsonStr(map[string]any{"ask": "ask1", "status": "open", "items": items}) + "\n"
}

type stubRunner struct {
	mu      sync.Mutex
	calls   [][]string
	runOut  string // stdout of `run`
	runRC   int
	logOut  string
	doneRun map[string]any // verify's run state for item 0 (nil = never run)
}

func (st *stubRunner) exec(ctx context.Context, argv []string) (int, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.calls = append(st.calls, argv)
	switch argv[3] {
	case "verify":
		return 0, verifyFixture(st.doneRun)
	case "run":
		return st.runRC, st.runOut
	case "log":
		return 0, st.logOut
	}
	return 1, `{"error":"unexpected"}`
}

func (st *stubRunner) count(cmd string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, a := range st.calls {
		if a[3] == cmd {
			n++
		}
	}
	return n
}

// runServer: a tower-enabled server with a stub runner and tokens with the given cap sets.
func runServer(t *testing.T) (*server, *stubRunner) {
	t.Helper()
	s, _ := towerTestServer(t, "ruben@example.com", towerFixture)
	s.cfg.towerRoot = t.TempDir()
	st := &stubRunner{runOut: `{"id":"ask1:0:aaaaaaaa","class":"ro","log":"state/runs/x.log","pid":42,"timeout":120}` + "\n"}
	s.towerExec = st.exec
	return s, st
}

func mintTok(t *testing.T, s *server, name string, caps ...string) string {
	t.Helper()
	_, tok, err := s.tokens.mint(name, caps...)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func runReq(s *server, tok, method, path, body string, h http.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Tailscale-User-Login", "ruben@example.com")
	req.RemoteAddr = "127.0.0.1:5555"
	w := httptest.NewRecorder()
	s.authAct(h).ServeHTTP(w, req)
	return w
}

func postRun(s *server, tok string, item int, sha string) *httptest.ResponseRecorder {
	return runReq(s, tok, "POST", "/api/rc/tower/run", jsonStr(map[string]any{"ask": "ask1", "item": item, "sha256": sha}), s.handleTowerRun)
}

func actionLog(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("DECK_REMOTE_STATE_DIR"), "deck-remote-tower-actions.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("action log line %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

// D1: the caps are known, run:* needs act, an unknown run cap is refused, and phase 1 maps
// only the ro class to a cap (run:mut / run:secret are reserved, they grant nothing yet).
func TestRunCapsMint(t *testing.T) {
	ts := newTokenStore(t.TempDir())
	for _, c := range []string{"run:ro", "run:mut", "run:secret"} {
		dt, _, err := ts.mint("p-"+c, "act", c)
		if err != nil || !dt.hasCap(c) {
			t.Fatalf("mint act+%s: %v %v", c, dt.Caps, err)
		}
	}
	if _, _, err := ts.mint("p", "act", "run:x"); err == nil {
		t.Fatal("run:x must be refused as unknown")
	}
	if _, _, err := ts.mint("p", "run:ro"); err == nil || !strings.Contains(err.Error(), "needs act") {
		t.Fatalf("run:ro without act must be refused, got %v", err)
	}
	if runCapFor("ro") != "run:ro" {
		t.Fatal("ro needs run:ro")
	}
	for _, c := range []string{"mut", "secret", "destructive", "interactive", "copy", ""} {
		if runCapFor(c) != "" {
			t.Fatalf("phase 1: class %q must have no phone cap", c)
		}
	}
}

// D1: the shared token never runs anything (authAct refuses it before the handler) and whoami
// reports no caps for it.
func TestRunSharedTokenNeverRuns(t *testing.T) {
	s, st := runServer(t)
	w := postRun(s, "shared-secret", 0, shaRo)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "shared token") {
		t.Fatalf("shared token: %d %s", w.Code, w.Body.String())
	}
	if st.count("run") != 0 || st.count("verify") != 0 {
		t.Fatal("the shared token must not reach the runner")
	}
	req := httptest.NewRequest("GET", "/api/rc/devices/whoami", nil)
	req.Header.Set("Authorization", "Bearer shared-secret")
	ww := httptest.NewRecorder()
	s.handleWhoami(ww, req)
	if strings.Contains(ww.Body.String(), "run:") {
		t.Fatalf("shared whoami must carry no run caps: %s", ww.Body.String())
	}
}

// D3: cap matrix × class matrix. Only run:ro (held directly, or through run:mut since phase-2
// #4) on an ro line reaches `run`; mut lines stay unrunnable until the phase-2 gate flip.
func TestRunCapClassMatrix(t *testing.T) {
	cases := []struct {
		caps []string
		item int
		sha  string
		code int
		want string
	}{
		{[]string{"act"}, 0, shaRo, 403, "lacks the run:ro"},
		{[]string{"act", "run:mut"}, 0, shaRo, 202, `"id":"ask1:0:aaaaaaaa"`},
		{[]string{"act", "run:mut"}, 1, shaMut, 403, "needs phase 2"},
		{[]string{"act", "run:secret"}, 0, shaRo, 403, "lacks the run:ro"},
		{[]string{"act", "run:ro"}, 1, shaMut, 403, "needs phase 2"},
		{[]string{"act", "run:ro", "run:mut"}, 1, shaMut, 403, "needs phase 2"},
		{[]string{"act", "run:ro"}, 0, shaRo, 202, `"id":"ask1:0:aaaaaaaa"`},
	}
	for i, c := range cases {
		s, st := runServer(t)
		tok := mintTok(t, s, "phone", c.caps...)
		w := postRun(s, tok, c.item, c.sha)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.want) {
			t.Fatalf("case %d %v item %d: %d %s", i, c.caps, c.item, w.Code, w.Body.String())
		}
		if (st.count("run") == 1) != (c.code == 202) {
			t.Fatalf("case %d: run called %d times", i, st.count("run"))
		}
	}
}

// D3: the run argv is the runner's contract, carries no token, and the env is minimal.
func TestRunArgvAndEnv(t *testing.T) {
	s, st := runServer(t)
	tok := mintTok(t, s, "ruben-iphone", "act", "run:ro")
	if w := postRun(s, tok, 0, shaRo); w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var argv []string
	for _, a := range st.calls {
		if a[3] == "run" {
			argv = a
		}
	}
	want := []string{"python3", "-I", filepath.Join(s.cfg.towerRoot, "scripts", "run_bang.py"), "run", "--ask", "ask1", "--item", "0", "--sha", shaRo,
		"--via", "phone", "--device", "ruben-iphone", "--login", "ruben@example.com"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv\n got %q\nwant %q", argv, want)
	}
	if strings.Contains(strings.Join(argv, " "), tok) {
		t.Fatal("the token must never reach argv")
	}
	t.Setenv("DECK_REMOTE_TOKEN", "x")
	t.Setenv("RUN_BANG_KEY", "/tmp/k")
	t.Setenv("VAULT_TOKEN", "hvs.x")
	env := runBangEnv()
	if len(env) != 3 || !strings.HasPrefix(env[0], "PATH=") || !strings.HasPrefix(env[1], "HOME=") || !strings.HasPrefix(env[2], "LANG=") {
		t.Fatalf("env must be exactly PATH, HOME, LANG: %q", env)
	}
}

// D3: a stale tab (hash mismatch) is 409 and never reaches `run`; the runner's own wrong-sha is 409 too.
func TestRunStaleHash409(t *testing.T) {
	s, st := runServer(t)
	tok := mintTok(t, s, "phone", "act", "run:ro")
	if w := postRun(s, tok, 0, strings.Repeat("e", 64)); w.Code != 409 || !strings.Contains(w.Body.String(), "stale") {
		t.Fatalf("stale: %d %s", w.Code, w.Body.String())
	}
	if st.count("run") != 0 {
		t.Fatal("a stale hash must not reach run")
	}
	st.runRC, st.runOut = 3, `{"id":"ask1:0:aaaaaaaa:refused:x","refused":"wrong-sha"}`+"\n"
	if w := postRun(s, tok, 0, shaRo); w.Code != 409 || !strings.Contains(w.Body.String(), "wrong-sha") {
		t.Fatalf("runner wrong-sha: %d %s", w.Code, w.Body.String())
	}
	if w := postRun(s, tok, 99, shaRo); w.Code != 404 {
		t.Fatalf("no such item: %d", w.Code)
	}
	if w := postRun(s, tok, 0, "ABC"); w.Code != 400 {
		t.Fatalf("bad sha shape: %d", w.Code)
	}
}

// D3: sig-v1 and sig-stale-params refuse with the re-sign hint (closes deckremote-runbang-v2-reasons).
func TestRunSigRefusalsCarryHint(t *testing.T) {
	s, st := runServer(t)
	tok := mintTok(t, s, "phone", "act", "run:ro")
	for _, c := range []struct {
		item int
		sha  string
		why  string
		hint string
	}{{3, shaV1, "sig-v1", "run_bang.py resign --ask ask1"}, {4, shaStale, "sig-stale-params", "cwd or timeout changed after signing"}} {
		w := postRun(s, tok, c.item, c.sha)
		if w.Code != 403 || !strings.Contains(w.Body.String(), c.why) || !strings.Contains(w.Body.String(), c.hint) {
			t.Fatalf("%s: %d %s", c.why, w.Code, w.Body.String())
		}
	}
	// the runner's own refusal hint is passed through
	st.runRC, st.runOut = 3, `{"id":"r","refused":"sig-v1","hint":"re-sign as v2: run_bang.py resign --ask ask1"}`+"\n"
	if w := postRun(s, tok, 0, shaRo); w.Code != 403 || !strings.Contains(w.Body.String(), "resign --ask ask1") {
		t.Fatalf("runner refusal: %d %s", w.Code, w.Body.String())
	}
	w := runReq(s, tok, "GET", "/api/rc/tower/run/verify?ask=ask1", "", s.handleTowerRunVerify)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hint":"re-sign as v2: run_bang.py resign --ask ask1"`) || !strings.Contains(w.Body.String(), `"cwd":"/Users/op"`) {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
}

// D3: a double tap within 10 s runs once; a refused attempt does not hold the guard.
func TestRunDupGuard(t *testing.T) {
	s, st := runServer(t)
	tok := mintTok(t, s, "phone", "act", "run:ro")
	if w := postRun(s, tok, 0, shaRo); w.Code != 202 {
		t.Fatalf("first: %d %s", w.Code, w.Body.String())
	}
	if w := postRun(s, tok, 0, shaRo); w.Code != 429 {
		t.Fatalf("double tap: %d %s", w.Code, w.Body.String())
	}
	if st.count("run") != 1 {
		t.Fatalf("run must be called once, got %d", st.count("run"))
	}
	s2, st2 := runServer(t)
	tok2 := mintTok(t, s2, "phone", "act", "run:ro")
	st2.runRC, st2.runOut = 1, `{"error":"boom"}`+"\n"
	if w := postRun(s2, tok2, 0, shaRo); w.Code != 502 {
		t.Fatalf("runner error: %d", w.Code)
	}
	st2.runRC, st2.runOut = 0, st.runOut
	if w := postRun(s2, tok2, 0, shaRo); w.Code != 202 {
		t.Fatalf("retry after an error must not be a dup: %d %s", w.Code, w.Body.String())
	}
}

// D3: every attempt is exactly one action-log line, allowed or refused, with no secrets.
func TestRunOneActionLogLinePerAttempt(t *testing.T) {
	s, _ := runServer(t)
	ro := mintTok(t, s, "phone", "act", "run:ro")
	act := mintTok(t, s, "old-phone", "act")
	attempts := []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return postRun(s, ro, 0, shaRo) },                   // started
		func() *httptest.ResponseRecorder { return postRun(s, ro, 0, shaRo) },                   // dup
		func() *httptest.ResponseRecorder { return postRun(s, ro, 1, shaMut) },                  // class
		func() *httptest.ResponseRecorder { return postRun(s, ro, 0, strings.Repeat("f", 64)) }, // stale
		func() *httptest.ResponseRecorder { return postRun(s, act, 0, shaRo) },                  // cap
		func() *httptest.ResponseRecorder { return postRun(s, "shared-secret", 0, shaRo) },      // authAct
	}
	for i, f := range attempts {
		f()
		if n := len(actionLog(t)); n != i+1 {
			t.Fatalf("after attempt %d: %d action-log lines", i, n)
		}
	}
	lines := actionLog(t)
	if lines[0]["allowed"] != true || lines[0]["outcome"] != "started" || lines[0]["id"] != "ask1:0:aaaaaaaa" || lines[0]["device"] != "phone" {
		t.Fatalf("started line: %v", lines[0])
	}
	for _, m := range lines[1:] {
		if m["allowed"] != false {
			t.Fatalf("refusal logged as allowed: %v", m)
		}
	}
	b, _ := json.Marshal(lines)
	if strings.Contains(string(b), ro) || strings.Contains(string(b), "shared-secret") {
		t.Fatal("tokens in the action log")
	}
}

// D3: the stream carries the runner's (redacted) log lines and ends with the done event.
func TestRunStreamEndsWithDone(t *testing.T) {
	s, st := runServer(t)
	tok := mintTok(t, s, "phone", "act", "run:ro")
	if err := os.WriteFile(filepath.Join(s.cfg.towerRoot, "journal.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.logOut = " M state/asks.md\ntoken=[redacted]\n"
	st.doneRun = map[string]any{"state": "done", "rc": 0, "id": "ask1:0:aaaaaaaa", "attempt": 1}
	srv := httptest.NewServer(s.authAct(http.HandlerFunc(s.handleTowerRunStream)))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/?ask=ask1&item=0&id=ask1:0:aaaaaaaa", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Tailscale-User-Login", "ruben@example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var evs []string
	sc := bufio.NewScanner(res.Body)
	ev := ""
	for sc.Scan() {
		ln := sc.Text()
		if e, ok := strings.CutPrefix(ln, "event: "); ok {
			ev = e
		} else if d, ok := strings.CutPrefix(ln, "data: "); ok {
			evs = append(evs, ev+" "+d)
		}
	}
	want := []string{`line " M state/asks.md"`, `line "token=[redacted]"`}
	if len(evs) != 3 || evs[0] != want[0] || evs[1] != want[1] || !strings.HasPrefix(evs[2], `done {`) || !strings.Contains(evs[2], `"rc":0`) {
		t.Fatalf("events: %q", evs)
	}
	// a run id that is not this ask/item's is refused
	w := runReq(s, tok, "GET", "/api/rc/tower/run/stream?ask=ask1&item=0&id=other:0:x", "", s.handleTowerRunStream)
	if w.Code != 400 {
		t.Fatalf("foreign run id: %d", w.Code)
	}
}

// D3: unfold goes through `run_bang.py log` (never --raw) and is one action-log line.
func TestRunLogUnfold(t *testing.T) {
	s, st := runServer(t)
	tok := mintTok(t, s, "phone", "act", "run:ro")
	st.logOut = "hello\n"
	w := runReq(s, tok, "GET", "/api/rc/tower/run/log?id=ask1:0:aaaaaaaa", "", s.handleTowerRunLog)
	if w.Code != 200 || w.Body.String() != "hello\n" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	for _, a := range st.calls {
		if strings.Contains(strings.Join(a, " "), "--raw") {
			t.Fatal("the phone never gets --raw")
		}
	}
	if l := actionLog(t); len(l) != 1 || l[0]["action"] != "run-log" {
		t.Fatalf("action log: %v", l)
	}
	act := mintTok(t, s, "old", "act")
	if w := runReq(s, act, "GET", "/api/rc/tower/run/log?id=ask1:0:aaaaaaaa", "", s.handleTowerRunLog); w.Code != 403 {
		t.Fatalf("unfold without run:ro: %d", w.Code)
	}
}
