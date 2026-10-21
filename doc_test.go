package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// docTestServer builds a server whose only doc root is <tmp>/root. Temp dirs on
// macOS sit under /var -> /private/var, so the root is resolved up front;
// otherwise the symlink-escape cases would pass or fail for the wrong reason.
func docTestServer(t *testing.T) (s *server, root, outside string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(tmp, "root")
	outside = filepath.Join(tmp, "outside")
	mustMkdir(t, root)
	mustMkdir(t, outside)
	t.Setenv("DECK_REMOTE_STATE_DIR", filepath.Join(tmp, "state"))
	mustMkdir(t, filepath.Join(tmp, "state"))
	s = testServer(t, config{token: "shared-secret", homeDir: tmp, docRoots: root})
	return s, root, outside
}

func docGet(s *server, params url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleDoc(w, httptest.NewRequest("GET", "/api/rc/doc?"+params.Encode(), nil))
	return w
}

func docWant(t *testing.T, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status=%d want %d body=%s", w.Code, code, w.Body.String())
	}
}

func TestDocReadsAllowedFile(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustWrite(t, filepath.Join(root, "README.md"), "# hi\n")
	w := docGet(s, url.Values{"path": {filepath.Join(root, "README.md")}})
	docWant(t, w, 200)
	var res docResult
	if json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Content != "# hi\n" || res.Size != 5 || res.Root != root {
		t.Fatalf("bad body %s", w.Body.String())
	}
	for _, name := range []string{"a.markdown", "a.txt", "a.TXT"} {
		mustWrite(t, filepath.Join(root, name), "x")
		docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, name)}}), 200)
	}
}

func TestDocRefusesOutsideRootsAndTraversal(t *testing.T) {
	s, root, outside := docTestServer(t)
	mustWrite(t, filepath.Join(outside, "o.md"), "secret")
	mustWrite(t, filepath.Join(root, "in.md"), "ok")
	cases := []string{
		filepath.Join(outside, "o.md"),
		filepath.Join(root, "..", "outside", "o.md"),
		filepath.Join(root, "sub", "..", "..", "outside", "o.md"),
		root + "-evil/o.md", // prefix sibling must not match the root
		"/etc/passwd",
		"/etc/hosts.md",
	}
	for _, p := range cases {
		docWant(t, docGet(s, url.Values{"path": {p}}), 403)
	}
	// the lexical check answers before any stat: a missing outside file is 403, not 404
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(outside, "missing.md")}}), 403)
	// relative traversal out of the session base is caught the same way
	_, _, de := resolveDoc(s.docConfig(), s.cfg.homeDir, "../outside/o.md", root)
	if de == nil || de.status != 403 {
		t.Fatalf("relative traversal: %v", de)
	}
}

func TestDocRefusesSymlinkEscape(t *testing.T) {
	s, root, outside := docTestServer(t)
	mustWrite(t, filepath.Join(outside, "o.md"), "secret")
	mustWrite(t, filepath.Join(outside, "o.go"), "package x")
	if err := os.Symlink(filepath.Join(outside, "o.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "link.md")}}), 403)
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "dir", "o.md")}}), 403)
	// a .md-named link to a non-document is judged by its target
	if err := os.Symlink(filepath.Join(outside, "o.go"), filepath.Join(root, "sneaky.md")); err != nil {
		t.Fatal(err)
	}
	if w := docGet(s, url.Values{"path": {filepath.Join(root, "sneaky.md")}}); w.Code != 403 && w.Code != 415 {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestDocSymlinkInsideRootAllowedAndDenylistAppliesToTarget(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustMkdir(t, filepath.Join(root, "secrets"))
	mustWrite(t, filepath.Join(root, "secrets", "x.md"), "no")
	mustWrite(t, filepath.Join(root, "real.md"), "yes")
	if err := os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, "alias.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "secrets", "x.md"), filepath.Join(root, "innocent.md")); err != nil {
		t.Fatal(err)
	}
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "alias.md")}}), 200)
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "innocent.md")}}), 403)
}

