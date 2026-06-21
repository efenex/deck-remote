package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// sseHub is a minimal fan-out for server-sent events. deck-remote publishes
// structured events here — chiefly async "reply" events when a turn finishes —
// and every connected phone client receives them. Status transitions still come
// from agent-deck's own /events/menu (proxied), so this hub only carries the
// gap-closing events deck-remote generates itself.
type sseHub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{clients: make(map[chan []byte]struct{})}
}

func (h *sseHub) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// publish marshals an event and fans it out. Slow clients drop the event rather
// than block the publisher (the phone re-fetches /reply on reconnect anyway).
func (h *sseHub) publish(event any) {
	b, err := json.Marshal(event)
	if err != nil {
		return
	}
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- b:
		default:
		}
	}
	h.mu.Unlock()
}

// GET /api/rc/events — SSE stream of deck-remote events (reply, approve-result,
// ask-state). Keyed by requestId so the PWA can match a reply to its prompt.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// Greet so the client knows the stream is live.
	_, _ = w.Write([]byte(": connected\n\n"))
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			_, _ = w.Write(msg)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}
