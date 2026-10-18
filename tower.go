package main

// Optional "tower" feature (2026-10-18): deck-remote shows the operator's tower overview
// (ai-tooling/tower, scripts/overview.py -> state/overview/overview.json) as a tab next to the
// session list, and lets the operator answer an open ask from the phone.
//
// Off unless --tower-overview points at the JSON file. Reads are behind the usual bearer
// (shared or device token). ACTIONS (today: reply-to-ask) need BOTH a device token minted with
// the "act" capability AND the Tailscale identity header that `tailscale serve` injects on
// proxied requests (Tailscale-User-Login) matching --tower-operator; the shared token is never
// an action credential (it is forwarded upstream and was never scoped). Every action attempt,
// allowed or refused, is appended as one JSON line to <state>/deck-remote-tower-actions.log.
// "Send to session" (typing an ask's `! cmd` into a session) is deliberately NOT in v1.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	towerPollEvery = 10 * time.Second // file mtime poll for the SSE nudge
	towerReplyMax  = 4000             // bytes of reply text accepted from the phone
	towerDedupFor  = 10 * time.Second // same ask + same text within this window = a double tap
)

// towerStore reads the overview file with an mtime cache so the tab and the
// poller never parse an unchanged file twice.
type towerStore struct {
	path string

	mu    sync.Mutex
	mtime time.Time
	raw   []byte
	doc   map[string]any
}

func newTowerStore(path string) *towerStore {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return &towerStore{path: path}
}

// load returns the current document, re-reading only when the mtime moved.
func (t *towerStore) load() (map[string]any, []byte, time.Time, error) {
	st, err := os.Stat(t.path)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.doc != nil && st.ModTime().Equal(t.mtime) {
		return t.doc, t.raw, t.mtime, nil
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, nil, time.Time{}, err
	}
	t.doc, t.raw, t.mtime = doc, b, st.ModTime()
	return doc, b, st.ModTime(), nil
}

// ask returns the waiting entry with this id (the overview's `waiting` list), if any.
func (t *towerStore) ask(id string) (map[string]any, bool) {
	doc, _, _, err := t.load()
	if err != nil || id == "" {
		return nil, false
	}
	items, _ := doc["waiting"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m != nil && m["id"] == id {
			return m, true
		}
	}
	return nil, false
}

// --- identity ---------------------------------------------------------------

// tsLogin is the identity `tailscale serve` stamps on proxied requests. A
// client on the loopback side can forge it; that is accepted (loopback is the
// operator's own machine) because the act token remains the real secret.
// Callers must first check viaServeProxy: off that path nothing stamps or
// strips the header, so any client could claim any login.
func tsLogin(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
}

// viaServeProxy reports whether r came from a loopback peer, the shape of a
// request relayed by `tailscale serve` (which stamps Tailscale-User-Login and
// strips any client copy). Serve may proxy to plain HTTP or, in the hybrid prod
// layout, to https+insecure://127.0.0.1:8443, so TLS on our side says nothing.
// What matters is the peer: the --tls-cert LAN fallback's pf anchor is a pure
// rdr (destination rewrite, no nat), so a LAN or tailnet client reaching the
// listener that way keeps its own source address, as does any client of a
// non-loopback --listen. For those the header is client-controlled: refuse.
func viaServeProxy(r *http.Request) (bool, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return false, "request did not come from loopback (tailscale serve); Tailscale-User-Login cannot be trusted"
	}
	return true, ""
}