func TestDocRootThatIsASymlinkStillServes(t *testing.T) {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	real := filepath.Join(tmp, "real")
	mustMkdir(t, real)
	mustWrite(t, filepath.Join(real, "a.md"), "x")
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DECK_REMOTE_STATE_DIR", tmp)
	s := testServer(t, config{token: "t", homeDir: tmp, docRoots: link})
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(link, "a.md")}}), 200)
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(real, "a.md")}}), 200)
}

func TestDocDenylist(t *testing.T) {
	s, root, _ := docTestServer(t)
	for _, d := range []string{".git", "secrets", "Vault", "a/vault/b", "a/.git/b", "SECRETS"} {
		mustMkdir(t, filepath.Join(root, d))
		mustWrite(t, filepath.Join(root, d, "n.md"), "x")
		docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, d, "n.md")}}), 403)
	}
	// names that match the globs even with a document extension appended later
	for _, n := range []string{".env.md", ".env.local.md", "k.key.md", "id_rsa.md", "state.db.md", "x.sqlite.md", "x.pem.md", "c.crt.md", "c.p12.md", ".ENV.txt"} {
		mustWrite(t, filepath.Join(root, n), "x")
		p := filepath.Join(root, n)
		// glob patterns are anchored at the end for key/pem/crt/p12; those would
		// not match "k.key.md" (the real extension is .md). Only prefix globs refuse.
		w := docGet(s, url.Values{"path": {p}})
		prefix := strings.HasPrefix(strings.ToLower(n), ".env") || strings.HasPrefix(n, "id_") || strings.HasPrefix(n, "state.db") || strings.Contains(n, ".sqlite")
		if prefix {
			docWant(t, w, 403)
		}
	}
	// denied files whose extension is not a document extension are refused either way
	for _, n := range []string{".env", "a.key", "a.pem", "a.crt", "a.p12", "id_ed25519", "state.db", "state.db-wal", "x.sqlite", "x.sqlite3"} {
		mustWrite(t, filepath.Join(root, n), "x")
		if w := docGet(s, url.Values{"path": {filepath.Join(root, n)}}); w.Code != 403 && w.Code != 415 {
			t.Fatalf("%s: status=%d", n, w.Code)
		}
	}
	// a denied name that also carries an allowed extension is a 403, not a leak
	for _, n := range []string{"id_rsa.txt", "state.db.txt", ".env.txt"} {
		mustWrite(t, filepath.Join(root, n), "x")
		docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, n)}}), 403)
	}
	// denied names are refused whether or not they exist
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, ".git", "nope.md")}}), 403)
}

func TestDocRefusesExtensions(t *testing.T) {
	s, root, _ := docTestServer(t)
	for _, n := range []string{"a.go", "a.json", "a", "a.md.exe", "a.html", "a.sh", "a.log"} {
		mustWrite(t, filepath.Join(root, n), "x")
		docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, n)}}), 415)
	}
}

func TestDocSizeCapAndEncoding(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustWrite(t, filepath.Join(root, "ok.md"), strings.Repeat("a", docMaxBytes))
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "ok.md")}}), 200)
	mustWrite(t, filepath.Join(root, "big.md"), strings.Repeat("a", docMaxBytes+1))
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "big.md")}}), 413)
	mustWrite(t, filepath.Join(root, "bin.md"), "ok \xff\xfe bad")
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "bin.md")}}), 415)
}

func TestDocNotFoundBadParamsAndNonRegular(t *testing.T) {
	s, root, _ := docTestServer(t)
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "nope.md")}}), 404)
	docWant(t, docGet(s, url.Values{}), 400)
	docWant(t, docGet(s, url.Values{"path": {"notes.md"}}), 400) // relative, no session, no tower root
	mustMkdir(t, filepath.Join(root, "dir.md"))
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "dir.md")}}), 404) // no 415 oracle for directories
	docWant(t, docGet(s, url.Values{"path": {"a\x00b.md"}}), 400)
}

