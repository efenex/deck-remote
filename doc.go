package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Doc viewer read endpoint: GET /api/rc/doc?path=<abs|~/|relative>&session=<id>.
// Serves plain-text documents from an allowlist of roots so the PWA can render
// a markdown file an agent mentioned. The surface is deliberately narrow: roots
// (--doc-roots), a fixed extension set, a size cap, UTF-8 only, and a hard
// denylist that applies even inside the roots. File content is never logged.

const (
	docMaxBytes     = 1 << 20
	defaultDocRoots = "~/shared-work/git,~/src/agent-deck-rebase"
)

var docExts = map[string]bool{".md": true, ".markdown": true, ".txt": true, ".log": true}

// Denied directory components (any depth, resolved path) and basename globs.
var (
	docDenyDirs  = []string{".git", "secrets", "vault"}
	docDenyGlobs = []string{".env*", "*.key", "*.pem", "*.crt", "*.p12", "id_*", "state.db*", "*.sqlite*"}
)

// docError carries the HTTP status for a refusal; msg is safe to return to the
// client (it never contains file content).
type docError struct {
	status int
	msg    string
}

func (e *docError) Error() string { return e.msg }

func docErr(status int, msg string) *docError { return &docError{status, msg} }

type docRoot struct{ lex, real string }

// docRoots parses --doc-roots (comma separated, ~/ expanded against home). Each
// root is kept both as written and symlink-resolved: a root that is itself a
// symlink must still accept files reached through either spelling. Roots that
// do not exist are dropped.
func (s *server) docRootList() []docRoot {
	spec := s.cfg.docRoots
	if strings.TrimSpace(spec) == "" {
		spec = defaultDocRoots
	}
	var out []docRoot
	for _, p := range strings.Split(spec, ",") {
		p = expandHome(strings.TrimSpace(p), s.cfg.homeDir)
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		out = append(out, docRoot{lex: p, real: real})
	}
	return out
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// within reports whether p is root or beneath it (both clean, absolute).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func mustRel(base, p string) string {
	rel, _ := filepath.Rel(base, p)
	return rel
}

// docDenied applies the denylist to every component of p (case-insensitive, as
// the macOS filesystem is). p is the path relative to its root.
func docDenied(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, c := range parts {
		lc := strings.ToLower(c)
		for _, d := range docDenyDirs {
			if lc == d {
				return "path is in a denied directory (" + d + ")"
			}
		}
		if i == len(parts)-1 {
			for _, g := range docDenyGlobs {
				if ok, _ := path.Match(g, lc); ok {
					return "file name is denied (" + g + ")"
				}
			}
		}
	}
	return ""
}

func docExtOK(name string) bool { return docExts[strings.ToLower(filepath.Ext(name))] }

type docResult struct {
	Path    string `json:"path"` // resolved absolute path
	Root    string `json:"root"`
	Size    int64  `json:"size"`
	MTime   string `json:"mtime"`
	Content string `json:"content"`
}

// resolveDoc maps a requested path to a readable file. base is the directory
// relative paths resolve against ("" = relative paths refused). It returns the
// symlink-resolved path and the root that contains it. Order matters: the cheap
// lexical root check comes first so out-of-root probes cannot learn whether a
// file exists; the denylist and extension are then checked on the RESOLVED
// path, so a symlink cannot launder a denied or non-document target.
func resolveDoc(roots []docRoot, home, req, base string) (real string, root docRoot, err *docError) {
	if req == "" || strings.ContainsRune(req, 0) {
		return "", docRoot{}, docErr(400, "path required")
	}
	p := expandHome(req, home)
	if !filepath.IsAbs(p) {
		if base == "" {
			return "", docRoot{}, docErr(400, "relative path needs a session with a known project directory")
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)

	lexRel, lexOK := "", false
	for _, r := range roots {
		for _, b := range []string{r.lex, r.real} {
			if within(b, p) {
				lexRel, lexOK = mustRel(b, p), true
			}
		}
	}
	if !lexOK {
		return "", docRoot{}, docErr(403, "path is outside the allowed roots")
	}
	// Denylist on the requested spelling too, so a denied name is a 403 whether
	// or not it exists (no existence oracle).
	if why := docDenied(lexRel); why != "" {
		return "", docRoot{}, docErr(403, why)
	}
	if !docExtOK(p) {
		return "", docRoot{}, docErr(415, "only .md, .markdown, .txt and .log files can be read")
	}

	real, e := filepath.EvalSymlinks(p)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return "", docRoot{}, docErr(404, "not found")
		}
		return "", docRoot{}, docErr(404, "not readable")
	}
	for _, r := range roots {
		if within(r.real, real) {
			root = r
			break
		}
	}
	if root.real == "" {
		return "", docRoot{}, docErr(403, "path resolves outside the allowed roots")
	}
	rel, _ := filepath.Rel(root.real, real)
	if why := docDenied(rel); why != "" {
		return "", docRoot{}, docErr(403, why)
	}
	if !docExtOK(real) {
		return "", docRoot{}, docErr(415, "only .md, .markdown, .txt and .log files can be read")
	}
	return real, root, nil
}

