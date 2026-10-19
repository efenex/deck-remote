package main

// Version check (2026-10-19): the server binary and the web/ directory it serves
// are deployed separately (web/ is served live from a checkout, the binary only
// changes on rebuild), so they can silently drift. GET /api/rc/version reports
// both revisions; the PWA and tower compare them.
//
//	server_sha/built_at  stamped at build time (-ldflags -X, see Makefile)
//	web_sha/web_dirty    HEAD of the checkout containing --web, and whether
//	                     uncommitted changes touch that directory

import (
	"context"
	"net/http"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Set by `make build` via -ldflags "-X main.buildSHA=… -X main.buildAt=… -X main.buildDirty=…".
var (
	buildSHA   = ""
	buildAt    = ""
	buildDirty = ""
)

const webVersionTTL = 30 * time.Second

type versionInfo struct {
	ServerSHA   string `json:"server_sha"`
	ServerDirty bool   `json:"server_dirty"`
	BuiltAt     string `json:"built_at"`
	WebSHA      string `json:"web_sha"`
	WebDirty    bool   `json:"web_dirty"`
	WebDir      string `json:"web_dir"`
	Match       bool   `json:"match"`
}

// serverBuild returns the stamped build identity, falling back to the VCS data
// `go build` embeds when the binary was built without the Makefile.
func serverBuild() (sha string, dirty bool, at string) {
	sha, at, dirty = buildSHA, buildAt, buildDirty == "true"
	if sha != "" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range bi.Settings {
			switch kv.Key {
			case "vcs.revision":
				if len(kv.Value) > 7 {
					sha = kv.Value[:7]
				} else {
					sha = kv.Value
				}
			case "vcs.time":
				if at == "" {
					at = kv.Value
				}
			case "vcs.modified":
				dirty = kv.Value == "true"
			}
		}
	}
	if sha == "" {
		sha = "unknown"
	}
	return
}

type webVersionCache struct {
	mu    sync.Mutex
	at    time.Time
	sha   string
	dirty bool
}

// gitOut runs git in dir; "" on any failure (not a checkout, git missing, timeout).
func gitOut(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// webVersion is HEAD of the checkout holding webDir plus whether webDir has
// uncommitted changes (tracked or untracked), cached for webVersionTTL.
func (s *server) webVersion() (string, bool) {
	c := &s.webVer
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < webVersionTTL {
		return c.sha, c.dirty
	}
	sha, ok := gitOut(s.cfg.webDir, "rev-parse", "--short=7", "HEAD")
	dirty := false
	if ok {
		st, _ := gitOut(s.cfg.webDir, "status", "--porcelain", "--", ".")
		dirty = st != ""
	}
	c.at, c.sha, c.dirty = time.Now(), sha, dirty
	return sha, dirty
}

// shaMatch: both known and one is a prefix of the other (short SHAs may differ in length).
func shaMatch(a, b string) bool {
	if a == "" || b == "" || a == "unknown" || b == "unknown" {
		return true // can't tell: don't alarm
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// GET /api/rc/version
func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	sha, dirty, at := serverBuild()
	wsha, wdirty := s.webVersion()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, versionInfo{
		ServerSHA: sha, ServerDirty: dirty, BuiltAt: at, WebSHA: wsha, WebDirty: wdirty, WebDir: s.cfg.webDir,
		Match: shaMatch(sha, wsha),
	})
}
