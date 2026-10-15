package main

import (
	"testing"
	"time"
)

func TestApplyView(t *testing.T) {
	now := time.Now()
	since := now.Add(-time.Hour).Format(time.RFC3339)
	recent := now.Add(-time.Minute).Format(time.RFC3339Nano)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	sessions := []sessionInfo{
		{ID: "open-recent", Status: "idle", LastActivityAt: recent},
		{ID: "open-old", Status: "idle", LastActivityAt: old},
		{ID: "stopped-recent", Status: "stopped", LastActivityAt: recent},
		{ID: "archived", Status: "idle", LastActivityAt: recent, Archived: true},
		{ID: "needs-old", Status: "waiting", LastActivityAt: old, NeedsAttention: true},
		{ID: "transcript-only", Status: "running", LastActivity: now.Unix()},
		{ID: "zero-web-time", Status: "running", LastActivityAt: "0001-01-01T00:00:00Z", LastActivity: now.Add(-72 * time.Hour).Unix()},
		{ID: "unknown-time", Status: "running"},
	}
	ids := func(ss []sessionInfo) map[string]bool {
		m := map[string]bool{}
		for _, s := range ss {
			m[s.ID] = true
		}
		return m
	}

	v := &viewFilter{StatusFilter: "active", Statuses: []string{"running", "waiting", "idle", "starting", "queued"},
		TimeFilter: "today", TimeFilterLabel: "Today", ActiveSince: since}
	got, info := applyView(append([]sessionInfo(nil), sessions...), v)
	want := map[string]bool{"open-recent": true, "needs-old": true, "transcript-only": true, "unknown-time": true}
	if g := ids(got); len(g) != len(want) {
		t.Fatalf("kept %v, want %v", g, want)
	} else {
		for id := range want {
			if !g[id] {
				t.Fatalf("kept %v, want %v", g, want)
			}
		}
	}
	if info.Label != "Open · Today" || info.Hidden != 4 {
		t.Fatalf("info = %+v", info)
	}

	got, info = applyView(append([]sessionInfo(nil), sessions...), nil)
	if len(got) != len(sessions) || info != (viewInfo{}) {
		t.Fatalf("nil filter changed the list: %d %+v", len(got), info)
	}

	got, _ = applyView(append([]sessionInfo(nil), sessions...), &viewFilter{StatusFilter: "archived", Statuses: []string{}, Archived: true})
	if g := ids(got); len(g) != 2 || !g["archived"] || !g["needs-old"] {
		t.Fatalf("archived view kept %v", g)
	}
}
