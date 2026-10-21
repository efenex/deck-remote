package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Per-device tokens are an ADDITIVE second factor alongside the legacy shared
// bearer (~/.agent-deck/web-token). The shared token still works everywhere
// (and remains the ONLY token forwarded upstream to agent-deck — device tokens
// never reach the reverse proxy, whose director always injects the shared one).
// Device tokens let you mint a named secret per phone and revoke it without
// rotating the shared token for every device.
//
// Storage decision: we store only a SHA-256 HASH of each token on disk (raw
// 0600 file would also be fine on a single-user tailnet box, but hashing means
// a leaked tokens.json can't be replayed). The raw token is shown EXACTLY once,
// at mint time. Verification is constant-time over the hash bytes.

// deviceToken is one named device credential. Secret is the hash; the raw value
// is never persisted.
type deviceToken struct {
	ID      string `json:"id"`      // short random id (stable handle for revoke)
	Name    string `json:"name"`    // human label, e.g. "ruben-iphone"
	Hash    string `json:"hash"`    // hex SHA-256 of the raw token (never the raw token)
	Created int64  `json:"created"` // unix seconds
	// Caps are capabilities beyond reading: "act" (tower actions, tower.go) and
	// "doc" (document viewer reads, doc.go; "act" implies it). Absent = read-only; existing tokens keep working unchanged.
	Caps []string `json:"caps,omitempty"`
}

func containsFold(xs []string, c string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, c) {
			return true
		}
	}
	return false
}

// capImplies maps a cap to the caps it carries with it (docs/design/run-bang-phase2-review.md
// §5 #4): run:mut runs mutating lines and read-only ones. run:* needing act is a mint rule
// (mint refuses run:* without act), not an implication: hasCap("act") never derives from run:*.
var capImplies = map[string][]string{"run:mut": {"run:ro"}}

// hasCap reports whether the token carries the capability (case-insensitive), directly or
// through capImplies.
func (t deviceToken) hasCap(c string) bool {
	if containsFold(t.Caps, c) {
		return true
	}
	for _, have := range t.Caps {
		if containsFold(capImplies[strings.ToLower(have)], c) {
			return true
		}
	}
	return false
}

