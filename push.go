package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// defaultPushSubject is the VAPID "sub" contact URI default. RFC 8292 §2.1
// blesses an https: URI as a valid subject, and Apple's web-push gateway
// REJECTS non-routable subjects (e.g. mailto:...@localhost) with 403
// BadJwtToken — which silently dropped every iOS push. An https: URL is
// accepted by Apple AND by FCM/Mozilla. Override via DECK_REMOTE_PUSH_SUBJECT.
const defaultPushSubject = "https://github.com/efenex/deck-remote"

// pushPrefs are the server-honored per-event + quiet-hours preferences. They are
// global (single-user tailnet daemon) and pushed from the PWA via /push/prefs.
//
// Kind/pref mapping (the watcher's server vocabulary differs from the client's):
//   - server Kind "approval" <- client pref Approve   (a permission dialog)
//   - server Kind "reply"    <- client pref Finished  (a settled reply)
//   - server Kind "stall"    <- client pref Stall      (a frozen/hung spinner)
//
// The client's error/idle toggles stay client-only; the server honors
// approve + finished + stall + quiet-hours.
type pushPrefs struct {
	Approve    bool `json:"approve"`    // false suppresses Kind=="approval"
	Finished   bool `json:"finished"`   // false suppresses Kind=="reply"
	Stall      bool `json:"stall"`      // false suppresses Kind=="stall"
	QuietOn    bool `json:"quietOn"`    // gate the quiet-hours window
	QuietStart int  `json:"quietStart"` // minutes-from-midnight, local TZ; -1 = unset
	QuietEnd   int  `json:"quietEnd"`   // minutes-from-midnight, local TZ; -1 = unset
}

func defaultPushPrefs() pushPrefs {
	// Finished defaults OFF: by default only "needs you" events push — a permission
	// dialog (Approve) and a frozen/hung spinner (Stall). Per-completed-turn pings
	// are opt-in via the PWA "Turn completed" toggle.
	return pushPrefs{Approve: true, Finished: false, Stall: true, QuietOn: false, QuietStart: -1, QuietEnd: -1}
}

// pushManager owns deck-remote's OWN Web Push: a VAPID keypair, the set of
// device subscriptions, and the sender. This is deliberately independent of
// agent-deck's push (which fires on agent-deck's unreliable status) — the
// watcher (watcher.go) drives this on reliable events (a reply settling, a real
// permission dialog appearing).
type pushManager struct {
	mu        sync.Mutex
	vapidPub  string
	vapidPriv string
	subject   string
	subs      map[string]*webpush.Subscription // endpoint -> subscription
	subsPath  string
	prefs     pushPrefs
	prefsPath string
	lastFocus time.Time // last time any client reported foreground (suppress window)
}

func newPushManager(dir, subject string) (*pushManager, error) {
	if subject == "" {
		subject = defaultPushSubject
	}
	pm := &pushManager{
		subject:   subject,
		subs:      map[string]*webpush.Subscription{},
		subsPath:  filepath.Join(dir, "deck-remote-subs.json"),
		prefs:     defaultPushPrefs(),
		prefsPath: filepath.Join(dir, "deck-remote-push-prefs.json"),
	}
	if b, err := os.ReadFile(pm.prefsPath); err == nil {
		var p pushPrefs
		if json.Unmarshal(b, &p) == nil {
			pm.prefs = p
		}
	}
	vapidPath := filepath.Join(dir, "deck-remote-vapid.json")
	if b, err := os.ReadFile(vapidPath); err == nil {
		var v struct{ Public, Private string }
		if json.Unmarshal(b, &v) == nil {
			pm.vapidPub, pm.vapidPriv = v.Public, v.Private
		}
	}
	if pm.vapidPub == "" || pm.vapidPriv == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		pm.vapidPub, pm.vapidPriv = pub, priv
		b, _ := json.Marshal(map[string]string{"public": pub, "private": priv})
		_ = os.WriteFile(vapidPath, b, 0o600)
	}
	if b, err := os.ReadFile(pm.subsPath); err == nil {
		var arr []*webpush.Subscription
		if json.Unmarshal(b, &arr) == nil {
			for _, s := range arr {
				if s != nil && s.Endpoint != "" {
					pm.subs[s.Endpoint] = s
				}
			}
		}
	}
	return pm, nil
}

