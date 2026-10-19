package main

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionsServedStaleWhileRevalidate(t *testing.T) {
	s := testServer(t, config{token: "t", homeDir: t.TempDir()})
	var calls atomic.Int32
	slow := make(chan struct{})
	s.sessBuild = func(ctx context.Context, all bool) ([]byte, string, error) {
		n := calls.Add(1)
		if n > 1 {
			<-slow // every rebuild after the first is slow
		}
		return []byte(`{"sessions":[],"n":` + string(rune('0'+n)) + `}`), "build;dur=1", nil
	}
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleSessions(w, httptest.NewRequest("GET", "/api/rc/sessions", nil))
		return w
	}
	if w := get(); w.Header().Get("X-Sessions-Cache") != "miss" || w.Code != 200 {
		t.Fatalf("cold request must build: %d %s", w.Code, w.Header().Get("X-Sessions-Cache"))
	}
	if w := get(); w.Header().Get("X-Sessions-Cache") != "hit" || calls.Load() != 1 {
		t.Fatalf("fresh request must not rebuild: %s calls=%d", w.Header().Get("X-Sessions-Cache"), calls.Load())
	}
	// age the entry past the fresh window; the slow rebuild must not hold the answer
	s.sess.mu.Lock()
	for _, e := range s.sess.m {
		e.at = time.Now().Add(-time.Minute)
	}
	s.sess.mu.Unlock()
	start := time.Now()
	w := get()
	if w.Header().Get("X-Sessions-Cache") != "stale" || time.Since(start) > 3*time.Second {
		t.Fatalf("stale request must answer within the grace window: %s after %v", w.Header().Get("X-Sessions-Cache"), time.Since(start))
	}
	if w.Body.String() != `{"sessions":[],"n":1}` {
		t.Fatalf("stale body: %s", w.Body.String())
	}
	// a second stale request joins the in-flight rebuild instead of starting another
	_ = get()
	if calls.Load() != 2 {
		t.Fatalf("rebuilds must be single-flight, got %d builds", calls.Load())
	}
	close(slow)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w := get(); w.Header().Get("X-Sessions-Cache") == "hit" && w.Body.String() == `{"sessions":[],"n":2}` {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("refreshed body never became visible")
}
