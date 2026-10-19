package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const towerFixture = `{"generated_at":"2026-10-18T09:00:00Z","counts":{"waiting":1,"urgent":0,"in_flight":0,"finished_12h":0,"requests_open":0},
"waiting":[{"id":"radiomics-qms-export","urgent":false,"text":"export the QMS library","detail":"full steps here","items":["! ls ~/work"],"session":"sharepoint"}],
"requests":[],"in_flight":[],"finished":[],"deadlines":[],"board":{"sessions":[]}}`

func towerTestServer(t *testing.T, operator string, fixture string) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	ov := filepath.Join(dir, "overview.json")
	if err := os.WriteFile(ov, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_REMOTE_STATE_DIR", dir)
	s := testServer(t, config{token: "shared-secret", homeDir: dir, towerOverview: ov, towerOperator: operator, towerSession: "tower"})
	return s, ov
}

func TestTowerDisabledWithoutPath(t *testing.T) {
	s := testServer(t, config{token: "shared-secret", homeDir: t.TempDir()})
	w := httptest.NewRecorder()
	s.handleTower(w, httptest.NewRequest("GET", "/api/rc/tower", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.authAct(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("must not run") })).ServeHTTP(w, httptest.NewRequest("POST", "/api/rc/tower/reply", nil))
	if w.Code != 404 {
		t.Fatalf("actions on a disabled feature must 404, got %d", w.Code)
	}
}

