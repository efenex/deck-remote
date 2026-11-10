package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// All CLI work is fake and all tracker/phone-log persistence is in t.TempDir.
func releaseTestServer(t *testing.T) (*server, string, *trackedSend) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent-deck")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + dir + `/calls'
case "$*" in
*"session send-status"*) cat '` + dir + `/status';;
*"session show"*) cat '` + dir + `/show';;
*"session queue release"*) cat '` + dir + `/release';;
*) exit 3;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", bin).CombinedOutput(); err != nil {
		t.Fatalf("shell validation: %s: %v", out, err)
	}
	s := askTestServer(t, bin)
	it := &trackedSend{RequestID: "req1", SendID: "send1", SessionID: "s1", Profile: "phone", Created: time.Now().Add(-time.Minute).Unix()}
	s.sends.items[it.RequestID] = it
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","state":"queued","reason":"composer occupied","attempts":1,"updated_at":"2026-11-10T20:04:00Z"}`)
	putFixture(t, dir, "show", `{"id":"s1","status":"waiting","hook_status":"waiting","hook_status_fresh":true,"substate":"idle-at-empty-prompt"}`)
	putFixture(t, dir, "release", `{"send_id":"send1","session_id":"s1","state":"queued","outcome":"refused","reason":"composer occupied; nothing typed","attempts":1}`)
	return s, dir, it
}