func (pm *pushManager) saveSubsLocked() {
	arr := make([]*webpush.Subscription, 0, len(pm.subs))
	for _, s := range pm.subs {
		arr = append(arr, s)
	}
	b, _ := json.Marshal(arr)
	_ = os.WriteFile(pm.subsPath, b, 0o600)
}

func (pm *pushManager) addSub(s *webpush.Subscription) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.subs[s.Endpoint] = s
	pm.saveSubsLocked()
}

func (pm *pushManager) setFocus(focused bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if focused {
		pm.lastFocus = time.Now()
	} else {
		pm.lastFocus = time.Time{}
	}
}

// suppressed reports whether a client is currently foreground, in which case we
// skip the push (the user is already looking).
func (pm *pushManager) suppressed() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return !pm.lastFocus.IsZero() && time.Since(pm.lastFocus) < 30*time.Second
}

func (pm *pushManager) hasSubs() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return len(pm.subs) > 0
}

func (pm *pushManager) savePrefsLocked() {
	b, _ := json.Marshal(pm.prefs)
	_ = os.WriteFile(pm.prefsPath, b, 0o600)
}

func (pm *pushManager) setPrefs(p pushPrefs) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.prefs = p
	pm.savePrefsLocked()
}

// inQuiet reports whether the minute-of-day `cur` falls inside the configured
// quiet-hours window. It is PURE (takes cur) so it is unit-testable without a
// clock. A start>end window wraps over midnight (e.g. 22:00–07:00 ->
// [start,1440)∪[0,end]). An unset (-1) or zero-width (start==end) window never
// suppresses, so a half-configured pref can't cause a 24h blackout.
func inQuiet(p pushPrefs, cur int) bool {
	if !p.QuietOn || p.QuietStart < 0 || p.QuietEnd < 0 || p.QuietStart == p.QuietEnd {
		return false
	}
	if p.QuietStart < p.QuietEnd { // same-day window
		return cur >= p.QuietStart && cur < p.QuietEnd
	}
	// overnight wrap
	return cur >= p.QuietStart || cur < p.QuietEnd
}

// allow reports whether a payload of the given Kind should be delivered right
// now, honoring quiet-hours and the per-event toggles. Kind "test" (and any
// unknown kind) is always allowed past the per-event filter — only quiet-hours
// could gate it, and the test endpoint bypasses allow() entirely anyway.
func (pm *pushManager) allow(kind string) bool {
	pm.mu.Lock()
	p := pm.prefs
	pm.mu.Unlock()
	now := time.Now()
	if inQuiet(p, now.Hour()*60+now.Minute()) {
		return false
	}
	switch kind {
	case "approval":
		return p.Approve
	case "reply":
		return p.Finished
	case "stall":
		return p.Stall
	default:
		return true
	}
}

// shortEndpoint returns scheme+host of a push endpoint so logs don't leak the
// per-device secret in the endpoint path. Falls back to a truncated string.
func shortEndpoint(ep string) string {
	if u, err := url.Parse(ep); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	if len(ep) > 40 {
		return ep[:40]
	}
	return ep
}

type pushPayload struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"` // "reply" | "approval"
}

// sendResult is the per-subscription outcome of a delivery attempt, surfaced by
// the test endpoint so the user can diagnose gateway rejections.
type sendResult struct {
	Endpoint string `json:"endpoint"`
	Status   int    `json:"status"`
	Err      string `json:"error,omitempty"`
}

// send delivers a payload to every subscription, applying foreground
// suppression and the per-event/quiet-hours filter (allow). The actual delivery
// and per-sub logging/pruning live in sendTo, which the test endpoint calls
// directly to BYPASS both gates (so the user can test while foregrounded).
func (pm *pushManager) send(p pushPayload) {
	if pm.suppressed() {
		return
	}
	if !pm.allow(p.Kind) {
		return
	}
	_ = pm.sendTo(p)
}