// readDoc opens an already-resolved path without following a final symlink or
// blocking on a FIFO, then enforces regular-file, size and UTF-8.
func readDoc(real string) (*docResult, *docError) {
	f, e := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, docErr(404, "not readable")
	}
	defer f.Close()
	fi, e := f.Stat()
	if e != nil || !fi.Mode().IsRegular() {
		return nil, docErr(415, "not a regular file")
	}
	if fi.Size() > docMaxBytes {
		return nil, docErr(413, "file is larger than 1 MB")
	}
	b, e := io.ReadAll(io.LimitReader(f, docMaxBytes+1))
	if e != nil {
		return nil, docErr(404, "not readable")
	}
	if len(b) > docMaxBytes {
		return nil, docErr(413, "file is larger than 1 MB")
	}
	if !utf8.Valid(b) {
		return nil, docErr(415, "file is not valid UTF-8")
	}
	return &docResult{Size: int64(len(b)), MTime: fi.ModTime().UTC().Format(time.RFC3339), Content: string(b)}, nil
}

// docLog appends one JSON line per read attempt (path, device, login, outcome,
// byte count). Never carries tokens or file content.
func (s *server) docLog(fields map[string]any) {
	fields["ts"] = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(fields)
	if err != nil {
		return
	}
	p := filepath.Join(stateDir(s.cfg.homeDir), "deck-remote-doc-reads.log")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("doc: read log: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// GET /api/rc/doc?path=&session= — one document, JSON {path,root,size,mtime,content}.
// Status: 400 bad request, 403 outside roots/denylisted, 404 missing, 413 over
// 1 MB, 415 wrong extension / not UTF-8 / not a regular file.
func (s *server) handleDoc(w http.ResponseWriter, r *http.Request) {
	req := r.URL.Query().Get("path")
	sid := r.URL.Query().Get("session")
	device := ""
	if dt, ok := s.tokens.find(bearerHeaderOnly(r)); ok {
		device = dt.Name
	}
	logf := func(status int, real, reason string, n int) {
		if real == "" {
			real = req
		}
		s.docLog(map[string]any{"action": "doc", "allowed": status == 200, "status": status, "path": real, "device": device, "login": tsLogin(r), "reason": reason, "bytes": n})
	}
	fail := func(e *docError, real string) {
		logf(e.status, real, e.msg, 0)
		httpError(w, e.status, e.msg)
	}

	base := ""
	if !filepath.IsAbs(expandHome(req, s.cfg.homeDir)) {
		if sid != "" {
			ctx, cancel := cliCtx(r.Context(), 5*time.Second)
			si, err := s.findSession(ctx, sid)
			cancel()
			if err == nil {
				base = si.Path
			}
		} else if s.cfg.towerRoot != "" {
			base = s.cfg.towerRoot
		}
	}
	real, root, de := resolveDoc(s.docRootList(), s.cfg.homeDir, req, base)
	if de != nil {
		fail(de, "")
		return
	}
	res, de := readDoc(real)
	if de != nil {
		fail(de, real)
		return
	}
	res.Path, res.Root = real, root.real
	logf(200, real, "", len(res.Content))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, http.StatusOK, res)
}
