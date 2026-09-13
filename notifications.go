package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// The notification inbox. Every push that passes the per-kind prefs is
// recorded here with its FULL body: the Web Push wire copy is a 140-rune
// preview (and iOS truncates further), so without this a notification is
// unreadable and a tap has nowhere to land. Recorded even when foreground
// suppression skips the OS notification — the user opted into that kind.
// Read state is per device and lives in the PWA (localStorage).

const (
	notifKeep    = 200  // newest entries kept on disk
	notifBodyMax = 8000 // runes of body kept per entry
)

type notification struct {
	ID        string `json:"id"`
	At        int64  `json:"at"` // unix milliseconds
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId,omitempty"`
	Title     string `json:"title"`
	Body      string `json:"body"`
}

func (pm *pushManager) loadNotifs() {
	b, err := os.ReadFile(pm.notifPath)
	if err != nil {
		return
	}
	var ns []notification
	if err := json.Unmarshal(b, &ns); err != nil {
		log.Printf("notifications: ignoring unreadable %s: %v", pm.notifPath, err)
		return
	}
	pm.notifs = ns
}

func (pm *pushManager) saveNotifsLocked() {
	b, _ := json.Marshal(pm.notifs)
	tmp := pm.notifPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("notifications: save: %v", err)
		return
	}
	if err := os.Rename(tmp, pm.notifPath); err != nil {
		log.Printf("notifications: save: %v", err)
	}
}

// record appends p to the inbox, trims it to notifKeep, persists it and
// notifies the live hook (SSE). IDs are base-36 nanoseconds: unique, and they
// sort in arrival order.
func (pm *pushManager) record(p pushPayload) notification {
	body := []rune(strings.TrimSpace(p.Body))
	if len(body) > notifBodyMax {
		body = append(body[:notifBodyMax], '…')
	}
	now := time.Now()
	n := notification{
		ID:        strconv.FormatInt(now.UnixNano(), 36),
		At:        now.UnixMilli(),
		Kind:      p.Kind,
		SessionID: p.SessionID,
		Title:     p.Title,
		Body:      string(body),
	}
	pm.mu.Lock()
	pm.notifs = append(pm.notifs, n)
	if over := len(pm.notifs) - notifKeep; over > 0 {
		pm.notifs = append([]notification(nil), pm.notifs[over:]...)
	}
	pm.saveNotifsLocked()
	hook := pm.onNotify
	pm.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return n
}

// listNotifs returns up to limit entries, newest first, older than the entry
// with ID before (all entries when before is empty or unknown).
func (pm *pushManager) listNotifs(limit int, before string) []notification {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	end := len(pm.notifs)
	if before != "" {
		for i, n := range pm.notifs {
			if n.ID == before {
				end = i
				break
			}
		}
	}
	out := make([]notification, 0, limit)
	for i := end - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, pm.notifs[i])
	}
	return out
}

// handleNotifications serves the inbox: GET /api/rc/notifications?limit=&before=
func (s *server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, notifKeep)
	}
	list := []notification{}
	if s.push != nil {
		list = s.push.listNotifs(limit, r.URL.Query().Get("before"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": list})
}
