package main

import (
	"context"
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
	"unicode"
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
	defaultDocExts  = ".md,.markdown,.txt"
	docLogPathMax   = 512
)

// A name denylist cannot be the control on a root this large (hundreds of
// thousands of files); it is a best-effort cut. The real bounds are the roots,
// the extension set, and the auth on the route (authDoc). Matching is on
// fold()ed names so APFS case/Unicode folding (ſ == s, K == k) cannot slip
// past, and is repeated on the kernel's spelling of the opened file.
var (
	// directory components, any depth (path.Match patterns)
	docDenyDirs = []string{".git", "secrets", "secret", "vault*", ".ssh", ".gnupg", ".aws", ".kube", ".docker", ".config", "node_modules"}
	// basenames (path.Match patterns)
	docDenyGlobs = []string{
		".env*", ".envrc*", "*.key", "*.pem", "*.crt", "*.p12", "*.pfx", "*.jks", "*.age", "*.sops*", "*.gpg", "*.asc",
		"id_*", "state.db*", "*.sqlite*", "*.db",
		"*secret*", "*credential*", "*token*", "*passw*", "*apikey*", "*api-key*", "*api_key*",
		"kubeconfig*", "*.kube*", ".netrc*", ".npmrc*", ".pypirc*", "known_hosts*", "authorized_keys*",
	}
)

// fold maps every rune to the smallest member of its Unicode simple-folding
// orbit, so two names the filesystem treats as equal fold to the same string.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		min := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < min {
				min = f
			}
		}
		return min
	}, s)
}

// parseDocExts normalises a comma-separated extension list (".md,txt").
func parseDocExts(spec string) map[string]bool {
	if strings.TrimSpace(spec) == "" {
		spec = defaultDocExts
	}
	out := map[string]bool{}
	for _, e := range strings.Split(spec, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out[e] = true
	}
	return out
}

// docCfg is the per-request policy: allowed roots and extensions.
type docCfg struct {
	roots []docRoot
	exts  map[string]bool
}

func (s *server) docConfig() docCfg {
	return docCfg{roots: s.docRootList(), exts: parseDocExts(s.cfg.docExts)}
}

func (c docCfg) extOK(name string) bool { return c.exts[strings.ToLower(filepath.Ext(name))] }

const docExtMsg = "only the configured document extensions (default .md, .markdown, .txt) can be read"

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

// docDenied applies the denylist to every component of rel (the path relative
// to its root), after folding.
func docDenied(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, c := range parts {
		fc := fold(c)
		for _, d := range docDenyDirs {
			if ok, _ := path.Match(fold(d), fc); ok {
				return "path is in a denied directory (" + d + ")"
			}
		}
		if i == len(parts)-1 {
			for _, g := range docDenyGlobs {
				if ok, _ := path.Match(fold(g), fc); ok {
					return "file name is denied (" + g + ")"
				}
			}
		}
	}
	return ""
}

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
func resolveDoc(cfg docCfg, home, req, base string) (real string, root docRoot, err *docError) {
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
	for _, r := range cfg.roots {
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
	if !cfg.extOK(p) {
		return "", docRoot{}, docErr(415, docExtMsg)
	}

	real, e := filepath.EvalSymlinks(p)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return "", docRoot{}, docErr(404, "not found")
		}
		return "", docRoot{}, docErr(404, "not readable")
	}
	for _, r := range cfg.roots {
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
	if !cfg.extOK(real) {
		return "", docRoot{}, docErr(415, docExtMsg)
	}
	return real, root, nil
}