func putFixture(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func releaseCalls(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeliveryReasonsAndTimestamps(t *testing.T) {
	s, _, it := releaseTestServer(t)
	ch := s.hub.subscribe()
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	rec := queuedSend{SendID: "send1", SessionID: "s1", State: "queued", Reason: "target busy", TargetStatus: "running", Attempts: 1, UpdatedAt: at}
	s.observeSend(it, rec)
	var first map[string]any
	if err := json.Unmarshal(<-ch, &first); err != nil {
		t.Fatal(err)
	}
	if first["reason"] != "target busy" || first["statusTs"] != float64(at.Unix()) || first["targetStatus"] != "running" || first["attempts"] != float64(1) {
		t.Fatalf("event: %+v", first)
	}
	rec.Reason = "composer occupied"
	s.observeSend(it, rec) // same STATE, different reason must be published
	select {
	case b := <-ch:
		if !strings.Contains(string(b), "composer occupied") {
			t.Fatalf("event: %s", b)
		}
	case <-time.After(time.Second):
		t.Fatal("reason-only update dropped")
	}
	rec.State, rec.Settled = "submitted", true
	if got := sendStateEvent(it, rec); got["state"] != "lost" || !strings.Contains(got["reason"].(string), "unconfirmed") {
		t.Fatalf("false delivery proof: %+v", got)
	}
	rec.State, rec.LandedRowID, rec.LandedAt = "landed", "row1", at.Add(time.Second)
	if got := sendStateEvent(it, rec); got["state"] != "delivered" || got["landedRowId"] != "row1" || got["statusTs"] != rec.LandedAt.Unix() {
		t.Fatalf("landed event: %+v", got)
	}
}

func TestNoLandedTraceAfterTwoMinutesIsNotProofOfLoss(t *testing.T) {
	for _, state := range []string{"typing", "typed", "submitted"} {
		r := queuedSend{State: state, SentAt: time.Now().Add(-3 * time.Minute)}
		st := mapSendState(r)
		if st.State != "lost" || st.Final || st.Retryable {
			t.Fatalf("%s: %+v", state, st)
		}
		if !strings.Contains(sendReason(r), "2 minutes") {
			t.Fatal(sendReason(r))
		}
	}
	if st := mapSendState(queuedSend{State: "queued", SentAt: time.Now().Add(-time.Hour)}); st.State != "queued" {
		t.Fatal(st)
	}
}

func TestReleaseNeverReenqueuesUncertainOrFinishedMessages(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	for _, state := range []string{"typing", "typed", "submitted", "landed", "failed", "cancelled"} {
		r := queuedSend{SendID: "send1", SessionID: "s1", State: state, Settled: true}
		got, why := s.releaseSend(context.Background(), r)
		if got.State != state || why == "" {
			t.Fatalf("%s: %+v %s", state, got, why)
		}
	}
	if calls := releaseCalls(t, dir); calls != "" {
		t.Fatalf("unsafe CLI call: %s", calls)
	}
}

func TestReleaseUsesOriginalIDAndDoesNotTrustOutcomeDelivered(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	putFixture(t, dir, "release", `{"send_id":"send1","session_id":"s1","state":"submitted","outcome":"delivered"}`)
	r, why := s.releaseSend(withProfile(context.Background(), "phone"), queuedSend{SendID: "send1", SessionID: "s1", State: "queued"})
	if mapSendState(r).State != "sent" || !strings.Contains(why, "submitted; transcript landing not yet confirmed") {
		t.Fatalf("%+v %s", r, why)
	}
	calls := releaseCalls(t, dir)
	if !strings.Contains(calls, "session queue release send1 --json") || !strings.Contains(calls, "phone") || strings.Contains(calls, "session send ") {
		t.Fatalf("calls: %s", calls)
	}
	putFixture(t, dir, "release", `{"send_id":"different","session_id":"s1","state":"landed","outcome":"delivered"}`)
	r, why = s.releaseSend(context.Background(), queuedSend{SendID: "send1", SessionID: "s1", State: "queued"})
	if r.State != "queued" || !strings.Contains(why, "no matching") {
		t.Fatalf("%+v %s", r, why)
	}
}

func postRedeliver(s *server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleRedeliver(w, httptest.NewRequest("POST", "/api/rc/ask/redeliver", strings.NewReader(body)))
	return w
}

const retryBody = `{"requestId":"req1","sessionId":"s1","sendId":"send1","profile":"phone"}`

func TestManualRedeliveryChecksOwnershipAndDedupesBySendID(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	w := postRedeliver(s, retryBody)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "refused") || !strings.Contains(w.Body.String(), "composer occupied") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","state":"landed","landed_row_id":"row1"}`)
	w = postRedeliver(s, retryBody)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"delivered"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if calls := releaseCalls(t, dir); strings.Count(calls, "queue release") != 1 || strings.Contains(calls, "session send ") {
		t.Fatal(calls)
	}
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"other","state":"queued"}`)
	if w = postRedeliver(s, retryBody); w.Code != 409 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w = postRedeliver(s, `{}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestAutoReleaseRequiresFreshIdleEvidenceAndHonorsCapAcrossRestart(t *testing.T) {
	s, dir, it := releaseTestServer(t)
	r := queuedSend{SendID: "send1", SessionID: "s1", State: "queued", Attempts: 1}
	for _, show := range []string{
		`{"id":"s1","status":"waiting","hook_status":"running","hook_status_fresh":true,"substate":"idle-at-empty-prompt"}`,
		`{"id":"s1","status":"waiting","hook_status":"waiting","hook_status_fresh":false,"substate":"idle-at-empty-prompt"}`,
		`{"id":"s1","status":"waiting","hook_status":"waiting","hook_status_fresh":true,"substate":"picker"}`,
	} {
		putFixture(t, dir, "show", show)
		it.lastProbe = 0
		_, note := s.maybeAutoRelease(context.Background(), it, r)
		if note != "" || it.AutoAttempts != 0 {
			t.Fatal("released without fresh idle evidence")
		}
	}
	putFixture(t, dir, "show", `{"id":"s1","status":"waiting","hook_status":"waiting","hook_status_fresh":true,"substate":"idle-at-empty-prompt"}`)
	for i := 1; i <= maxAutoReleases; i++ {
		it.LastAutoAttempt, it.lastProbe = 0, 0
		got, note := s.maybeAutoRelease(context.Background(), it, r)
		if got.State != "queued" || !strings.Contains(note, "refused") || it.AutoAttempts != i {
			t.Fatalf("%+v %s (%d)", got, note, it.AutoAttempts)
		}
		if _, note = s.maybeAutoRelease(context.Background(), it, r); note != "" {
			t.Fatal("ignored backoff")
		}
	}
	t2 := newSendTracker(filepath.Dir(s.sends.path))
	t2.load()
	s.sends = t2
	if t2.items[it.RequestID].AutoAttempts != 3 {
		t.Fatal("cap not persisted")
	}
	it = t2.items[it.RequestID]
	it.LastAutoAttempt = 0
	if _, note := s.maybeAutoRelease(context.Background(), it, r); note != "" {
		t.Fatal("cap reset on restart")
	}
	if calls := releaseCalls(t, dir); strings.Count(calls, "queue release") != 3 || strings.Contains(calls, "session send ") {
		t.Fatal(calls)
	}
}

func TestCheckOnlyRecoversMissedSSEWithoutRelease(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	w := postRedeliver(s, strings.TrimSuffix(retryBody, "}")+`,"checkOnly":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "composer occupied") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if calls := releaseCalls(t, dir); strings.Contains(calls, "queue release") || strings.Contains(calls, "session send ") {
		t.Fatal(calls)
	}
}

func TestAutoReleasePreservesQueueOrder(t *testing.T) {
	s, dir, it := releaseTestServer(t)
	s.sends.items["older"] = &trackedSend{RequestID: "older", SessionID: "s1", SendID: "send0", Profile: "phone"}
	_, note := s.maybeAutoRelease(context.Background(), it, queuedSend{SendID: "send1", SessionID: "s1", State: "queued"})
	if note != "" || releaseCalls(t, dir) != "" {
		t.Fatal("automatic release bypassed an older pending message")
	}
}

func TestAutoReleaseFailsClosedOnBudgetPersistenceError(t *testing.T) {
	s, dir, it := releaseTestServer(t)
	s.sends.path = filepath.Join(dir, "missing", "ask-pending.json")
	_, note := s.maybeAutoRelease(context.Background(), it, queuedSend{SendID: "send1", SessionID: "s1", State: "queued"})
	if !strings.Contains(note, "could not persist") {
		t.Fatal(note)
	}
	if strings.Contains(releaseCalls(t, dir), "queue release") {
		t.Fatal("released without durable budget")
	}
}

func TestConcurrentAutoReleaseChecksAreSerialized(t *testing.T) {
	s, dir, it := releaseTestServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.maybeAutoRelease(context.Background(), it, queuedSend{SendID: "send1", SessionID: "s1", State: "queued"})
		}()
	}
	wg.Wait()
	if it.AutoAttempts != 1 {
		t.Fatalf("attempts = %d", it.AutoAttempts)
	}
	if calls := releaseCalls(t, dir); strings.Count(calls, "queue release") != 1 || strings.Count(calls, "session show") != 1 {
		t.Fatal(calls)
	}
}

func TestPhoneLogKeepsDeliveryDiagnostics(t *testing.T) {
	s, _, it := releaseTestServer(t)
	ev := sendStateEvent(it, queuedSend{State: "queued", Reason: "target busy", TargetStatus: "running", Attempts: 2, UpdatedAt: time.Now()})
	ev["redeliveryReason"] = "Automatic re-delivery check 1/3: refused"
	s.phones.tap(ev)
	b, err := os.ReadFile(s.phones.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"reason", "statusTs", "targetStatus", "attempts", "redeliveryReason"} {
		if !strings.Contains(string(b), `"`+field+`"`) {
			t.Fatalf("missing %s: %s", field, b)
		}
	}
}

func TestPhoneUsesPersistedPromptGuardWhenTransportFlagGone(t *testing.T) {
	bin, argv, _ := fakeAdeckAskQueue(t)
	putFixture(t, os.Getenv("FAKE_ADECK_DIR"), "help", "  -require-input-prompt\n  -image value\n")
	s := askTestServer(t, bin)
	_, err := s.enqueueSend(context.Background(), "s1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	calls := readFile(t, argv)
	if !strings.Contains(calls, "--require-input-prompt") || strings.Contains(calls, "--transport") {
		t.Fatal(calls)
	}
}
