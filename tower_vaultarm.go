package main

// Run-bang V-2, deck-remote side of the Vault-signed `power` arm (docs/design/run-bang-vault-sign.md
// §2.4, §3.4, §8 V-2). A sequence step tagged `prog` runs from the phone only under an arm of its
// revision: the operator's ed25519 signature, made with transit-ligna/keys/runbang-prog in Vault,
// over tower's canonical run-bang-arm/v2 string. The phone gets a 5-minute sign-only Vault token
// through Forgejo OIDC (role runbang-signer) inside the PWA, signs once and discards the token;
// deck-remote never sees the token, only the signature (public by nature):
//
//   POST /api/rc/tower/run/arm {ask, seq_rev, exp, sig, version?}   authAct + run:prog + prof:power
//        → `run_bang.py seq arm --ask … --via phone --device <token id> --login … --sig … --exp …`;
//        the runner verifies the signature over the current revision with the pinned public key
//        and journals `armed`, or `refused` (rc 3) and nothing is armed. One action-log line:
//        run/arm (armed) or run/arm-refused.
//   GET  /vault/callback?state=&code=   static page (web/vault-callback.html): hands state/code to
//        the PWA in the same tab (sessionStorage, read once) and goes back to the app. No server
//        logic; the code is useless without the client_nonce the PWA holds.
//
// The arm sheet's data comes from `run/verify` (seq block, arm_payload for this device token's id,
// and "arm": armConfig). The Vault origin is --vault-url; empty = no arming from the phone (and
// nothing added to the CSP).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The Vault objects of design §3.3/§4 (rdesmet/vault, applied by the operator 2026-10-22/23).
const (
	vaultOIDCMount = "forgejo-oidc"
	vaultOIDCRole  = "runbang-signer"
	vaultTransit   = "transit-ligna"
	vaultArmKey    = "runbang-prog"
	vaultCallback  = "/vault/callback"
)

var (
	armRevRe = regexp.MustCompile(`^[0-9a-f]{16}$`)
	// tower varm.SIG_RE: vault:v<N>:<base64 of 64 bytes>.
	armSigRe = regexp.MustCompile(`^vault:v([1-9][0-9]{0,5}):[A-Za-z0-9+/]{86}==$`)
)

// vaultOrigin is --vault-url reduced to its origin, or "" when unset or not an https origin with
// no path, query or credentials (then arming is off and the CSP names no Vault).
func vaultOrigin(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || !cspHostRe.MatchString(u.Host) {
		return ""
	}
	return "https://" + u.Host
}

// armRedirect is the OIDC redirect_uri the runbang-signer role allows: the --public-url origin's
// /vault/callback (passkeys.go uses the same origin as the rpId).
func (s *server) armRedirect() string {
	raw := strings.TrimRight(strings.TrimSpace(s.cfg.publicURL), "/")
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && u.Hostname() == "localhost")) {
		return ""
	}
	return u.Scheme + "://" + u.Host + vaultCallback
}

// armConfig is what the arm sheet needs to run the Vault flow, and whether this token may arm.
// Nothing in it is secret.
func (s *server) armConfig(dt deviceToken) map[string]any {
	out := map[string]any{"enabled": false, "can": dt.hasCap("run:prog") && dt.hasCap("prof:power")}
	vault, redirect := vaultOrigin(s.cfg.vaultURL), s.armRedirect()
	switch {
	case vault == "":
		out["why"] = "arming from the phone is off: --vault-url (DECK_REMOTE_VAULT_URL) not set to an https origin"
	case redirect == "":
		out["why"] = "arming from the phone is off: --public-url not set (the OIDC redirect is <public-url>" + vaultCallback + ")"
	default:
		out["enabled"] = true
		out["vault"], out["mount"], out["role"], out["redirect"] = vault, vaultOIDCMount, vaultOIDCRole, redirect
		out["transit"], out["key"] = vaultTransit, vaultArmKey
	}
	if out["can"] == false {
		out["can_why"] = "device token " + dt.Name + " needs run:prog and prof:power to arm (minted on the laptop)"
	}
	return out
}

type towerArmReq struct {
	Ask     string `json:"ask"`
	SeqRev  string `json:"seq_rev"`
	Exp     int64  `json:"exp"`
	Sig     string `json:"sig"`
	Version string `json:"version,omitempty"`
}

