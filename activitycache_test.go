package main

import (
	"testing"
	"time"
)

// TestStepStall drives the pure stall-detection transition through a sequence of
// parsed activities, asserting Stalled flips on only after the same concrete
// spinner label has persisted across >=stallThreshold polls — and never silently
// flips Working off.
func TestStepStall(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	step := func(n int) time.Time { return base.Add(time.Duration(n) * 6 * time.Second) }

	type poll struct {
		in          activityInfo
		wantWorking bool
		wantStalled bool
	}
	cases := []struct {
		name  string
		polls []poll
	}{
		{
			name: "not working never stalls",
			polls: []poll{
				{activityInfo{Working: false}, false, false},
				{activityInfo{Working: false}, false, false},
				{activityInfo{Working: false}, false, false},
			},
		},
		{
			name: "moving spinner stays live",
			polls: []poll{
				{activityInfo{Working: true, Activity: "Channelling… (0s)"}, true, false},
				{activityInfo{Working: true, Activity: "Channelling… (6s)"}, true, false},
				{activityInfo{Working: true, Activity: "Channelling… (12s)"}, true, false},
			},
		},
		{
			name: "frozen spinner stalls after threshold, working stays true",
			polls: []poll{
				{activityInfo{Working: true, Activity: "Channelling… (1m 0s)"}, true, false},
				{activityInfo{Working: true, Activity: "Channelling… (1m 0s)"}, true, true},
				{activityInfo{Working: true, Activity: "Channelling… (1m 0s)"}, true, true},
			},
		},
		{
			name: "movement clears a stall",
			polls: []poll{
				{activityInfo{Working: true, Activity: "Hammering… (10s)"}, true, false},
				{activityInfo{Working: true, Activity: "Hammering… (10s)"}, true, true},
				{activityInfo{Working: true, Activity: "Hammering… (16s)"}, true, false},
				{activityInfo{Working: true, Activity: "Hammering… (16s)"}, true, true},
			},
		},
		{
			name: "empty activity never stalls even if repeated",
			polls: []poll{
				{activityInfo{Working: true, Activity: ""}, true, false},
				{activityInfo{Working: true, Activity: ""}, true, false},
				{activityInfo{Working: true, Activity: ""}, true, false},
			},
		},
		{
			name: "stopping work resets, fresh spinner starts clean",
			polls: []poll{
				{activityInfo{Working: true, Activity: "Thinking… (5s)"}, true, false},
				{activityInfo{Working: true, Activity: "Thinking… (5s)"}, true, true},
				{activityInfo{Working: false}, false, false},
				{activityInfo{Working: true, Activity: "Thinking… (5s)"}, true, false},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var st activityState
			for i, p := range tc.polls {
				st = stepStall(st, p.in, step(i))
				if st.Working != p.wantWorking {
					t.Fatalf("poll %d: Working=%v want %v", i, st.Working, p.wantWorking)
				}
				if st.Stalled != p.wantStalled {
					t.Fatalf("poll %d: Stalled=%v want %v (activity=%q sameCount=%d)",
						i, st.Stalled, p.wantStalled, p.in.Activity, st.sameCount)
				}
			}
		})
	}
}