// bearerHeaderOnly reads the token from the Authorization header only: an action
// must never be authorised by a ?token= left in a URL or a referrer log.
func bearerHeaderOnly(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// towerActionOK decides whether r may perform a tower action. Returns the
// device-token name on success and a short refusal reason otherwise.
func (s *server) towerActionOK(r *http.Request) (device string, reason string) {
	if s.cfg.towerOperator == "" {
		return "", "actions disabled: --tower-operator not set"
	}
	tok := bearerHeaderOnly(r)
	dt, ok := s.tokens.find(tok)
	if !ok {
		if subtleConstantEq(tok, s.cfg.token) {
			return "", "the shared token cannot perform tower actions; mint a device token with caps [act]"
		}
		return "", "device token required"
	}
	if !dt.hasCap("act") {
		return "", "device token " + dt.Name + " lacks the act capability"
	}
	if ok, why := viaServeProxy(r); !ok {
		return dt.Name, why
	}
	login := tsLogin(r)
	if login == "" {
		return dt.Name, "no Tailscale-User-Login header (request did not come through tailscale serve)"
	}
	if !strings.EqualFold(login, s.cfg.towerOperator) {
		return dt.Name, "Tailscale identity is not the operator"
	}
	return dt.Name, ""
}

// authAct gates tower action endpoints. Refusals are logged with the reason.
func (s *server) authAct(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.tower == nil {
			httpError(w, http.StatusNotFound, "tower overview not configured")
			return
		}
		device, reason := s.towerActionOK(r)
		if reason != "" {
			s.towerLog(map[string]any{"action": r.URL.Path, "allowed": false, "device": device, "login": tsLogin(r), "reason": reason})
			httpError(w, http.StatusForbidden, reason)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// towerLog appends one JSON line per action attempt. Never carries tokens or
// the reply text itself (the text reaches tower's journal, not this log).
func (s *server) towerLog(fields map[string]any) {
	fields["ts"] = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(fields)
	if err != nil {
		return
	}
	path := filepath.Join(stateDir(s.cfg.homeDir), "deck-remote-tower-actions.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("tower: action log: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// --- handlers ---------------------------------------------------------------

// GET /api/rc/tower — the overview document (read token). {enabled:false} when
// the feature is off, so the PWA can hide the tab.
func (s *server) handleTower(w http.ResponseWriter, r *http.Request) {
	if s.tower == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	_, raw, mtime, err := s.tower.load()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "error": "overview unreadable: " + err.Error()})
		return
	}
	var buf bytes.Buffer
	buf.WriteString(`{"enabled":true,"mtime":`)
	buf.WriteString(time.Time(mtime).UTC().Format(`"2006-01-02T15:04:05Z"`))
	buf.WriteString(`,"actions":`)
	actions := "false"
	if s.cfg.towerOperator != "" {
		actions = "true"
	}
	buf.WriteString(actions)
	buf.WriteString(`,"overview":`)
	buf.Write(bytes.TrimSpace(raw))
	buf.WriteString("}")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// GET /api/rc/tower/whoami — what the server sees for this request: the token
// kind/caps and the Tailscale identity header. This is how the operator verifies,
// after deploy, that `tailscale serve` really stamps the login (and overrides a
// forged one): curl it with and without a fake header.
func (s *server) handleTowerWhoami(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"login": tsLogin(r), "operator_set": s.cfg.towerOperator != "", "actions_enabled": s.tower != nil && s.cfg.towerOperator != ""}
	tok := bearerHeaderOnly(r)
	if dt, ok := s.tokens.find(tok); ok {
		out["device"] = dt.Name
		out["caps"] = dt.Caps
		out["act"] = dt.hasCap("act")
	} else if subtleConstantEq(bearer(r), s.cfg.token) {
		out["device"] = "shared"
	}
	if _, reason := s.towerActionOK(r); reason != "" {
		out["would_refuse"] = reason
	}
	writeJSON(w, http.StatusOK, out)
}

type towerReplyReq struct {
	Ask  string `json:"ask"`
	Text string `json:"text"`
}

// POST /api/rc/tower/reply {ask, text} — the operator's answer to an open ask,
// sent to the tower session as a message tower journals and fans out (/ask).
// Gated by authAct. The ask must exist in the CURRENT overview (a stale tab
// cannot answer something already closed).
func (s *server) handleTowerReply(w http.ResponseWriter, r *http.Request) {
	var req towerReplyReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json")
		return
	}
	req.Ask = strings.TrimSpace(req.Ask)
	req.Text = strings.TrimSpace(req.Text)
	if req.Ask == "" || req.Text == "" {
		httpError(w, http.StatusBadRequest, "ask and text are required")
		return
	}
	if len(req.Text) > towerReplyMax {
		httpError(w, http.StatusBadRequest, "reply too long")
		return
	}
	ask, ok := s.tower.ask(req.Ask)
	if !ok {
		httpError(w, http.StatusNotFound, "ask not open in the current overview")
		return
	}
	if s.towerDup(req.Ask, req.Text) {
		httpError(w, http.StatusConflict, "same reply sent seconds ago")
		return
	}
	device, _ := s.towerActionOK(r)
	askText, _ := ask["text"].(string)
	msg := "[deck-remote reply] ask " + req.Ask + " (" + oneLineGo(askText, 80) + "): " + req.Text + "\n" +
		"(answered by " + device + " via deck-remote at " + time.Now().UTC().Format("2006-01-02T15:04Z") + "; journal it --to answered and fan out)"
	reqID := newRequestID()
	target := s.cfg.towerSession
	s.towerLog(map[string]any{"action": "reply", "allowed": true, "device": device, "login": tsLogin(r), "ask": req.Ask, "target": target, "requestId": reqID, "bytes": len(req.Text)})
	queued := s.queue.submit("tower:"+target, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		// Same shape as tower's own sendq: socket text, defer while the target is
		// mid-turn, no [agent-deck from:] envelope (the body says where it came from).
		_, err := s.sendMessage(ctx, target, msg, "--defer-if-busy", "--defer-timeout", "5m", "--no-tag")
		ev := map[string]any{"type": "tower-reply-result", "requestId": reqID, "ask": req.Ask, "ts": time.Now().Unix()}
		if err != nil {
			s.towerUndup(req.Ask, req.Text)
			ev["error"] = err.Error()
			log.Printf("tower: reply ask=%s req=%s ERROR: %v", req.Ask, reqID, err)
		} else {
			log.Printf("tower: reply ask=%s req=%s delivered to %s", req.Ask, reqID, target)
		}
		s.hub.publish(ev)
	})
	if !queued {
		s.towerUndup(req.Ask, req.Text) // nothing was sent: a retry must not read as a double tap
		httpError(w, http.StatusServiceUnavailable, "tower reply queue is full")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "requestId": reqID, "ask": req.Ask, "target": target})
}