// effectiveCaps is Caps plus what they imply, for whoami (the PWA checks caps by name).
func (t deviceToken) effectiveCaps() []string {
	out := append([]string{}, t.Caps...)
	for _, have := range t.Caps {
		for _, c := range capImplies[strings.ToLower(have)] {
			if !containsFold(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// knownCaps is the closed set a mint may grant; anything else is refused so a
// typo never silently mints a read-only token the operator believes can act.
// run:* are the run-bang caps (docs/design/run-bang.md §3.3): which class of an
// ask's signed `! ` lines the phone may run (tower_run.go). Phase 1 runs the ro
// class only; run:mut and run:secret are known so they can be minted ahead of
// their review, but runCapFor grants nothing with them yet.
var knownCaps = map[string]bool{"act": true, "doc": true, "run:ro": true, "run:mut": true, "run:secret": true}

// runCapFor is the cap a device token needs to run a line of class cls from the
// phone, or "" when no cap runs that class in this phase. Phase 1: ro needs
// run:ro; every other class is
// copy-only on the phone (run:mut holds run:ro through capImplies, but mut lines stay
// unrunnable here until the phase-2 gate flip). The runner refuses non-ro again on its side.
func runCapFor(cls string) string {
	if cls == "ro" {
		return "run:ro"
	}
	return ""
}

// tokenStore persists named device tokens to a 0600 JSON file. All methods are
// safe for concurrent use; the auth path (verify) is the hot one.
type tokenStore struct {
	mu     sync.RWMutex
	path   string
	tokens []deviceToken
}

// newTokenStore loads (or initializes) the device-token store at
// dir/deck-remote-tokens.json. A missing/corrupt file yields an empty store
// (never an error that would block startup) — the shared token still works.
func newTokenStore(dir string) *tokenStore {
	ts := &tokenStore{path: filepath.Join(dir, "deck-remote-tokens.json")}
	if b, err := os.ReadFile(ts.path); err == nil {
		var arr []deviceToken
		if json.Unmarshal(b, &arr) == nil {
			for _, t := range arr {
				if t.Hash != "" {
					ts.tokens = append(ts.tokens, t)
				}
			}
		}
	}
	return ts
}

// saveLocked writes the store atomically-ish (write temp, rename) with 0600.
// Caller holds the write lock.
func (ts *tokenStore) saveLocked() error {
	b, err := json.MarshalIndent(ts.tokens, "", "  ")
	if err != nil {
		return err
	}
	tmp := ts.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ts.path)
}

// hashToken returns the hex SHA-256 of a raw token. Pure; used for both store
// and verify so the two can never drift.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// genSecret returns a URL-safe random token (32 bytes of crypto/rand, ~43
// base64url chars). Distinct from the id so the id can be logged but the secret
// never is.
func genSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// genID returns a short random handle for a device token (8 hex chars).
func genID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// verify reports whether raw matches ANY stored device token, in constant time
// per candidate (constant-time compare of the fixed-width hash). An empty raw
// never matches. This is the auth hot path; it takes only a read lock.
func (ts *tokenStore) verify(raw string) bool {
	if raw == "" {
		return false
	}
	want := hashToken(raw)
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	ok := false
	for i := range ts.tokens {
		// Constant-time per-candidate compare; we do NOT early-return on a match
		// so timing doesn't reveal which (or how many) tokens matched.
		if subtle.ConstantTimeCompare([]byte(ts.tokens[i].Hash), []byte(want)) == 1 {
			ok = true
		}
	}
	return ok
}

// find returns the device-token metadata (id/name/created) matching raw, and
// whether it matched. Constant-time per candidate (no early return). Used by
// whoami so a device can see its own label.
func (ts *tokenStore) find(raw string) (deviceToken, bool) {
	if raw == "" {
		return deviceToken{}, false
	}
	want := hashToken(raw)
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	match := deviceToken{}
	found := false
	for i := range ts.tokens {
		if subtle.ConstantTimeCompare([]byte(ts.tokens[i].Hash), []byte(want)) == 1 {
			match = ts.tokens[i]
			found = true
		}
	}
	return match, found
}

// byID returns the device token with the given id (the stable handle; tickets bind to it).
func (ts *tokenStore) byID(id string) (deviceToken, bool) {
	if id == "" {
		return deviceToken{}, false
	}
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	for _, t := range ts.tokens {
		if t.ID == id {
			return t, true
		}
	}
	return deviceToken{}, false
}

// mint creates and persists a new device token with the given name and returns
// the metadata plus the RAW secret (shown exactly once — never stored raw).
func (ts *tokenStore) mint(name string, caps ...string) (deviceToken, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "device"
	}
	var cl []string
	for _, c := range caps {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if !knownCaps[c] {
			return deviceToken{}, "", fmt.Errorf("unknown capability %q", c)
		}
		cl = append(cl, c)
	}
	for _, c := range cl {
		// The run endpoints sit behind authAct, so a run:* cap without act
		// would mint a token that looks able to run and cannot.
		if strings.HasPrefix(c, "run:") && !containsFold(cl, "act") {
			return deviceToken{}, "", fmt.Errorf("capability %q needs act", c)
		}
	}
	raw, err := genSecret()
	if err != nil {
		return deviceToken{}, "", err
	}
	id, err := genID()
	if err != nil {
		return deviceToken{}, "", err
	}
	dt := deviceToken{ID: id, Name: name, Hash: hashToken(raw), Created: time.Now().Unix(), Caps: cl}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.tokens = append(ts.tokens, dt)
	if err := ts.saveLocked(); err != nil {
		// Roll back the in-memory append so a failed write doesn't leave a token
		// that verifies but isn't durable.
		ts.tokens = ts.tokens[:len(ts.tokens)-1]
		return deviceToken{}, "", err
	}
	return dt, raw, nil
}

// revoke removes a device token by id OR name (id wins). Returns whether a
// token was removed.
func (ts *tokenStore) revoke(idOrName string) (bool, error) {
	_, ok, err := ts.revokeToken(idOrName)
	return ok, err
}

// revokeToken is revoke that also returns the removed token (its id keys the passkey cascade).
func (ts *tokenStore) revokeToken(idOrName string) (deviceToken, bool, error) {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return deviceToken{}, false, nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	idx := -1
	for i := range ts.tokens {
		if ts.tokens[i].ID == idOrName {
			idx = i
			break
		}
	}
	if idx < 0 { // fall back to name match
		for i := range ts.tokens {
			if ts.tokens[i].Name == idOrName {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return deviceToken{}, false, nil
	}
	dt := ts.tokens[idx]
	ts.tokens = append(ts.tokens[:idx], ts.tokens[idx+1:]...)
	return dt, true, ts.saveLocked()
}

// list returns metadata (id/name/created) for all device tokens, newest first.
// NEVER includes the secret or its hash.
func (ts *tokenStore) list() []map[string]any {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]map[string]any, 0, len(ts.tokens))
	for _, t := range ts.tokens {
		out = append(out, map[string]any{"id": t.ID, "name": t.Name, "created": t.Created, "caps": t.Caps})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["created"].(int64) > out[j]["created"].(int64)
	})
	return out
}

// --- HTTP handlers ---

// handleWhoami reports the calling token's identity to the PWA settings UI:
// whether it's the shared/admin token (which may administer devices) or a named
// device token. Gated by s.auth (any valid token). Never echoes the secret.
func (s *server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	if subtleConstantEq(tok, s.cfg.token) {
		writeJSON(w, http.StatusOK, map[string]any{"kind": "shared", "admin": true})
		return
	}
	if dt, ok := s.tokens.find(tok); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"kind": "device", "admin": false,
			"id": dt.ID, "name": dt.Name, "created": dt.Created, "caps": dt.effectiveCaps(), "act": dt.hasCap("act"),
		})
		return
	}
	// auth() already vetted the token, so this is unreachable in practice.
	writeJSON(w, http.StatusOK, map[string]any{"kind": "unknown", "admin": false})
}