// POST /api/rc/tower/run/arm — see the file comment. Gated by authAct.
func (s *server) handleTowerRunArm(w http.ResponseWriter, r *http.Request) {
	entry := map[string]any{"action": "run/arm-refused", "login": tsLogin(r), "allowed": false}
	refuse := func(code int, reason string, extra map[string]any) {
		entry["reason"], entry["code"] = reason, code
		s.towerLog(entry)
		body := map[string]any{"error": reason}
		for k, v := range extra {
			body[k] = v
		}
		writeJSON(w, code, body)
	}
	dt, ok := s.tokens.find(bearerHeaderOnly(r))
	entry["device"], entry["device_id"] = dt.Name, dt.ID
	var req towerArmReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "bad json", nil)
		return
	}
	req.Ask = strings.TrimSpace(req.Ask)
	entry["ask"], entry["seq_rev"], entry["exp"] = req.Ask, req.SeqRev, req.Exp
	if m := armSigRe.FindStringSubmatch(req.Sig); m != nil {
		entry["version"], entry["sig8"] = m[1], req.Sig[len("vault:v"+m[1]+":"):][:8]
	}
	switch {
	case !ok:
		refuse(http.StatusForbidden, "device token required", nil)
		return
	case !dt.hasCap("run:prog") || !dt.hasCap("prof:power"):
		refuse(http.StatusForbidden, "device token "+dt.Name+" needs run:prog and prof:power to arm", nil)
		return
	}
	if cfg := s.armConfig(dt); cfg["enabled"] != true {
		refuse(http.StatusConflict, cfg["why"].(string), nil)
		return
	}
	m := armSigRe.FindStringSubmatch(req.Sig)
	if req.Ask == "" || len(req.Ask) > 200 || strings.HasPrefix(req.Ask, "-") || !armRevRe.MatchString(req.SeqRev) || req.Exp <= 0 || m == nil ||
		(req.Version != "" && req.Version != m[1]) {
		refuse(http.StatusBadRequest, "need ask, seq_rev (16 hex), exp (unix s) and sig (vault:v<N>:<base64 of 64 bytes>)", nil)
		return
	}
	s.towerActMu.Lock()
	defer s.towerActMu.Unlock()
	// The rev the sheet showed must still be the ask's signed revision: a patch since then moved it
	// and the runner would call the signature tampered. Saying so here is the useful message.
	v, err := s.runVerify(req.Ask, dt.ID)
	if err != nil {
		refuse(http.StatusBadGateway, err.Error(), nil)
		return
	}
	var sq struct {
		Rev string `json:"rev"`
	}
	if len(v.Seq) == 0 || json.Unmarshal(v.Seq, &sq) != nil {
		refuse(http.StatusNotFound, "not a sequence ask: nothing to arm", nil)
		return
	}
	if sq.Rev != req.SeqRev {
		refuse(http.StatusConflict, "the sequence was patched since the sheet loaded (rev "+sq.Rev+"): reload and arm again", map[string]any{"stale": true})
		return
	}
	rc, out := s.runBang(s.runBangArgv("seq", "arm", "--ask", req.Ask, "--via", "phone", "--device", dt.ID, "--login", tsLogin(r),
		"--sig", req.Sig, "--exp", strconv.FormatInt(req.Exp, 10)))
	var res struct {
		ID      string `json:"id"`
		Armed   bool   `json:"armed"`
		SeqRev  string `json:"seq_rev"`
		Exp     int64  `json:"exp"`
		Version string `json:"version"`
		Refused string `json:"refused"`
		Hint    string `json:"hint"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal([]byte(lastLine(out)), &res)
	entry["id"] = res.ID
	switch {
	case rc == 0 && res.Armed && res.ID != "":
		entry["action"], entry["allowed"], entry["outcome"] = "run/arm", true, "armed"
		s.towerLog(entry)
		s.towerM.invalidate()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": res.ID, "seq_rev": res.SeqRev, "exp": res.Exp, "version": res.Version})
	case rc == 3:
		entry["outcome"], entry["refused"] = "runner-refused", res.Refused
		refuse(http.StatusForbidden, "runner refused the arm: "+res.Refused, map[string]any{"refused": res.Refused, "hint": res.Hint, "id": res.ID})
	case rc == 2:
		entry["rc"] = rc
		refuse(http.StatusBadRequest, "run_bang.py seq arm: "+oneLineGo(res.Error, 160), nil)
	default:
		entry["rc"] = rc
		refuse(http.StatusBadGateway, fmt.Sprintf("run_bang.py seq arm rc=%d: %s", rc, oneLineGo(res.Error+" "+out, 160)), nil)
	}
}

// GET /vault/callback — the OIDC redirect target: the static callback page, never cached, and
// sending no Referer (the URL carries the one-time code).
func (s *server) handleVaultCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.ServeFile(w, r, filepath.Join(s.cfg.webDir, "vault-callback.html"))
}