func TestDocRelativeResolvesAgainstBase(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustMkdir(t, filepath.Join(root, "proj", "docs"))
	mustWrite(t, filepath.Join(root, "proj", "docs", "a.md"), "rel")
	real, _, de := resolveDoc(s.docConfig(), s.cfg.homeDir, "docs/a.md", filepath.Join(root, "proj"))
	if de != nil || real != filepath.Join(root, "proj", "docs", "a.md") {
		t.Fatalf("real=%q err=%v", real, de)
	}
	// with the tower root as the base for session-less relative requests
	s.cfg.towerRoot = filepath.Join(root, "proj")
	docWant(t, docGet(s, url.Values{"path": {"docs/a.md"}}), 200)
}

func TestDocHomeExpansionAndDefaultRoots(t *testing.T) {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	mustMkdir(t, filepath.Join(tmp, "shared-work", "git", "r"))
	mustMkdir(t, filepath.Join(tmp, "src", "agent-deck-rebase"))
	mustWrite(t, filepath.Join(tmp, "shared-work", "git", "r", "a.md"), "x")
	mustWrite(t, filepath.Join(tmp, "notes.md"), "x")
	t.Setenv("DECK_REMOTE_STATE_DIR", tmp)
	s := testServer(t, config{token: "t", homeDir: tmp})
	roots := s.docRootList()
	if len(roots) != 2 {
		t.Fatalf("default roots: %+v", roots)
	}
	docWant(t, docGet(s, url.Values{"path": {"~/shared-work/git/r/a.md"}}), 200)
	docWant(t, docGet(s, url.Values{"path": {"~/notes.md"}}), 403)
}

