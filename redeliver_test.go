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
*"session send-status"*)
  if test -f '` + dir + `/status-error'; then cat '` + dir + `/status-error'; exit 2; fi
  cat '` + dir + `/status';;
*"session show"*) cat '` + dir + `/show';;
*"session redeliver"*)
  if test -f '` + dir + `/cli-error'; then cat '` + dir + `/cli-error' >&2; exit 2; fi
  key=$(printf '%s' "$*" | sed 's/.*--request-id \([^ ]*\).*/\1/')
  sed "s/TAP_KEY/$key/g" '` + dir + `/redeliver';;
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
	s.sends.running = true // tests drive polling synchronously
	it := &trackedSend{RequestID: "req1", SendID: "send1", SessionID: "s1", Profile: "phone", Created: time.Now().Add(-time.Minute).Unix()}
	s.sends.items[it.RequestID] = it
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","state":"queued","reason":"composer occupied","attempts":1,"updated_at":"2026-11-10T20:04:00Z"}`)
	putFixture(t, dir, "show", `{"id":"s1","status":"waiting","hook_status":"waiting","hook_status_fresh":true,"substate":"idle-at-empty-prompt"}`)
	putFixture(t, dir, "release", `{"send_id":"send1","session_id":"s1","state":"queued","outcome":"refused","reason":"composer occupied; nothing typed","attempts":1}`)
	putFixture(t, dir, "redeliver", `{"send_id":"send1","request_id":"TAP_KEY","outcome":"refused","reason":"composer_occupied","snapshot":{"session_id":"s1","state":"queued","attempts":1}}`)
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

func TestRedeliverEveryDecisionUsesOriginalID(t *testing.T) {
	for _, tc := range []struct{ outcome, state, want, reason string }{
		{"blocked_unknown", "typed", "blocked_unknown", "may have landed; not re-sent · check again"},
		{"unsupported", "submitted", "unsupported", "unverifiable"},
		{"already_landed", "landed", "delivered", "landed"},
		{"landed", "landed", "delivered", "landed"},
		{"pending", "queued", "pending", "in progress"},
		{"pending", "failed", "pending", "in progress"},
		{"conflict", "typed", "refused", "conflict"},
		{"not_found", "", "refused", "not found"},
		{"refused", "cancelled", "refused", "refused"},
		{"refused", "failed", "refused", "refused"},
	} {
		t.Run(tc.outcome+tc.state, func(t *testing.T) {
			s, dir, _ := releaseTestServer(t)
			result := map[string]any{"send_id": "send1", "request_id": "TAP_KEY", "outcome": tc.outcome, "reason": "fixture_reason"}
			if tc.state != "" {
				result["snapshot"] = map[string]any{"session_id": "s1", "state": tc.state, "landed_row_id": "row1"}
			}
			putFixture(t, dir, "redeliver", mustJSON(t, result))
			w := postRedeliver(s, retryBody)
			var ev map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &ev); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || ev["state"] != tc.want || !strings.Contains(ev["redeliveryReason"].(string), tc.reason) || ev["operationId"] != "tap1" {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			calls := releaseCalls(t, dir)
			if calls != "-p phone session redeliver send1 --request-id tap1 --json\n" {
				t.Fatal(calls)
			}
			if tc.state == "landed" && ev["landedRowId"] != "row1" {
				t.Fatal(ev)
			}
		})
	}
}

func TestRedeliverUnknownCommandNeverFallsBack(t *testing.T) {
	for _, message := range []string{"unknown command: redeliver", "unknown session command: redeliver"} {
		for _, state := range []string{"typed", "submitted", "failed", "queued"} {
			s, dir, _ := releaseTestServer(t)
			putFixture(t, dir, "cli-error", message)
			rec, reason, err := s.releaseSend(context.Background(), queuedSend{SendID: "send1", SessionID: "s1", State: state, Settled: true}, "tap1")
			if err != nil || mapSendState(rec).State != "unsupported" || !strings.Contains(reason, "unverifiable") {
				t.Fatalf("%+v %s %v", rec, reason, err)
			}
			if calls := releaseCalls(t, dir); strings.Count(calls, "session redeliver") != 1 || strings.Contains(calls, "queue release") || strings.Contains(calls, "session send ") {
				t.Fatal(calls)
			}
		}
	}
}

