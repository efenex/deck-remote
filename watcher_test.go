package main

import (
	"testing"
	"time"
)

// TestPushDenylisted covers the attention-push denylist: the built-in zz-
// scratch-session prefix plus the configurable test group
// (DECK_REMOTE_PUSH_DENY_PREFIXES).
func TestPushDenylisted(t *testing.T) {
	tests := []struct {
		title string
		env   string
		want  bool
	}{
		{"zz-scratch turn test", "", true},
		{"zz-", "", true},
		{"zzfoo", "", false}, // prefix includes the dash
		{"fix the login bug", "", false},
		{"", "", false},
		{"deploy watchdog", "qa-, test-", false},
		{"qa-load soak", "qa-, test-", true},
		{"test-runner smoke", "qa-, test-", true},
		{"zz- still denied with a group set", "qa-", true},
	}
	for _, tc := range tests {
		t.Run(tc.title, func(t *testing.T) {
			t.Setenv("DECK_REMOTE_PUSH_DENY_PREFIXES", tc.env)
			if got := pushDenylisted(tc.title); got != tc.want {
				t.Fatalf("pushDenylisted(%q) with prefixes %q = %v, want %v", tc.title, tc.env, got, tc.want)
			}
		})
	}
}

// TestWatchAttentionDenylist pins the wiring: a normal session still pushes,
// a zz- session (and configured test group) never does — neither approvals nor
// questions.
func TestWatchAttentionDenylist(t *testing.T) {
	pm, err := newPushManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	var pushed []string
	pm.onNotify = func(n notification) { pushed = append(pushed, n.Kind) }
	s := &server{push: pm}
	attn := attentionInfo{Pending: true, ID: "a1", Kind: "approval"}

	// Control: a regular session's pending approval pushes as usual.
	s.watchAttention(sessionInfo{ID: "s1", Title: "fix the login bug"}, &sessWatch{}, false, attn)
	if len(pushed) != 1 || pushed[0] != "approval" {
		t.Fatalf("regular session push = %+v, want one approval", pushed)
	}

	t.Setenv("DECK_REMOTE_PUSH_DENY_PREFIXES", "qa-")
	pushed = nil
	s.watchAttention(sessionInfo{ID: "s2", Title: "zz-scratch turn test"}, &sessWatch{}, false, attn)
	s.watchAttention(sessionInfo{ID: "s3", Title: "zz-scratch turn test"}, &sessWatch{}, false,
		attentionInfo{Pending: true, ID: "q1", Kind: "question"})
	s.watchAttention(sessionInfo{ID: "s4", Title: "qa-load soak"}, &sessWatch{}, false, attn)
	if len(pushed) != 0 {
		t.Fatalf("denylisted sessions pushed: %+v", pushed)
	}
}

// TestStallShouldNotify covers the pure stall-notification decision, in
// particular the stallNotifyAfter gate that holds alerts back until a freeze has
// persisted long enough to not be noise.
func TestStallShouldNotify(t *testing.T) {
	tests := []struct {
		name         string
		stalled      bool
		isNew        bool
		notified     bool
		frozen       time.Duration
		wantPush     bool
		wantNotified bool
	}{
		{"not stalled clears latch", false, false, true, 5 * time.Minute, false, false},
		{"pre-existing stall on first sight is baselined", true, true, false, 5 * time.Minute, false, true},
		{"already notified does not re-push", true, false, true, 5 * time.Minute, false, true},
		{"short freeze does not push and does not latch", true, false, false, 30 * time.Second, false, false},
		{"just under threshold stays quiet", true, false, false, stallNotifyAfter - time.Second, false, false},
		{"at threshold pushes", true, false, false, stallNotifyAfter, true, true},
		{"long freeze pushes", true, false, false, 5 * time.Minute, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			push, notified := stallShouldNotify(tc.stalled, tc.isNew, tc.notified, tc.frozen)
			if push != tc.wantPush || notified != tc.wantNotified {
				t.Fatalf("stallShouldNotify(%v,%v,%v,%v) = (push=%v, notified=%v), want (push=%v, notified=%v)",
					tc.stalled, tc.isNew, tc.notified, tc.frozen, push, notified, tc.wantPush, tc.wantNotified)
			}
		})
	}
}
