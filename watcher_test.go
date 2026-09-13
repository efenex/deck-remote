package main

import (
	"testing"
	"time"
)

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
