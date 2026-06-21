package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

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
	lastFocus time.Time // last time any client reported foreground (suppress window)
}

func newPushManager(dir string) (*pushManager, error) {
	pm := &pushManager{
		subject:  "mailto:deck-remote@localhost",
		subs:     map[string]*webpush.Subscription{},
		subsPath: filepath.Join(dir, "deck-remote-subs.json"),
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

type pushPayload struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"` // "reply" | "approval"
}

// send delivers a payload to every subscription, pruning dead ones (404/410).
func (pm *pushManager) send(p pushPayload) {
	if pm.suppressed() {
		return
	}
	body, _ := json.Marshal(p)
	pm.mu.Lock()
	subs := make([]*webpush.Subscription, 0, len(pm.subs))
	for _, s := range pm.subs {
		subs = append(subs, s)
	}
	pub, priv, subj := pm.vapidPub, pm.vapidPriv, pm.subject
	pm.mu.Unlock()

	var dead []string
	for _, s := range subs {
		resp, err := webpush.SendNotification(body, s, &webpush.Options{
			Subscriber:      subj,
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             60,
		})
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			dead = append(dead, s.Endpoint)
		}
	}
	if len(dead) > 0 {
		pm.mu.Lock()
		for _, e := range dead {
			delete(pm.subs, e)
		}
		pm.saveSubsLocked()
		pm.mu.Unlock()
	}
}

// --- HTTP handlers (deck-remote's own push API; the PWA uses these, not
// agent-deck's, so push fires on reliable events) ---

func (s *server) handlePushConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "publicKey": s.push.vapidPub})
}

func (s *server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var sub webpush.Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Endpoint == "" {
		httpError(w, http.StatusBadRequest, "invalid subscription")
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