// sendTo delivers a payload to every subscription unconditionally (no
// suppression, no prefs), pruning dead subs (404/410) and logging every non-2xx
// status + body and every transport error via the std logger. It returns the
// per-subscription results.
func (pm *pushManager) sendTo(p pushPayload) []sendResult {
	body, _ := json.Marshal(p)
	pm.mu.Lock()
	subs := make([]*webpush.Subscription, 0, len(pm.subs))
	for _, s := range pm.subs {
		subs = append(subs, s)
	}
	pub, priv, subj := pm.vapidPub, pm.vapidPriv, pm.subject
	pm.mu.Unlock()

	results := make([]sendResult, 0, len(subs))
	var dead []string
	for _, s := range subs {
		short := shortEndpoint(s.Endpoint)
		resp, err := webpush.SendNotification(body, s, &webpush.Options{
			Subscriber:      subj,
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             60,
		})
		if err != nil {
			log.Printf("push: transport error endpoint=%s: %v", short, err)
			results = append(results, sendResult{Endpoint: short, Err: err.Error()})
			continue
		}
		code := resp.StatusCode
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		res := sendResult{Endpoint: short, Status: code}
		switch {
		case code/100 == 2:
			// delivered
		case code == http.StatusNotFound || code == http.StatusGone:
			log.Printf("push: subscription gone (%d) endpoint=%s — pruning", code, short)
			dead = append(dead, s.Endpoint)
		default:
			trimmed := strings.TrimSpace(string(rb))
			log.Printf("push: non-2xx %d endpoint=%s body=%q", code, short, trimmed)
			res.Err = trimmed
		}
		results = append(results, res)
	}
	if len(dead) > 0 {
		pm.mu.Lock()
		for _, e := range dead {
			delete(pm.subs, e)
		}
		pm.saveSubsLocked()
		pm.mu.Unlock()
	}
	return results
}

// --- HTTP handlers (deck-remote's own push API; the PWA uses these, not
// agent-deck's, so push fires on reliable events) ---

func (s *server) handlePushConfig(w http.ResponseWriter, r *http.Request) {
	// Return both publicKey (legacy) and vapidPublicKey (the client tolerates
	// either) plus the active subject for diagnostics.
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":        true,
		"publicKey":      s.push.vapidPub,
		"vapidPublicKey": s.push.vapidPub,
		"subject":        s.push.subject,
	})
}

// handlePushTest fires a test payload to all subs and returns the per-sub
// gateway status codes/errors so the user can diagnose delivery. It bypasses
// foreground suppression AND per-event/quiet-hours prefs on purpose (the whole
// point is to test while the app is open).
func (s *server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	p := pushPayload{Title: "deck-remote test", Body: "If you can read this, push works.", Kind: "test"}
	res := s.push.sendTo(p)
	writeJSON(w, http.StatusOK, map[string]any{"sent": len(res), "results": res})
}

// handlePushPrefs stores the server-honored per-event + quiet-hours prefs (global,
// single-user). Times are minutes-from-midnight in the daemon's local TZ.
func (s *server) handlePushPrefs(w http.ResponseWriter, r *http.Request) {
	var p pushPrefs
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpError(w, http.StatusBadRequest, "invalid prefs")
		return
	}
	s.push.setPrefs(p)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var sub webpush.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Endpoint == "" {
		httpError(w, http.StatusBadRequest, "invalid subscription")
		return
	}
	// Push gateways are always https; reject anything else so an authenticated
	// caller can't enroll an arbitrary endpoint the watcher/test would POST to.
	if u, err := url.Parse(sub.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		httpError(w, http.StatusBadRequest, "endpoint must be an https URL")
		return
	}
	s.push.addSub(&sub)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handlePushPresence(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Focused bool `json:"focused"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	s.push.setFocus(b.Focused)
	w.WriteHeader(http.StatusNoContent)
}