func TestTowerReadEmbedsOverviewAndTracksMtime(t *testing.T) {
	s, ov := towerTestServer(t, "", towerFixture)
	w := httptest.NewRecorder()
	s.handleTower(w, httptest.NewRequest("GET", "/api/rc/tower", nil))
	var resp struct {
		Enabled  bool           `json:"enabled"`
		Actions  bool           `json:"actions"`
		Overview map[string]any `json:"overview"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || !resp.Enabled || resp.Actions || resp.Overview["generated_at"] != "2026-10-18T09:00:00Z" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// a rewrite with a newer mtime is picked up; same mtime is served from cache
	newer := strings.Replace(towerFixture, "09:00:00Z", "09:05:00Z", 1)
	if err := os.WriteFile(ov, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(ov, future, future)
	doc, _, _, err := s.tower.load()
	if err != nil || doc["generated_at"] != "2026-10-18T09:05:00Z" {
		t.Fatalf("reload after mtime change failed: %v %v", err, doc["generated_at"])
	}
	if _, ok := s.tower.ask("radiomics-qms-export"); !ok {
		t.Fatal("ask lookup by id failed")
	}
	if _, ok := s.tower.ask("nope"); ok {
		t.Fatal("unknown ask must not resolve")
	}
}

func TestTowerActionGate(t *testing.T) {
	s, _ := towerTestServer(t, "ruben@example.com", towerFixture)
	_, readRaw, err := s.tokens.mint("phone-read")
	if err != nil {
		t.Fatal(err)
	}
	_, actRaw, err := s.tokens.mint("phone-act", "act")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.tokens.mint("typo", "admin"); err == nil {
		t.Fatal("unknown capability must be refused at mint")
	}
	cases := []struct {
		name   string
		bearer string
		login  string
		query  string
		want   int
		reason string
	}{
		{"shared token refused", "shared-secret", "ruben@example.com", "", 403, "shared token cannot"},
		{"read-only device refused", readRaw, "ruben@example.com", "", 403, "lacks the act"},
		{"act token without identity", actRaw, "", "", 403, "no Tailscale-User-Login"},
		{"act token wrong identity", actRaw, "mallory@example.com", "", 403, "not the operator"},
		{"act token via ?token= refused", "", "ruben@example.com", "?token=" + actRaw, 403, "device token required"},
		{"act token + operator identity", actRaw, "RUBEN@example.com", "", 200, ""},
		{"operator header from a LAN peer refused", actRaw, "ruben@example.com", "lan", 403, "did not come from loopback"},
		{"LAN peer over self-terminated TLS (pf rdr) refused", actRaw, "ruben@example.com", "lan+tls", 403, "did not come from loopback"},
		{"serve -> https+insecure loopback hop allowed", actRaw, "ruben@example.com", "tls", 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := c.query
			if q == "lan" || q == "tls" || q == "lan+tls" {
				q = ""
			}
			r := httptest.NewRequest("POST", "/api/rc/tower/reply"+q, nil)
			r.RemoteAddr = "127.0.0.1:50000" // tailscale serve's hop
			switch c.query {
			case "lan":
				r.RemoteAddr = "192.168.1.23:50000" // pf rdr keeps the client's source
			case "tls":
				r.TLS = &tls.ConnectionState{} // serve proxying https+insecure to --tls-cert
			case "lan+tls":
				r.RemoteAddr = "192.168.1.23:50000"
				r.TLS = &tls.ConnectionState{}
			}
			if c.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+c.bearer)
			}
			if c.login != "" {
				r.Header.Set("Tailscale-User-Login", c.login)
			}
			w := httptest.NewRecorder()
			s.authAct(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
			if w.Code != c.want || (c.reason != "" && !strings.Contains(w.Body.String(), c.reason)) {
				t.Fatalf("want %d %q, got %d %s", c.want, c.reason, w.Code, w.Body.String())
			}
		})
	}
	// every attempt was logged, none with a token in it
	logb, err := os.ReadFile(filepath.Join(os.Getenv("DECK_REMOTE_STATE_DIR"), "deck-remote-tower-actions.log"))
	if err != nil {
		t.Fatalf("action log missing: %v", err)
	}
	if n := strings.Count(string(logb), "\n"); n != 7 {
		t.Fatalf("expected 7 refused attempts logged, got %d: %s", n, logb)
	}
	if strings.Contains(string(logb), actRaw) || strings.Contains(string(logb), readRaw) || strings.Contains(string(logb), "shared-secret") {
		t.Fatal("action log must never contain a token")
	}
}

func TestTowerOperatorUnsetRefusesEveryone(t *testing.T) {
	s, _ := towerTestServer(t, "", towerFixture)
	_, actRaw, _ := s.tokens.mint("phone-act", "act")
	r := httptest.NewRequest("POST", "/api/rc/tower/reply", nil)
	r.Header.Set("Authorization", "Bearer "+actRaw)
	r.Header.Set("Tailscale-User-Login", "anyone@example.com")
	w := httptest.NewRecorder()
	s.authAct(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(w, r)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "tower-operator not set") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestTowerReplySendsToTowerSession(t *testing.T) {
	bin, argvLog := fakeAdeckSend(t, `{"success":true,"delivery":"queued_socket","session_id":"tower"}`, 0)
	dir := t.TempDir()
	ov := filepath.Join(dir, "overview.json")
	if err := os.WriteFile(ov, []byte(towerFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_REMOTE_STATE_DIR", dir)
	s := testServer(t, config{bin: bin, token: "shared-secret", homeDir: dir, towerOverview: ov, towerOperator: "ruben@example.com", towerSession: "tower"})
	_, actRaw, _ := s.tokens.mint("phone-act", "act")
	events := s.hub.subscribe()
	defer s.hub.unsubscribe(events)

	do := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/rc/tower/reply", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:50000"
		r.Header.Set("Authorization", "Bearer "+actRaw)
		r.Header.Set("Tailscale-User-Login", "ruben@example.com")
		w := httptest.NewRecorder()
		s.auth(s.authAct(http.HandlerFunc(s.handleTowerReply))).ServeHTTP(w, r)
		return w
	}
	if w := do(`{"ask":"unknown","text":"yes"}`); w.Code != 404 {
		t.Fatalf("unknown ask: got %d %s", w.Code, w.Body.String())
	}
	if w := do(`{"ask":"radiomics-qms-export","text":""}`); w.Code != 400 {
		t.Fatalf("empty text: got %d", w.Code)
	}
	w := do(`{"ask":"radiomics-qms-export","text":"done, files are in ~/work/radiomics-qtest/qms-export/"}`)
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"target":"tower"`) {
		t.Fatalf("reply: got %d %s", w.Code, w.Body.String())
	}
	if w := do(`{"ask":"radiomics-qms-export","text":"done, files are in ~/work/radiomics-qtest/qms-export/"}`); w.Code != 409 {
		t.Fatalf("double tap must be refused, got %d", w.Code)
	}
	select {
	case ev := <-events:
		if !strings.Contains(string(ev), `"type":"tower-reply-result"`) || strings.Contains(string(ev), `"error"`) {
			t.Fatalf("unexpected event %s", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no tower-reply-result event")
	}
	argv, _ := os.ReadFile(argvLog)
	line := string(argv)
	for _, want := range []string{"session send tower --message-file - --defer-if-busy --defer-timeout 5m --no-tag --json"} {
		if !strings.Contains(line, want) {
			t.Fatalf("argv %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "done, files") {
		t.Fatal("reply text must travel on stdin, never argv")
	}
}

// A reply that never reached the queue must not block an immediate retry of the
// same text as a "double tap" (ultrareview #10).
func TestTowerReplyRetryAfterQueueFull(t *testing.T) {
	bin, _ := fakeAdeckSend(t, `{"success":true,"delivery":"queued_socket","session_id":"tower"}`, 0)
	dir := t.TempDir()
	ov := filepath.Join(dir, "overview.json")
	if err := os.WriteFile(ov, []byte(towerFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_REMOTE_STATE_DIR", dir)
	s := testServer(t, config{bin: bin, token: "shared-secret", homeDir: dir, towerOverview: ov, towerOperator: "ruben@example.com", towerSession: "tower"})
	_, actRaw, _ := s.tokens.mint("phone-act", "act")
	// An unbuffered channel with no worker: every submit fails as "queue full".
	s.queue.mu.Lock()
	s.queue.q["tower:tower"] = make(chan func())
	s.queue.mu.Unlock()

	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/rc/tower/reply", strings.NewReader(`{"ask":"radiomics-qms-export","text":"yes"}`))
		r.RemoteAddr = "127.0.0.1:50000"
		r.Header.Set("Authorization", "Bearer "+actRaw)
		r.Header.Set("Tailscale-User-Login", "ruben@example.com")
		w := httptest.NewRecorder()
		s.auth(s.authAct(http.HandlerFunc(s.handleTowerReply))).ServeHTTP(w, r)
		return w
	}
	if w := do(); w.Code != 503 {
		t.Fatalf("full queue: got %d %s", w.Code, w.Body.String())
	}
	if w := do(); w.Code != 503 {
		t.Fatalf("retry after a refused submit must be tried again (503), not deduped: got %d %s", w.Code, w.Body.String())
	}
	// Queue drains: the retry goes through, and only then does dedupe apply.
	s.queue.mu.Lock()
	delete(s.queue.q, "tower:tower")
	s.queue.mu.Unlock()
	if w := do(); w.Code != 202 {
		t.Fatalf("retry after queue drained: got %d %s", w.Code, w.Body.String())
	}
	if w := do(); w.Code != 409 {
		t.Fatalf("double tap after a queued reply must be refused, got %d", w.Code)
	}
}

// Folded asks are still open: counts.waiting includes them and they live in
// waiting_folded, so ask() lookups (reply by id) must find them.
const towerFoldedOnlyFixture = `{"generated_at":"2026-10-19T17:00:00Z","counts":{"waiting":2,"waiting_folded":2,"urgent":0,"in_flight":0,"finished_12h":0,"requests_open":0},
"waiting":[],
"waiting_folded":[{"id":"hh-flux-pr","urgent":false,"text":"merge the flux fix","items":[],"folded":{"since":"2026-10-19T16:45:47Z"}},{"id":"other-ask","text":"x","folded":{}}],
"requests":[],"in_flight":[],"finished":[],"deadlines":[],"board":{"sessions":[]}}`

func TestTowerAskFindsFoldedAsks(t *testing.T) {
	s, _ := towerTestServer(t, "", towerFoldedOnlyFixture)
	a, ok := s.tower.ask("hh-flux-pr")
	if !ok || a["text"] != "merge the flux fix" {
		t.Fatalf("folded ask not found: %v %v", a, ok)
	}
	if _, ok := s.tower.ask("nope"); ok {
		t.Fatal("unknown ask must not resolve")
	}
	w := httptest.NewRecorder()
	s.handleTower(w, httptest.NewRequest("GET", "/api/rc/tower", nil))
	if !strings.Contains(w.Body.String(), `"waiting_folded":[{"id":"hh-flux-pr"`) {
		t.Fatalf("overview must pass waiting_folded through: %s", w.Body.String())
	}
}

// --- tower tab parity: model + act ------------------------------------------

const towerStubModel = `{"header":"TOWER 17:35Z","generated_at":"2026-10-19T17:35:00Z","palette":{},"legend":[],"sections":[
{"id":"waiting","title":"WAITING ON YOU","count":"1","urgent":0,"folded":null,"items":[{"kind":"ask","id":"hh-flux-pr","key":"ask:hh-flux-pr","fold_ok":true,"confirm_ok":true,"status":"waiting","blocks":[]}]},
{"id":"requests","title":"WHAT YOU ASKED FOR","count":"2","urgent":0,"folded":null,"items":[
 {"kind":"request","id":"req-open","key":"request:req-open","fold_ok":true,"confirm_ok":true,"status":"waiting"},
 {"kind":"request","id":"req-busy","key":"request:req-busy","fold_ok":true,"confirm_ok":false,"status":"flight"}]},
{"id":"flight","title":"IN FLIGHT","count":"2","urgent":0,"folded":null,"items":[
 {"kind":"task","id":"op-task","key":"task:op-task","fold_ok":false,"confirm_ok":true,"status":"flight"},
 {"kind":"task","id":"agent-task","key":"task:agent-task","fold_ok":false,"confirm_ok":false,"status":"flight"}]},
{"id":"suggested","title":"SUGGESTED","count":"1","urgent":0,"folded":null,"items":[{"kind":"suggest","id":"sg_Next.Thing","key":"suggest:sg_Next.Thing","title":"Do the thing","task":"t-1","fold_ok":false,"confirm_ok":false,"status":"suggest"}]}]}`

// towerActServer: a server whose model comes from a stub script and whose writes are
// captured instead of run. Returns a request builder carrying a valid act credential.
func towerActServer(t *testing.T) (*server, func(body string) *httptest.ResponseRecorder, *[][]string) {
	t.Helper()
	s, _ := towerTestServer(t, "ruben@example.com", towerFixture)
	stub := filepath.Join(t.TempDir(), "stub_model.py")
	if err := os.WriteFile(stub, []byte("import sys\nsys.stdout.write("+jsonStr(towerStubModel)+")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.towerModelScript = stub
	s.cfg.towerRoot = "/tower"
	s.cfg.bin = "agent-deck"
	var mu sync.Mutex
	var ran [][]string
	s.towerExec = func(ctx context.Context, argv []string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, argv)
		return 0, ""
	}
	_, tok, err := s.tokens.mint("phone", "act")
	if err != nil {
		t.Fatal(err)
	}
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/rc/tower/act", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Tailscale-User-Login", "ruben@example.com")
		req.RemoteAddr = "127.0.0.1:5555"
		w := httptest.NewRecorder()
		s.authAct(http.HandlerFunc(s.handleTowerAct)).ServeHTTP(w, req)
		return w
	}
	return s, do, &ran
}

func TestTowerActPickAndDismissUseTUIArgv(t *testing.T) {
	_, do, ran := towerActServer(t)
	if w := do(`{"op":"pick","id":"sg_Next.Thing"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "queued") {
		t.Fatalf("pick: %d %s", w.Code, w.Body.String())
	}
	j := "/tower/scripts/journal.py"
	want := [][]string{
		{"python3", "-I", j, "add", "--kind", "request", "--id", "pick-sg-next-thing", "--to", "open", "--by", "operator",
			"--data", `{"text":"operator picked suggestion sg_Next.Thing: Do the thing","suggestion":"sg_Next.Thing","task":"t-1","from":"tui"}`},
		{"python3", "-I", j, "add", "--kind", "suggest", "--id", "sg_Next.Thing", "--to", "picked", "--by", "operator"},
		{"agent-deck", "session", "send", "--no-wait", "tower", "[tui] picked suggestion sg_Next.Thing"},
	}
	if !reflect.DeepEqual(*ran, want) {
		t.Fatalf("pick argv:\n got %q\nwant %q", *ran, want)
	}
	*ran = nil
	if w := do(`{"op":"dismiss","id":"sg_Next.Thing"}`); w.Code != 200 {
		t.Fatalf("dismiss: %d %s", w.Code, w.Body.String())
	}
	if got := (*ran)[0]; !reflect.DeepEqual(got, []string{"python3", "-I", j, "add", "--kind", "suggest", "--id", "sg_Next.Thing", "--to", "dismissed", "--by", "operator"}) {
		t.Fatalf("dismiss argv: %q", got)
	}
}

func TestTowerActFoldCloseArgvAndValidation(t *testing.T) {
	_, do, ran := towerActServer(t)
	j := "/tower/scripts/journal.py"
	cases := []struct {
		body string
		code int
		argv []string
	}{
		{`{"op":"fold","id":"hh-flux-pr"}`, 200, []string{"python3", "-I", j, "add", "--kind", "fold", "--id", "hh-flux-pr", "--to", "folded", "--by", "operator", "--ref", "operator via overview TUI"}},
		{`{"op":"unfold","id":"section:requests"}`, 200, []string{"python3", "-I", j, "add", "--kind", "fold", "--id", "section:requests", "--to", "unfolded", "--by", "operator", "--ref", "operator via overview TUI"}},
		{`{"op":"close","kind":"ask","id":"hh-flux-pr"}`, 200, []string{"python3", "-I", j, "add", "--kind", "ask", "--id", "hh-flux-pr", "--to", "closed", "--by", "operator", "--data", `{"answer":"closed by operator in TUI"}`}},
		{`{"op":"close","kind":"request","id":"req-open"}`, 200, []string{"python3", "-I", j, "add", "--kind", "request", "--id", "req-open", "--to", "done", "--by", "operator", "--data", `{"result":"confirmed by operator in TUI"}`}},
		{`{"op":"close","kind":"task","id":"op-task"}`, 200, []string{"python3", "-I", j, "add", "--kind", "task", "--id", "op-task", "--to", "done", "--by", "operator", "--data", `{"done_evidence":"closed by operator in TUI"}`}},
		// refusals: nothing may be journaled
		{`{"op":"close","kind":"request","id":"req-busy"}`, 404, nil},
		{`{"op":"close","kind":"task","id":"agent-task"}`, 404, nil},
		{`{"op":"close","kind":"suggest","id":"sg_Next.Thing"}`, 404, nil},
		{`{"op":"fold","id":"op-task"}`, 404, nil},
		{`{"op":"fold","id":"section:nope"}`, 404, nil},
		{`{"op":"pick","id":"not-there"}`, 404, nil},
		{`{"op":"rm -rf","id":"hh-flux-pr"}`, 400, nil},
		{`{"op":"fold","id":""}`, 400, nil},
	}
	for _, c := range cases {
		*ran = nil
		w := do(c.body)
		if w.Code != c.code {
			t.Fatalf("%s: status %d want %d (%s)", c.body, w.Code, c.code, w.Body.String())
		}
		if c.argv == nil && len(*ran) != 0 {
			t.Fatalf("%s: refused request still wrote: %q", c.body, *ran)
		}
		if c.argv != nil && (len(*ran) != 1 || !reflect.DeepEqual((*ran)[0], c.argv)) {
			t.Fatalf("%s: argv\n got %q\nwant %q", c.body, *ran, c.argv)
		}
	}
}

func TestTowerActNeedsActCredential(t *testing.T) {
	s, _, ran := towerActServer(t)
	for name, hdr := range map[string]map[string]string{
		"no token":     {},
		"shared token": {"Authorization": "Bearer shared-secret", "Tailscale-User-Login": "ruben@example.com"},
	} {
		req := httptest.NewRequest("POST", "/api/rc/tower/act", strings.NewReader(`{"op":"dismiss","id":"sg_Next.Thing"}`))
		req.RemoteAddr = "127.0.0.1:5555"
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.authAct(http.HandlerFunc(s.handleTowerAct)).ServeHTTP(w, req)
		if w.Code != 403 || len(*ran) != 0 {
			t.Fatalf("%s: status %d, wrote %q", name, w.Code, *ran)
		}
	}
}

func TestTowerActReportsJournalFailureAndWakeFailureSeparately(t *testing.T) {
	s, do, _ := towerActServer(t)
	s.towerExec = func(ctx context.Context, argv []string) (int, string) {
		if argv[0] == "python3" {
			return 1, "boom"
		}
		return 0, ""
	}
	if w := do(`{"op":"dismiss","id":"sg_Next.Thing"}`); w.Code != 502 || !strings.Contains(w.Body.String(), "rc=1") {
		t.Fatalf("journal failure must surface: %d %s", w.Code, w.Body.String())
	}
	s.towerExec = func(ctx context.Context, argv []string) (int, string) {
		if argv[0] == "agent-deck" {
			return 1, "no such session"
		}
		return 0, ""
	}
	w := do(`{"op":"pick","id":"sg_Next.Thing"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "waking tower failed") {
		t.Fatalf("a failed wake keeps the journaled pick: %d %s", w.Code, w.Body.String())
	}
}

func TestTowerModelEndpointServesStubAndCaches(t *testing.T) {
	s, _, _ := towerActServer(t)
	w := httptest.NewRecorder()
	s.handleTowerModel(w, httptest.NewRequest("GET", "/api/rc/tower/model", nil))
	var resp struct {
		Enabled bool           `json:"enabled"`
		Actions bool           `json:"actions"`
		Model   map[string]any `json:"model"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || !resp.Enabled || !resp.Actions || resp.Model["header"] != "TOWER 17:35Z" {
		t.Fatalf("model: %d %s", w.Code, w.Body.String())
	}
}