func TestRedeliverRejectsMismatchedAndUncertainReceipts(t *testing.T) {
	for _, result := range []string{
		`{}`, `not-json`,
		`{"send_id":"wrong","request_id":"TAP_KEY","outcome":"landed"}`,
		`{"send_id":"send1","request_id":"wrong","outcome":"landed"}`,
		`{"send_id":"send1","request_id":"TAP_KEY","outcome":"landed"}`,
		`{"send_id":"send1","request_id":"TAP_KEY","outcome":"pending","snapshot":{"session_id":"wrong","state":"queued"}}`,
		`{"send_id":"send1","request_id":"TAP_KEY","outcome":"future_value"}`,
	} {
		s, dir, _ := releaseTestServer(t)
		putFixture(t, dir, "redeliver", result)
		if w := postRedeliver(s, retryBody); w.Code != 502 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	s, dir, _ := releaseTestServer(t)
	putFixture(t, dir, "cli-error", "connection interrupted")
	if w := postRedeliver(s, retryBody); w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestRedeliverCurrentLandedSnapshotWinsOverStickyDecision(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	putFixture(t, dir, "redeliver", `{"send_id":"send1","request_id":"TAP_KEY","outcome":"blocked_unknown","replayed":true,"snapshot":{"session_id":"s1","state":"landed","landed_row_id":"row1"}}`)
	if w := postRedeliver(s, retryBody); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"delivered"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	putFixture(t, dir, "redeliver", `{"outcome":"unsupported","reason":"remote_redeliver_unavailable"}`)
	if w := postRedeliver(s, retryBody); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"unsupported"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func postRedeliver(s *server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleRedeliver(w, httptest.NewRequest("POST", "/api/rc/ask/redeliver", strings.NewReader(body)))
	return w
}

const retryBody = `{"operationId":"tap1","requestId":"req1","sessionId":"s1","sendId":"send1","profile":"phone"}`

func TestManualRedeliveryChecksOwnershipAndReusesOperation(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	for i := 0; i < 2; i++ {
		if w := postRedeliver(s, retryBody); w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	if w := postRedeliver(s, strings.Replace(retryBody, "tap1", "tap2", 1)); w.Code != 200 {
		t.Fatal(w.Code)
	}
	calls := releaseCalls(t, dir)
	if strings.Count(calls, "--request-id tap1") != 2 || strings.Count(calls, "--request-id tap2") != 1 {
		t.Fatal(calls)
	}
	for _, body := range []string{strings.Replace(retryBody, "s1", "other", 1), strings.Replace(retryBody, "phone", "other", 1)} {
		if w := postRedeliver(s, body); w.Code != 409 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	// Settled receipts leave the tracker; read-only status verifies their owner.
	delete(s.sends.items, "req1")
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","state":"failed"}`)
	if w := postRedeliver(s, retryBody); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(releaseCalls(t, dir), "send-status send1 --no-resume --json") {
		t.Fatal(releaseCalls(t, dir))
	}
	delete(s.sends.items, "req1")
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"other","state":"queued"}`)
	if w := postRedeliver(s, retryBody); w.Code != 409 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := postRedeliver(s, `{}`); w.Code != 400 {
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

func TestRedeliverRejectsSendIDsThatCouldParseAsFlags(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	for _, id := range []string{"--help", "-x", "../etc", "a b", ""} {
		body := `{"requestId":"req1","sessionId":"s1","sendId":` + strings.TrimSpace(mustJSON(t, id)) + `}`
		if w := postRedeliver(s, body); w.Code != 400 {
			t.Fatalf("%q: %d %s", id, w.Code, w.Body.String())
		}
	}
	if calls := releaseCalls(t, dir); calls != "" {
		t.Fatalf("CLI reached with an unvalidated id: %s", calls)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A phone that backgrounds mid-release cancels the HTTP request; the CLI that
// filed the upstream control request must still be allowed to finish.
func TestRedeliverSurvivesClientDisconnect(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	s.handleRedeliver(w, httptest.NewRequest("POST", "/api/rc/ask/redeliver", strings.NewReader(retryBody)).WithContext(ctx))
	if w.Code != 200 || strings.Count(releaseCalls(t, dir), "session redeliver") != 1 {
		t.Fatalf("%d %s calls=%s", w.Code, w.Body.String(), releaseCalls(t, dir))
	}
}

// An empty (failed/timed-out) capability probe must not pin the process to the
// legacy unguarded socket send for its whole lifetime.
func TestCapabilityProbeRetriesUntilItAnswers(t *testing.T) {
	bin, argv, _ := fakeAdeckAskQueue(t)
	s := askTestServer(t, bin)
	if flags := s.typedSendFlags(context.Background()); len(flags) != 0 {
		t.Fatalf("no help text yet: %v", flags)
	}
	putFixture(t, os.Getenv("FAKE_ADECK_DIR"), "help", "  -require-input-prompt\n")
	if flags := s.typedSendFlags(context.Background()); len(flags) != 1 || flags[0] != "--require-input-prompt" {
		t.Fatalf("probe was cached after an empty answer: %v\n%s", flags, readFile(t, argv))
	}
	before := strings.Count(readFile(t, argv), "--help")
	s.typedSendFlags(context.Background())
	if strings.Count(readFile(t, argv), "--help") != before {
		t.Fatal("a successful probe must be cached")
	}
}

func TestStatusReadsNeverResumeAndUnsupportedToolsNeverSayLost(t *testing.T) {
	for _, tool := range []string{"opencode", "shell", "ssh"} {
		for _, state := range []string{"typing", "typed", "submitted"} {
			s, dir, _ := releaseTestServer(t)
			putFixture(t, dir, "status", mustJSON(t, queuedSend{SendID: "send1", SessionID: "s1", State: state, Tool: tool, Settled: true, SentAt: time.Now().Add(-time.Hour)}))
			w := httptest.NewRecorder()
			s.handleSendStatus(w, httptest.NewRequest("GET", "/api/rc/ask/status?requestId=req1&sendId=send1&sessionId=s1&profile=phone", nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"unsupported"`) || strings.Contains(w.Body.String(), "lost") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if calls := releaseCalls(t, dir); calls != "-p phone session send-status send1 --no-resume --json\n" {
				t.Fatal(calls)
			}
		}
	}
}

func TestUntrackedOldCLIAndPrunedReceiptsAreShortRefusals(t *testing.T) {
	for _, tc := range []struct{ message, outcome string }{
		{"flag provided but not defined: -no-resume", "unsupported"},
		{`{"error":"send send1: sendqueue: unknown send id","code":"NOT_FOUND"}`, "not_found"},
	} {
		s, dir, _ := releaseTestServer(t)
		delete(s.sends.items, "req1")
		putFixture(t, dir, "status-error", tc.message)
		w := postRedeliver(s, retryBody)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"redeliveryOutcome":"`+tc.outcome+`"`) {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		if calls := releaseCalls(t, dir); strings.Contains(calls, "queue release") || strings.Contains(calls, "session redeliver") || strings.Count(calls, "--no-resume") != 1 {
			t.Fatal(calls)
		}
	}
}

func TestReopenedSendIsFollowedAgain(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	delete(s.sends.items, "req1")
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","state":"failed"}`)
	putFixture(t, dir, "redeliver", `{"send_id":"send1","request_id":"TAP_KEY","outcome":"pending","reason":"reopened","snapshot":{"session_id":"s1","state":"queued"}}`)
	if w := postRedeliver(s, retryBody); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if s.sends.items["req1"] == nil {
		t.Fatal("reopened send not tracked")
	}
	saved := newSendTracker(filepath.Dir(s.sends.path))
	saved.load()
	if saved.items["req1"] == nil {
		t.Fatal("reopened receipt not persisted")
	}
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","state":"landed","landed_row_id":"row1"}`)
	if s.pollSendsOnce() {
		t.Fatal("landed send still open")
	}
}

func TestRedeliverRequiresValidTapKeyBeforeCLI(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	for _, key := range []string{"", "--help", "../key", strings.Repeat("k", 65)} {
		if w := postRedeliver(s, strings.Replace(retryBody, "tap1", key, 1)); w.Code != 400 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	if calls := releaseCalls(t, dir); calls != "" {
		t.Fatal(calls)
	}
}

func TestSSHPlaceholderUsesUnverifiableEvenWithClaudeTool(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	s.lists.m["phone"] = listCacheEntry{sessions: []sessionInfo{{ID: "s1", Tool: "claude", SSHHost: "fixture-owner"}}}
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","tool":"claude","state":"typed","settled":true}`)
	rec, err := s.sendStatus(withProfile(context.Background(), "phone"), "send1", false)
	if err != nil || mapSendState(rec).State != "unsupported" {
		t.Fatalf("%+v %v", rec, err)
	}
	if calls := releaseCalls(t, dir); strings.Count(calls, "\n") != 1 {
		t.Fatal(calls)
	}
	// Unverifiable is about landing proof only. A queued SSH send is still
	// queued; a failed/cancelled one is final and must leave the tracker.
	for _, tc := range []struct{ state, want string }{{"queued", "queued"}, {"failed", "failed"}, {"cancelled", "failed"}} {
		putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","tool":"claude","state":"`+tc.state+`"}`)
		rec, err := s.sendStatus(withProfile(context.Background(), "phone"), "send1", false)
		st := mapSendState(rec)
		if err != nil || st.State != tc.want || st.Final != (tc.state != "queued") {
			t.Fatalf("%s: %+v %+v %v", tc.state, st, rec, err)
		}
	}
}

// The server's own poller keeps pre-R0 worker resumption: a plain send-status
// restarts a dead queue worker, so a queued phone send is not stranded after a
// reboot. Phone-triggered reads stay --no-resume. A pre-R0 agent-deck without
// --no-resume must not blind the poller either.
func TestPollerResumesWorkersButPhoneReadsDoNot(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	putFixture(t, dir, "status", `{"send_id":"send1","session_id":"s1","state":"failed","reason":"target not running"}`)
	ch := s.hub.subscribe()
	s.pollSendsOnce()
	if calls := releaseCalls(t, dir); calls != "-p phone session send-status send1 --json\n" {
		t.Fatalf("poller call: %q", calls)
	}
	var ev map[string]any
	if err := json.Unmarshal(<-ch, &ev); err != nil || ev["state"] != "failed" {
		t.Fatalf("%v %+v", err, ev)
	}
	if _, ok := s.sends.items["req1"]; ok {
		t.Fatal("final failed send must leave the tracker")
	}
}

func TestDeliveryStatusIsReadCapableButRedeliverStillNeedsAct(t *testing.T) {
	s, dir, _ := releaseTestServer(t)
	_, token, err := s.tokens.mint("fixture-reader")
	if err != nil {
		t.Fatal(err)
	}
	handler := s.routes()
	req := httptest.NewRequest("GET", "/api/rc/ask/status?requestId=req1&sendId=send1&sessionId=s1&profile=phone", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("POST", "/api/rc/ask/redeliver", strings.NewReader(retryBody))
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if calls := releaseCalls(t, dir); strings.Contains(calls, "redeliver") || !strings.Contains(calls, "--no-resume") {
		t.Fatal(calls)
	}
}
