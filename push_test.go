package main

import "testing"

// TestInQuiet exercises the pure quiet-hours window logic (no clock).
func TestInQuiet(t *testing.T) {
	const ( // a few minute-of-day anchors
		noon = 12 * 60
		t23  = 23 * 60
		t06  = 6 * 60
	)
	tests := []struct {
		name string
		p    pushPrefs
		cur  int
		want bool
	}{
		{"off never suppresses", pushPrefs{QuietOn: false, QuietStart: 0, QuietEnd: 1439}, noon, false},
		{"unset never suppresses", pushPrefs{QuietOn: true, QuietStart: -1, QuietEnd: -1}, noon, false},
		{"zero-width never suppresses", pushPrefs{QuietOn: true, QuietStart: 600, QuietEnd: 600}, 600, false},
		{"overnight 22-07 at 23:00 suppresses", pushPrefs{QuietOn: true, QuietStart: 1320, QuietEnd: 420}, t23, true},
		{"overnight 22-07 at 06:00 suppresses", pushPrefs{QuietOn: true, QuietStart: 1320, QuietEnd: 420}, t06, true},
		{"overnight 22-07 at noon allows", pushPrefs{QuietOn: true, QuietStart: 1320, QuietEnd: 420}, noon, false},
		{"same-day 09-17 at noon suppresses", pushPrefs{QuietOn: true, QuietStart: 540, QuietEnd: 1020}, noon, true},
		{"same-day 09-17 at 23:00 allows", pushPrefs{QuietOn: true, QuietStart: 540, QuietEnd: 1020}, t23, false},
		{"same-day boundary end is exclusive", pushPrefs{QuietOn: true, QuietStart: 540, QuietEnd: 1020}, 1020, false},
		{"same-day boundary start is inclusive", pushPrefs{QuietOn: true, QuietStart: 540, QuietEnd: 1020}, 540, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inQuiet(tc.p, tc.cur); got != tc.want {
				t.Fatalf("inQuiet(%+v, %d) = %v, want %v", tc.p, tc.cur, got, tc.want)
			}
		})
	}
}

// TestAllowPerEvent checks the per-event Kind->pref mapping (quiet-hours off so
// only the toggles gate). Server Kind "approval"<-Approve, "reply"<-Finished;
// "test"/unknown always pass.
func TestAllowPerEvent(t *testing.T) {
	tests := []struct {
		name  string
		prefs pushPrefs
		kind  string
		want  bool
	}{
		{"both on, approval", pushPrefs{Approve: true, Finished: true, QuietStart: -1, QuietEnd: -1}, "approval", true},
		{"both on, reply", pushPrefs{Approve: true, Finished: true, QuietStart: -1, QuietEnd: -1}, "reply", true},
		{"approve off suppresses approval", pushPrefs{Approve: false, Finished: true, QuietStart: -1, QuietEnd: -1}, "approval", false},
		{"approve off allows reply", pushPrefs{Approve: false, Finished: true, QuietStart: -1, QuietEnd: -1}, "reply", true},
		{"finished off suppresses reply", pushPrefs{Approve: true, Finished: false, QuietStart: -1, QuietEnd: -1}, "reply", false},
		{"finished off allows approval", pushPrefs{Approve: true, Finished: false, QuietStart: -1, QuietEnd: -1}, "approval", true},
		{"stall on allows stall", pushPrefs{Stall: true, QuietStart: -1, QuietEnd: -1}, "stall", true},
		{"stall off suppresses stall", pushPrefs{Stall: false, QuietStart: -1, QuietEnd: -1}, "stall", false},
		{"stall off allows approval", pushPrefs{Approve: true, Stall: false, QuietStart: -1, QuietEnd: -1}, "approval", true},
		{"test always passes per-event", pushPrefs{Approve: false, Finished: false, QuietStart: -1, QuietEnd: -1}, "test", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pm := &pushManager{prefs: tc.prefs}
			if got := pm.allow(tc.kind); got != tc.want {
				t.Fatalf("allow(%q) with %+v = %v, want %v", tc.kind, tc.prefs, got, tc.want)
			}
		})
	}
}

// TestNewPushManagerSubjectDefault verifies the safe default and override, and
// that the broken mailto:@localhost is never the default.
func TestNewPushManagerSubjectDefault(t *testing.T) {
	dir := t.TempDir()
	pm, err := newPushManager(dir, "")
	if err != nil {
		t.Fatalf("newPushManager: %v", err)
	}
	if pm.subject != defaultPushSubject {
		t.Fatalf("default subject = %q, want %q", pm.subject, defaultPushSubject)
	}
	if pm.subject == "mailto:deck-remote@localhost" {
		t.Fatalf("default subject is the broken non-routable mailto")
	}
	pm2, err := newPushManager(t.TempDir(), "mailto:x@y.com")
	if err != nil {
		t.Fatalf("newPushManager override: %v", err)
	}
	if pm2.subject != "mailto:x@y.com" {
		t.Fatalf("override subject = %q, want mailto:x@y.com", pm2.subject)
	}
}

// TestSetPrefsPersist asserts prefs round-trip through disk.
func TestSetPrefsPersist(t *testing.T) {
	dir := t.TempDir()
	pm, err := newPushManager(dir, "")
	if err != nil {
		t.Fatalf("newPushManager: %v", err)
	}
	want := pushPrefs{Approve: false, Finished: true, QuietOn: true, QuietStart: 1320, QuietEnd: 420}
	pm.setPrefs(want)

	pm2, err := newPushManager(dir, "")
	if err != nil {
		t.Fatalf("newPushManager reload: %v", err)
	}
	if pm2.prefs != want {
		t.Fatalf("reloaded prefs = %+v, want %+v", pm2.prefs, want)
	}
}

// TestShortEndpoint ensures logs only carry scheme+host, never the secret path.
func TestShortEndpoint(t *testing.T) {
	got := shortEndpoint("https://web.push.apple.com/abc/SECRET/token123")
	if got != "https://web.push.apple.com" {
		t.Fatalf("shortEndpoint leaked path: %q", got)
	}
	if shortEndpoint("not a url with spaces and a very long body that should truncate cleanly here") == "" {
		t.Fatalf("shortEndpoint fallback returned empty")
	}
}