// --- admin handlers (gated by the SHARED token only, via s.authShared) ---

// handleDevicesList returns device-token metadata (no secrets). Admin-gated.
func (s *server) handleDevicesList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"devices": s.tokens.list()})
}

// handleDeviceMint creates a new named device token and returns the RAW secret
// (once) plus a ready-to-use phone URL. Admin-gated.
func (s *server) handleDeviceMint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string   `json:"name"`
		Caps []string `json:"caps"` // e.g. ["act"] for a phone that may perform tower actions
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	for _, c := range body.Caps {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(c)), "run:") {
			continue
		}
		if why := localClient(r); why != "" {
			s.towerLog(map[string]any{"action": "devices/mint", "allowed": false, "caps": body.Caps, "login": tsLogin(r), "reason": why})
			httpError(w, http.StatusForbidden, "run:* caps are minted from the laptop only: "+why)
			return
		}
		break
	}
	dt, raw, err := s.tokens.mint(body.Name, body.Caps...)
	if err != nil {
		if strings.Contains(err.Error(), "capabilit") {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		httpError(w, http.StatusInternalServerError, "mint failed")
		return
	}
	// Phone URL embeds the token so the device can self-enroll by opening it;
	// the PWA strips the token from the address bar on load (see app.js).
	phoneURL := s.phoneURL(r, raw)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       dt.ID,
		"name":     dt.Name,
		"created":  dt.Created,
		"caps":     dt.Caps,
		"token":    raw,
		"phoneUrl": phoneURL,
	})
}

// localClient reports "" when r comes from a client on this machine itself, the only place a
// run:* cap may be minted (run-bang-phase2-review §1.2, §5 #4): the shared token works from
// anywhere on the tailnet, so authShared alone would let a shared-token holder mint run:mut.
// A loopback peer is not enough: `tailscale serve` relays are loopback peers too. serve stamps
// Tailscale-User-Login (absent for tagged nodes and Funnel) and the X-Forwarded-* headers on
// every relayed request, so any of them marks a relay. A local process can forge their absence,
// which is accepted: that process already runs as the operator on the laptop.
func localClient(r *http.Request) string {
	if ok, _ := viaServeProxy(r); !ok {
		return "request did not come from loopback"
	}
	for _, h := range []string{"Tailscale-User-Login", "Tailscale-User-Name", "Tailscale-Funnel-Request", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if r.Header.Get(h) != "" {
			return "request came through tailscale serve (" + h + ")"
		}
	}
	return ""
}

// handleDeviceRevoke removes a device token by id or name. Admin-gated.
func (s *server) handleDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	key := body.ID
	if key == "" {
		key = body.Name
	}
	dt, removed, err := s.tokens.revokeToken(key)
	if removed {
		// Revoking the token revokes its passkeys (run-bang.md §3.4), even if the token file
		// write failed: the token is gone from memory either way.
		n, perr := s.passkeys.revokeDevice(dt.ID)
		s.towerLog(map[string]any{"action": "devices/revoke", "allowed": true, "device": dt.Name, "passkeys": n})
		if perr != nil {
			err = perr
		}
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	if !removed {
		httpError(w, http.StatusNotFound, "no such device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// phoneURL builds an enrollment URL (origin + ?token=). A configured public base
// (--public-url / DECK_REMOTE_PUBLIC_URL, e.g. the fixed tailscale hostname; also the passkey rpId) is preferred and
// trusted; otherwise it derives the origin from the request's forwarded
// host/proto (tailscale serve sets these), falling back to Host.
func (s *server) phoneURL(r *http.Request, raw string) string {
	if base := strings.TrimRight(s.cfg.publicURL, "/"); base != "" {
		return fmt.Sprintf("%s/?token=%s", base, raw)
	}
	scheme := "https"
	if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	} else if r.TLS == nil && r.Header.Get("X-Forwarded-For") == "" {
		scheme = "http"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return fmt.Sprintf("%s://%s/?token=%s", scheme, host, raw)
}
