package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, name := range []string{"a.markdown", "a.txt", "a.LOG"} {
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
	_, _, de := resolveDoc(s.docRootList(), s.cfg.homeDir, "../outside/o.md", root)
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
	for _, n := range []string{"id_rsa.txt", "state.db.log", ".env.txt"} {
		mustWrite(t, filepath.Join(root, n), "x")
		docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, n)}}), 403)
	}
	// denied names are refused whether or not they exist
	docWant(t, docGet(s, url.Values{"path": {filepath.Join(root, ".git", "nope.md")}}), 403)
}

func TestDocRefusesExtensions(t *testing.T) {
	s, root, _ := docTestServer(t)
	for _, n := range []string{"a.go", "a.json", "a", "a.md.exe", "a.html", "a.sh"} {
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
	if w := docGet(s, url.Values{"path": {filepath.Join(root, "dir.md")}}); w.Code != 415 {
		t.Fatalf("directory: status=%d", w.Code)
	}
	docWant(t, docGet(s, url.Values{"path": {"a\x00b.md"}}), 400)
}

func TestDocRelativeResolvesAgainstBase(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustMkdir(t, filepath.Join(root, "proj", "docs"))
	mustWrite(t, filepath.Join(root, "proj", "docs", "a.md"), "rel")
	real, _, de := resolveDoc(s.docRootList(), s.cfg.homeDir, "docs/a.md", filepath.Join(root, "proj"))
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

func TestDocRouteRequiresAuth(t *testing.T) {
	s, root, _ := docTestServer(t)
	mustWrite(t, filepath.Join(root, "a.md"), "x")
	h := s.auth(http.HandlerFunc(s.handleDoc))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/rc/doc?path="+url.QueryEscape(filepath.Join(root, "a.md")), nil))
	if w.Code != 401 {
		t.Fatalf("status=%d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/rc/doc?path="+url.QueryEscape(filepath.Join(root, "a.md")), nil)
	r.Header.Set("Authorization", "Bearer shared-secret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
}