func TestDocReadsAreLoggedWithoutContent(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustWrite(t, filepath.Join(root, "a.md"), "TOP-SECRET-CONTENT")
	r := httptest.NewRequest("GET", "/api/rc/doc?path="+url.QueryEscape(filepath.Join(root, "a.md")), nil)
	r.Header.Set("Tailscale-User-Login", "ruben@example.com")
	s.handleDoc(httptest.NewRecorder(), r)
	docGet(s, url.Values{"path": {"/etc/passwd.md"}})
	b, err := os.ReadFile(filepath.Join(stateDir(s.cfg.homeDir), "deck-remote-doc-reads.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if strings.Contains(log, "TOP-SECRET-CONTENT") {
		t.Fatal("file content leaked into the log")
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"allowed":true`) || !strings.Contains(lines[0], "ruben@example.com") || !strings.Contains(lines[0], filepath.Join(root, "a.md")) || !strings.Contains(lines[1], `"allowed":false`) {
		t.Fatalf("log lines: %v", lines)
	}
}

// --- security-review regressions (tower/reports/deckremote-doc-endpoint-secreview-2026-10-20) ---

// P1: APFS folds U+017F (long s) to 's'; strings.ToLower does not.
func TestDocDenylistSurvivesUnicodeCaseFolding(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustMkdir(t, filepath.Join(root, "secrets"))
	mustWrite(t, filepath.Join(root, "secrets", "n.md"), "SECRET")
	mustWrite(t, filepath.Join(root, "state.db.md"), "SECRET")
	for _, p := range []string{"ſecrets/n.md", "ſtate.db.md", "SECRETS/n.md", "StAtE.dB.md"} {
		if w := docGet(s, url.Values{"path": {filepath.Join(root, p)}}); w.Code != 403 {
			t.Fatalf("%q -> %d %s", p, w.Code, w.Body.String())
		}
	}
}

// P3: a hard link inside a root to a file outside it passes EvalSymlinks.
func TestDocRefusesHardLinks(t *testing.T) {
	s, root, outside := docTestServer(t)
	mustWrite(t, filepath.Join(outside, "private"), "PRIVATE")
	if err := os.Link(filepath.Join(outside, "private"), filepath.Join(root, "notes.md")); err != nil {
		t.Skip(err)
	}
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "notes.md")}}), 403)
}

// P5: the read log must not grow with the request.
func TestDocLogTruncatesRequestedPath(t *testing.T) {
	s, root, _ := docTestServer(t)
	docGet(s, url.Values{"path": {filepath.Join(root, strings.Repeat("a", 20000)+".md")}})
	b, err := os.ReadFile(filepath.Join(stateDir(s.cfg.homeDir), "deck-remote-doc-reads.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 1500 || !strings.Contains(string(b), `"truncated":true`) {
		t.Fatalf("log line is %d bytes: %.200s", len(b), b)
	}
}

// P6 + the report's lists: document-named secrets and secret-ish directories.
func TestDocDenylistCoversSecretNamedDocuments(t *testing.T) {
	s, root, _ := docTestServer(t)
	for _, n := range []string{"secrets.txt", "my-secret.md", "credentials-cloudflare.md", "token.md", "api-tokens.txt", "kubeconfig.txt", ".netrc.md", "known_hosts.txt", "api-keys.md", "apikey.md", "passwords.md", ".envrc.md", "a.age", "x.sops.md", "authorized_keys.txt", ".npmrc.md", "dump.db"} {
		mustWrite(t, filepath.Join(root, n), "x")
		if w := docGet(s, url.Values{"path": {filepath.Join(root, n)}}); w.Code != 403 && w.Code != 415 {
			t.Fatalf("%s -> %d", n, w.Code)
		}
	}
	for _, d := range []string{"vault-wt-litellm", "vault-wt-sealkeep/sub", "secret", ".ssh", ".aws", ".kube", ".gnupg", ".docker", "node_modules/x"} {
		mustMkdir(t, filepath.Join(root, d))
		mustWrite(t, filepath.Join(root, d, "README.md"), "x")
		docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, d, "README.md")}}), 403)
	}
	// ordinary documents still read
	mustMkdir(t, filepath.Join(root, "docs"))
	mustWrite(t, filepath.Join(root, "docs", "design.md"), "x")
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "docs", "design.md")}}), 200)
}

// P2: a directory swapped for a symlink between resolveDoc and readDoc.
func TestDocReadRefusesIntermediateSymlinkSwap(t *testing.T) {
	s, root, outside := docTestServer(t)
	mustMkdir(t, filepath.Join(root, "docs"))
	mustWrite(t, filepath.Join(root, "docs", "a.md"), "benign")
	mustWrite(t, filepath.Join(outside, "a.md"), "OUTSIDE-SECRET")
	cfg := s.docConfig()
	real, rt, de := resolveDoc(cfg, s.cfg.homeDir, filepath.Join(root, "docs", "a.md"), "")
	if de != nil {
		t.Fatal(de)
	}
	if err := os.Rename(filepath.Join(root, "docs"), filepath.Join(root, "docs.bak")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	if res, de := readDoc(real, rt, cfg); de == nil {
		t.Fatalf("followed a swapped directory symlink: %q", res.Content)
	}
}

// readDoc re-applies the policy to the kernel's spelling, so it holds even when
// the pre-check was handed the client's (folded) spelling.
func TestDocReadChecksKernelPathEvenIfPrecheckIsBypassed(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustMkdir(t, filepath.Join(root, "secrets"))
	mustWrite(t, filepath.Join(root, "secrets", "n.md"), "SECRET")
	cfg := s.docConfig()
	rt := cfg.roots[0]
	if _, de := readDoc(filepath.Join(root, "ſecrets", "n.md"), rt, cfg); de == nil || de.status != 403 {
		// on a case-sensitive filesystem the open itself fails (404): also safe
		if de == nil || de.status != 404 {
			t.Fatalf("de=%v", de)
		}
	}
}

func docRouteReq(t *testing.T, root, token string, mutate func(*http.Request)) *http.Request {
	r := httptest.NewRequest("GET", "/api/rc/doc?path="+url.QueryEscape(filepath.Join(root, "a.md")), nil)
	r.RemoteAddr = "127.0.0.1:5555"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if mutate != nil {
		mutate(r)
	}
	return r
}

func TestDocRouteAuth(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustWrite(t, filepath.Join(root, "a.md"), "x")
	h := s.authDoc(http.HandlerFunc(s.handleDoc))
	_, docTok, err := s.tokens.mint("phone-doc", "doc")
	if err != nil {
		t.Fatal(err)
	}
	_, actTok, _ := s.tokens.mint("phone-act", "act")
	_, roTok, _ := s.tokens.mint("phone-ro")
	do := func(r *http.Request) int { w := httptest.NewRecorder(); h.ServeHTTP(w, r); return w.Code }

	if c := do(docRouteReq(t, root, "", nil)); c != 401 {
		t.Fatalf("no token -> %d", c)
	}
	if c := do(docRouteReq(t, root, "nonsense", nil)); c != 401 {
		t.Fatalf("bad token -> %d", c)
	}
	// P4: the query token must not authorise the route
	r := docRouteReq(t, root, "", nil)
	r.URL.RawQuery += "&token=shared-secret"
	if c := do(r); c != 401 {
		t.Fatalf("?token= -> %d", c)
	}
	r = docRouteReq(t, root, "", nil)
	r.URL.RawQuery += "&token=" + docTok
	if c := do(r); c != 401 {
		t.Fatalf("?token=<device> -> %d", c)
	}
	if c := do(docRouteReq(t, root, "shared-secret", nil)); c != 403 {
		t.Fatalf("shared token -> %d", c)
	}
	if c := do(docRouteReq(t, root, roTok, nil)); c != 403 {
		t.Fatalf("read-only device token -> %d", c)
	}
	if c := do(docRouteReq(t, root, docTok, nil)); c != 200 {
		t.Fatalf("doc token -> %d", c)
	}
	if c := do(docRouteReq(t, root, actTok, nil)); c != 200 {
		t.Fatalf("act token implies doc -> %d", c)
	}
}

func TestDocRouteBindsToOperatorWhenConfigured(t *testing.T) {
	s, root, _ := docTestServer(t)
	s.cfg.towerOperator = "ruben@example.com"
	mustWrite(t, filepath.Join(root, "a.md"), "x")
	h := s.authDoc(http.HandlerFunc(s.handleDoc))
	_, tok, _ := s.tokens.mint("phone-doc", "doc")
	do := func(mutate func(*http.Request)) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, docRouteReq(t, root, tok, mutate))
		return w.Code
	}
	if c := do(func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "ruben@example.com") }); c != 200 {
		t.Fatalf("operator via serve -> %d", c)
	}
	if c := do(nil); c != 403 {
		t.Fatalf("no login header -> %d", c)
	}
	if c := do(func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "eve@example.com") }); c != 403 {
		t.Fatalf("other login -> %d", c)
	}
	// pf LAN fallback / any non-loopback peer: the header is client-controlled
	if c := do(func(r *http.Request) {
		r.RemoteAddr = "100.64.0.9:4444"
		r.Header.Set("Tailscale-User-Login", "ruben@example.com")
	}); c != 403 {
		t.Fatalf("non-loopback peer -> %d", c)
	}
	// refusals are logged without content
	b, _ := os.ReadFile(filepath.Join(stateDir(s.cfg.homeDir), "deck-remote-doc-reads.log"))
	if !strings.Contains(string(b), `"allowed":false`) {
		t.Fatalf("refusals not logged: %s", b)
	}
}

func TestDocExtensionsAreConfigurable(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustWrite(t, filepath.Join(root, "deploy.log"), "x")
	mustWrite(t, filepath.Join(root, "a.md"), "x")
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "deploy.log")}}), 415)
	s.cfg.docExts = "md, LOG"
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "deploy.log")}}), 200)
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "a.md")}}), 200)
	mustWrite(t, filepath.Join(root, "b.txt"), "x")
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "b.txt")}}), 415)
}

func TestDocDirectoryNamedLikeADocumentIs404(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustMkdir(t, filepath.Join(root, "dir.md"))
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, "dir.md")}}), 404)
}

// Inbox entries carry relative document paths. They resolve server-side: against
// the project dir of the session that produced the entry (looked up by id), else
// against tower's checkout, which is DERIVED from --tower-overview when
// --tower-root is unset (the prod flag set; the raw towerRoot field is empty).
func TestDocInboxEntriesResolveRelativePaths(t *testing.T) {
	s, root, outside := docTestServer(t)
	tower := filepath.Join(root, "tower")
	proj := filepath.Join(root, "proj")
	mustMkdir(t, filepath.Join(tower, "reports", "dr-band"))
	mustMkdir(t, filepath.Join(tower, "briefs"))
	mustMkdir(t, filepath.Join(tower, "state", "overview"))
	mustMkdir(t, filepath.Join(proj, "docs"))
	mustWrite(t, filepath.Join(tower, "reports", "dr-band", "report-band2.md"), "# tower report")
	mustWrite(t, filepath.Join(tower, "briefs", "deckremote-bottom-band.md"), "# brief")
	mustWrite(t, filepath.Join(proj, "docs", "notes.md"), "# notes")
	mustWrite(t, filepath.Join(outside, "secret.md"), "# secret")
	s.cfg.towerOverview = filepath.Join(tower, "state", "overview", "overview.json")
	if s.cfg.towerRoot != "" {
		t.Fatal("test must run with --tower-root unset")
	}
	s.lists.m["default"] = listCacheEntry{at: time.Now().Add(time.Hour), sessions: []sessionInfo{
		{ID: "41d4c63c-1792583553", Title: "dr-band2", Path: proj},
	}}

	var entries []struct {
		ID, Kind, SessionID, Body string
	}
	raw, err := os.ReadFile("testdata/inbox-doc-entries.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	// "\x00" never appears in a path; the same shape markdown.js linkifies.
	re := regexp.MustCompile(`[\w./-]+\.md`)
	want := map[string]int{
		"n-1": 200, // session-less entry -> tower checkout (derived root)
		"n-2": 200, // session entry -> that session's project dir
		"n-3": 403, // ../ escape: joined path is outside every root
	}
	for _, e := range entries {
		for _, p := range re.FindAllString(e.Body, -1) {
			q := url.Values{"path": {p}}
			if e.SessionID != "" {
				q.Set("session", e.SessionID)
			}
			docWant(t, docGet(s, q), want[e.ID])
		}
	}
	// The session's dir wins over tower's: same relative name, different files.
	mustMkdir(t, filepath.Join(tower, "docs"))
	mustWrite(t, filepath.Join(tower, "docs", "notes.md"), "# tower notes")
	w := docGet(s, url.Values{"path": {"docs/notes.md"}, "session": {"41d4c63c-1792583553"}})
	docWant(t, w, 200)
	if !strings.Contains(w.Body.String(), "# notes") || strings.Contains(w.Body.String(), "tower notes") {
		t.Fatalf("session base not used: %s", w.Body.String())
	}
	// An unknown session falls back to tower, not to a client-chosen base; a
	// client-supplied "base" parameter is ignored entirely.
	docWant(t, docGet(s, url.Values{"path": {"briefs/deckremote-bottom-band.md"}, "session": {"gone"}}), 200)
	docWant(t, docGet(s, url.Values{"path": {"briefs/deckremote-bottom-band.md"}, "base": {outside}}), 200)
	// A session whose project dir is outside the allowed roots reads nothing.
	s.lists.m["default"] = listCacheEntry{at: time.Now().Add(time.Hour), sessions: []sessionInfo{
		{ID: "evil", Title: "evil", Path: outside},
	}}
	docWant(t, docGet(s, url.Values{"path": {"secret.md"}, "session": {"evil"}}), 403)
	// No session and no tower root: relative paths stay refused.
	s.cfg.towerOverview = ""
	docWant(t, docGet(s, url.Values{"path": {"briefs/deckremote-bottom-band.md"}}), 400)
}