// towerDup remembers the last (ask, text) for a short window so a double tap
// on the phone does not journal two answers.
func (s *server) towerDup(ask, text string) bool {
	s.towerMu.Lock()
	defer s.towerMu.Unlock()
	key := ask + "\x00" + text
	now := time.Now()
	if t, ok := s.towerLast[key]; ok && now.Sub(t) < towerDedupFor {
		return true
	}
	if s.towerLast == nil {
		s.towerLast = map[string]time.Time{}
	}
	for k, t := range s.towerLast {
		if now.Sub(t) > towerDedupFor {
			delete(s.towerLast, k)
		}
	}
	s.towerLast[key] = now
	return false
}

// towerUndup drops the dedupe entry towerDup reserved, for a reply that was
// never queued or whose delivery failed. towerDup reserves before the submit
// (not after) so two concurrent taps cannot both pass the check.
func (s *server) towerUndup(ask, text string) {
	s.towerMu.Lock()
	defer s.towerMu.Unlock()
	delete(s.towerLast, ask+"\x00"+text)
}

// runTowerWatch publishes an SSE nudge when the overview file's mtime moves, so
// an open tab reloads within towerPollEvery. No parsing here; the tab fetches.
func (s *server) runTowerWatch(ctx context.Context) {
	if s.tower == nil {
		return
	}
	var last time.Time
	t := time.NewTicker(towerPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := os.Stat(s.tower.path)
			if err != nil {
				continue
			}
			if !st.ModTime().Equal(last) {
				if !last.IsZero() {
					s.hub.publish(map[string]any{"type": "tower-overview", "mtime": st.ModTime().UTC().Format(time.RFC3339), "ts": time.Now().Unix()})
				}
				last = st.ModTime()
			}
		}
	}
}

func oneLineGo(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
