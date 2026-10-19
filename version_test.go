package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestVersionEndpointReportsServerStampAndWebCheckout(t *testing.T) {
	repo := t.TempDir()
	web := filepath.Join(repo, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(web, "app.js"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	head := gitIn(t, repo, "rev-parse", "--short=7", "HEAD")

	oldSHA, oldAt, oldDirty := buildSHA, buildAt, buildDirty
	defer func() { buildSHA, buildAt, buildDirty = oldSHA, oldAt, oldDirty }()
	buildSHA, buildAt, buildDirty = head, "2026-10-19T18:00:00Z", "false"

	s := testServer(t, config{token: "t", homeDir: t.TempDir(), webDir: web})
	get := func() versionInfo {
		s.webVer.at = s.webVer.at.AddDate(-1, 0, 0) // expire the cache
		w := httptest.NewRecorder()
		s.handleVersion(w, httptest.NewRequest("GET", "/api/rc/version", nil))
		var v versionInfo
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("version: %d %s", w.Code, w.Body.String())
		}
		return v
	}
	v := get()
	if v.ServerSHA != head || v.BuiltAt != "2026-10-19T18:00:00Z" || v.WebSHA != head || v.WebDirty || !v.Match {
		t.Fatalf("clean checkout, matching build: %+v", v)
	}
	// uncommitted change inside web/ -> dirty
	if err := os.WriteFile(filepath.Join(web, "app.js"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := get(); !v.WebDirty || !v.Match {
		t.Fatalf("modified web file must report web_dirty: %+v", v)
	}
	// a commit after the build -> web_sha moves, server_sha does not: mismatch
	gitIn(t, repo, "commit", "-q", "-am", "next")
	v = get()
	if v.WebSHA == head || v.ServerSHA != head || v.Match || v.WebDirty {
		t.Fatalf("web ahead of the binary must not match: %+v", v)
	}
	// a change OUTSIDE web/ does not make web dirty
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := get(); v.WebDirty {
		t.Fatalf("changes outside web/ are not web_dirty: %+v", v)
	}
}

func TestVersionWebDirNotACheckout(t *testing.T) {
	s := testServer(t, config{token: "t", homeDir: t.TempDir(), webDir: t.TempDir()})
	w := httptest.NewRecorder()
	s.handleVersion(w, httptest.NewRequest("GET", "/api/rc/version", nil))
	var v versionInfo
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || v.WebSHA != "" || !v.Match {
		t.Fatalf("no checkout -> unknown web sha, no alarm: %+v %s", v, w.Body.String())
	}
}

func TestShaMatch(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"abc1234", "abc1234", true}, {"abc1234", "abc1234def", true}, {"abc1234", "def5678", false}, {"", "abc", true}, {"unknown", "abc", true}} {
		if shaMatch(c.a, c.b) != c.want {
			t.Fatalf("shaMatch(%q,%q) != %v", c.a, c.b, c.want)
		}
	}
}