// readDoc is the authority; resolveDoc is only the cheap pre-check. It opens
// with O_NOFOLLOW_ANY (a directory swapped for a symlink after resolveDoc fails
// with ELOOP instead of being followed), then re-applies root, denylist and
// extension policy to the kernel's own spelling of the opened file, and refuses
// anything that is not a single-link regular file within the size cap.
func readDoc(real string, root docRoot, cfg docCfg) (*docResult, *docError) {
	f, e := openDocFile(real)
	if e != nil {
		return nil, docErr(404, "not readable")
	}
	defer f.Close()
	canon, e := fdPath(f)
	if e != nil {
		return nil, docErr(404, "not readable")
	}
	if !within(root.real, canon) {
		return nil, docErr(403, "path resolves outside the allowed roots")
	}
	if why := docDenied(mustRel(root.real, canon)); why != "" {
		return nil, docErr(403, why)
	}
	if !cfg.extOK(canon) {
		return nil, docErr(415, docExtMsg)
	}
	fi, e := f.Stat()
	if e != nil || !fi.Mode().IsRegular() {
		return nil, docErr(404, "not found")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && uint64(st.Nlink) > 1 {
		return nil, docErr(403, "hard-linked files cannot be read")
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
	return &docResult{Path: canon, Size: int64(len(b)), MTime: fi.ModTime().UTC().Format(time.RFC3339), Content: string(b)}, nil
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
	var logSession, logBase string
	var logTried int
	logf := func(status int, real, reason string, n int) {
		if real == "" {
			real = req
		}
		f := map[string]any{"action": "doc", "allowed": status == 200, "status": status, "path": real, "req": req, "session": logSession, "base": logBase, "tried": logTried, "device": device, "login": tsLogin(r), "reason": reason, "bytes": n}
		if len(real) > docLogPathMax {
			f["path"], f["truncated"] = strings.ToValidUTF8(real[:docLogPathMax], ""), true
		}
		if len(req) > docLogPathMax {
			f["req"], f["truncated"] = strings.ToValidUTF8(req[:docLogPathMax], ""), true
		}
		s.docLog(f)
	}
	fail := func(e *docError, real string) {
		logf(e.status, real, e.msg, 0)
		httpError(w, e.status, e.msg)
	}

	cfg := s.docConfig()
	bases, firm := []string{""}, 0
	if !filepath.IsAbs(expandHome(req, s.cfg.homeDir)) {
		bases, firm = s.docBases(r.Context(), sid, cfg)
	}
	real, root, base, de := resolveDocBases(cfg, s.cfg.homeDir, req, bases, firm)
	logSession, logBase, logTried = sid, base, len(bases)
	if de != nil {
		fail(de, "")
		return
	}
	res, de := readDoc(real, root, cfg)
	if de != nil {
		fail(de, real)
		return
	}
	res.Root = root.real
	logf(200, res.Path, "", len(res.Content))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, http.StatusOK, res)
}

// docBases lists the directories a relative document path may resolve against,
// in order: the project dir of session sid (looked up server-side; an inbox
// entry or the open sheet names the session, the client never supplies a
// directory), tower's checkout (--tower-root, else derived from
// --tower-overview), then the project dir of every other listed session that
// lies inside a doc root. The last tier exists because inbox notifications
// from other projects (gitops-assistant's state/… and docs/…) carry a path
// relative to their own checkout and no session id. Every candidate is only a
// base: resolveDoc still requires the joined path inside the allowed roots and
// clear of the denylist, so a base cannot widen what is readable.
func (s *server) docBases(parent context.Context, sid string, cfg docCfg) (out []string, firm int) {
	seen := map[string]bool{}
	add := func(p string) {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || p == "." || !filepath.IsAbs(p) || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	ctx, cancel := cliCtx(parent, 5*time.Second)
	sessions, err := s.listSessions(ctx)
	cancel()
	if sid != "" && err == nil {
		for _, si := range sessions {
			if si.ID == sid || si.Title == sid {
				add(si.Path)
				break
			}
		}
	}
	add(s.cfg.towerRootDir())
	firm = len(out)
	if err == nil {
		for _, si := range sessions {
			p := filepath.Clean(si.Path)
			for _, r := range cfg.roots {
				if filepath.IsAbs(p) && (within(r.lex, p) || within(r.real, p)) {
					add(p)
					break
				}
			}
		}
	}
	return out, firm
}

// resolveDocBases tries each base in order. The first `firm` bases (the named
// session, tower) win on first hit; past them, a request that resolves under
// several (the same docs/ path in every worktree) takes the most recently
// modified file. The first base's error is reported when none resolves. base is
// the one used ("" for absolute requests or when nothing resolved).
func resolveDocBases(cfg docCfg, home, req string, bases []string, firm int) (real string, root docRoot, base string, de *docError) {
	if len(bases) == 0 {
		bases = []string{""}
	}
	var first *docError
	var best time.Time
	for i, b := range bases {
		r, rt, e := resolveDoc(cfg, home, req, b)
		if e != nil {
			if first == nil {
				first = e
			}
			continue
		}
		if i < firm {
			return r, rt, b, nil
		}
		var mt time.Time
		if fi, err := os.Stat(r); err == nil {
			mt = fi.ModTime()
		}
		if real == "" || mt.After(best) {
			real, root, base, best = r, rt, b, mt
		}
	}
	if real == "" {
		return "", docRoot{}, "", first
	}
	return real, root, base, nil
}

// docReadOK decides whether r may read documents. The bar mirrors tower
// actions minus the act cap: a header-only bearer (never ?token=, which lands
// in proxy and referrer logs), a DEVICE token carrying the "doc" (or "act")
// capability — the shared token is refused — and, when --tower-operator is set,
// a loopback peer (the tailscale serve hop) whose Tailscale-User-Login is that
// operator. The last clause disables the viewer over the pf LAN fallback, where
// the login header is client-controlled. Returns the device name and a refusal
// reason ("" = allowed); status distinguishes an unknown token (401) from a
// known-but-unqualified one (403).
func (s *server) docReadOK(r *http.Request) (device, reason string, status int) {
	tok := bearerHeaderOnly(r)
	dt, ok := s.tokens.find(tok)
	if !ok {
		if subtleConstantEq(tok, s.cfg.token) {
			return "", "the shared token cannot read documents; mint a device token with caps [doc]", http.StatusForbidden
		}
		return "", "unauthorized", http.StatusUnauthorized
	}
	if !dt.hasCap("doc") && !dt.hasCap("act") {
		return dt.Name, "device token " + dt.Name + " lacks the doc capability", http.StatusForbidden
	}
	if s.cfg.towerOperator != "" {
		if ok, why := viaServeProxy(r); !ok {
			return dt.Name, why, http.StatusForbidden
		}
		if !strings.EqualFold(tsLogin(r), s.cfg.towerOperator) {
			return dt.Name, "Tailscale identity is not the operator", http.StatusForbidden
		}
	}
	return dt.Name, "", 0
}

// authDoc gates the doc route (replaces s.auth for it). Refusals are logged.
func (s *server) authDoc(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		device, reason, status := s.docReadOK(r)
		if reason != "" {
			if status != http.StatusUnauthorized {
				s.docLog(map[string]any{"action": "doc", "allowed": false, "status": status, "device": device, "login": tsLogin(r), "reason": reason})
			}
			httpError(w, status, reason)
			return
		}
		next.ServeHTTP(w, r)
	})
}
