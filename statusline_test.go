package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var slNow = time.UnixMilli(1792578000000) // 2026-10-21T10:20Z

// slRow renders one `sqlite3 -json` row; record is the raw statusline payload.
func slRow(claude, inst string, capMs int64, record string) string {
	rec, _ := json.Marshal(record)
	return `{"claude_session_id":"` + claude + `","session_id":"` + inst + `","captured_at":` + itoa(capMs) + `,"record":` + string(rec) + `}`
}

// Shaped after a real record (ids redacted).
const slRecA = `{"claude_session_id":"cA","model":{"id":"claude-sonnet-5-5","display_name":"Sonnet 5.5"},"context_window":{"used_percentage":4,"context_window_size":1000000,"total_input_tokens":44033},"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":1792590000},"seven_day":{"used_percentage":25,"resets_at":1793077200}}}`

func slSnap(t *testing.T, rows ...string) *statuslineSnap {
	t.Helper()
	return parseStatuslineRows([]byte("[" + strings.Join(rows, ",") + "]"))
}

func TestParseStatuslineRowsAndJoin(t *testing.T) {
	cap := slNow.UnixMilli() - 60_000
	sn := slSnap(t,
		slRow("cA", "i1", cap, slRecA),
		slRow("cOld", "i1", cap-3_600_000, strings.Replace(slRecA, `"used_percentage":4,`, `"used_percentage":99,`, 1)),
		slRow("cNoInst", "", cap, slRecA),
		`{"claude_session_id":"bad","session_id":"x","captured_at":1,"record":"not json"}`,
	)
	if _, ok := sn.entryFor(sessionInfo{ID: "i1", ClaudeSessionID: "cA"}); !ok {
		t.Fatal("claude id join failed")
	}
	// After /clear the list carries the new claude id: the old conversation's
	// record (same agent-deck id) must not be shown for it.
	if _, ok := sn.entryFor(sessionInfo{ID: "i1", ClaudeSessionID: "cNew"}); ok {
		t.Error("rotated claude id matched a stale record")
	}
	// No claude id in the list yet: fall back to the agent-deck id, freshest wins.
	e, ok := sn.entryFor(sessionInfo{ID: "i1"})
	if !ok || *e.rec.ContextWindow.UsedPercentage != 4 {
		t.Errorf("instance fallback = %+v ok=%v, want the freshest (4%%)", e.rec.ContextWindow, ok)
	}
	if _, ok := sn.entryFor(sessionInfo{ID: "bad", ClaudeSessionID: "bad"}); ok {
		t.Error("unparseable record joined")
	}
	if got := parseStatuslineRows(nil); len(got.byClaude) != 0 {
		t.Error("empty output must be an empty snapshot")
	}
	var nilSnap *statuslineSnap
	if _, ok := nilSnap.entryFor(sessionInfo{ID: "i1"}); ok {
		t.Error("nil snapshot joined")
	}
}

func TestCtxFor(t *testing.T) {
	sn := slSnap(t, slRow("cA", "i1", slNow.UnixMilli()-60_000, slRecA))
	e, _ := sn.entryFor(sessionInfo{ClaudeSessionID: "cA"})
	c := ctxFor(e, slNow)
	if c == nil || c.Pct != 4 || c.Tokens != 44033 || c.Window != 1000000 || c.Model != "Sonnet 5.5" || c.AsOf != slNow.Unix()-60 {
		t.Fatalf("ctx = %+v", c)
	}
	// No used_percentage: derive it from tokens / window; no display name: id minus "claude-".
	sn = slSnap(t, slRow("cB", "i2", slNow.UnixMilli(), `{"model":{"id":"claude-opus-5-5"},"context_window":{"context_window_size":200000,"total_input_tokens":50000}}`))
	e, _ = sn.entryFor(sessionInfo{ClaudeSessionID: "cB"})
	if c := ctxFor(e, slNow); c == nil || c.Pct != 25 || c.Model != "opus-5-5" {
		t.Errorf("derived ctx = %+v", c)
	}
	// Too old: no bar. No context figure at all: no bar.
	e.capturedAt = slNow.Add(-13 * time.Hour)
	if ctxFor(e, slNow) != nil {
		t.Error("record older than statuslineMaxAge produced a ctx")
	}
	sn = slSnap(t, slRow("cC", "i3", slNow.UnixMilli(), `{"model":{"id":"x"}}`))
	e, _ = sn.entryFor(sessionInfo{ClaudeSessionID: "cC"})
	if ctxFor(e, slNow) != nil {
		t.Error("record without context produced a ctx")
	}
}

func rec(five, seven float64, r5, r7 int64) string {
	b, _ := json.Marshal(map[string]any{
		"model":          map[string]string{"id": "claude-opus-5-5"},
		"context_window": map[string]any{"used_percentage": 10},
		"rate_limits": map[string]any{
			"five_hour": map[string]any{"used_percentage": five, "resets_at": r5},
			"seven_day": map[string]any{"used_percentage": seven, "resets_at": r7},
		},
	})
	return string(b)
}

func fp(f float64) *float64 { return &f }

func TestQuotaByGroup(t *testing.T) {
	now := slNow
	ms := now.UnixMilli()
	r5, r7 := now.Unix()+3600, now.Unix()+86400
	sn := slSnap(t,
		// group "up" has two sessions: the fresher record wins.
		slRow("c1", "i1", ms-600_000, rec(10, 20, r5, r7)),
		slRow("c2", "i2", ms-60_000, rec(12, 20, r5, r7)),
		// "tower" is the same account as "up" at the moment c3 was captured? No: other numbers.
		slRow("c3", "i3", ms-120_000, rec(40, 70, r5+100, r7+100)),
		// "ai" agrees with "up" exactly: same account, one entry.
		slRow("c4", "i4", ms-30_000, rec(12, 20, r5, r7)),
		// expired 5h window (reset passed) and an archived session are ignored.
		slRow("c5", "i5", ms, rec(99, 50, now.Unix()-10, r7+7)),
		slRow("c6", "i6", ms, rec(77, 77, r5, r7)),
	)
	sessions := []sessionInfo{
		{ID: "i1", ClaudeSessionID: "c1", Group: "up"},
		{ID: "i2", ClaudeSessionID: "c2", Group: "up"},
		{ID: "i3", ClaudeSessionID: "c3", Group: "tower"},
		{ID: "i4", ClaudeSessionID: "c4", Group: "ai"},
		{ID: "i5", ClaudeSessionID: "c5", Group: "luna"},
		{ID: "i6", ClaudeSessionID: "c6", Group: "gone", Archived: true},
		{ID: "i7", ClaudeSessionID: "none", Group: "norecord"},
	}
	e8 := slEntry{rec: sn.byClaude["c4"].rec, capturedAt: now.Add(-10 * time.Second)}
	// Same account as "up"/"ai" (same reset pair) captured a moment later, one percent higher:
	// merges, and the fresher figures win.
	e8.rec.RateLimits.FiveHour = &rateWindow{UsedPercentage: fp(13), ResetsAt: r5}
	sn.byClaude["c8"] = e8
	sessions = append(sessions, sessionInfo{ID: "i8", ClaudeSessionID: "c8", Group: "rootlease"})
	q := quotaByGroup(sessions, sn, now)
	if len(q) != 3 {
		t.Fatalf("want 3 accounts, got %d: %+v", len(q), q)
	}
	byFirst := map[string]quotaInfo{}
	for _, e := range q {
		byFirst[e.Groups[0]] = e
	}
	ai := byFirst["ai"]
	if strings.Join(ai.Groups, ",") != "ai,rootlease,up" || ai.FiveHour.Pct != 13 || ai.SevenDay.Pct != 20 || ai.AsOf != now.Unix()-10 {
		t.Errorf("merged account = %+v", ai)
	}
	if tw := byFirst["tower"]; tw.FiveHour.Pct != 40 || tw.SevenDay.ResetsAt != r7+100 {
		t.Errorf("tower = %+v", tw)
	}
	lu := byFirst["luna"]
	if lu.FiveHour != nil || lu.SevenDay == nil || lu.SevenDay.Pct != 50 {
		t.Errorf("expired 5h window not dropped: %+v", lu)
	}
	if quotaByGroup(sessions, nil, now) != nil {
		t.Error("nil snapshot must give no quota")
	}
}

func TestAnnotateCtxOnlyClaude(t *testing.T) {
	sn := slSnap(t, slRow("c1", "i1", slNow.UnixMilli(), slRecA))
	ss := []sessionInfo{{ID: "i1", Tool: "claude", ClaudeSessionID: "c1"}, {ID: "i1", Tool: "codex", ClaudeSessionID: "c1"}}
	annotateCtx(ss, sn, slNow)
	if ss[0].Ctx == nil || ss[1].Ctx != nil {
		t.Errorf("ctx on claude=%v codex=%v", ss[0].Ctx, ss[1].Ctx)
	}
	b, _ := json.Marshal(ss[0])
	if !strings.Contains(string(b), `"ctx":{"pct":4,"tokens":44033,"window":1000000,"model":"Sonnet 5.5","asOf":`) {
		t.Errorf("wire shape: %s", b)
	}
}

func TestStatuslineStoreTTLAndStale(t *testing.T) {
	st := newStatuslineStore(t.TempDir())
	calls := 0
	fail := false
	st.run = func(ctx context.Context, db string) ([]byte, error) {
		calls++
		if !strings.Contains(db, filepath.Join("profiles", "")) || !strings.HasSuffix(db, "state.db") {
			t.Errorf("db path %q", db)
		}
		if fail {
			return nil, errors.New("boom")
		}
		return []byte("[" + slRow("c1", "i1", slNow.UnixMilli(), slRecA) + "]"), nil
	}
	ctx := context.Background()
	if sn := st.snapshot(ctx, "default"); sn == nil || len(sn.byClaude) != 1 {
		t.Fatal("first read")
	}
	st.snapshot(ctx, "default")
	if calls != 1 {
		t.Errorf("TTL not honoured: %d reads", calls)
	}
	// Expire; a failing read serves the previous snapshot and backs off one TTL.
	e := st.m["default"]
	e.at = time.Now().Add(-time.Minute)
	st.m["default"] = e
	fail = true
	if sn := st.snapshot(ctx, "default"); sn == nil || len(sn.byClaude) != 1 {
		t.Error("stale snapshot not served on failure")
	}
	st.snapshot(ctx, "default")
	if calls != 2 {
		t.Errorf("failure not backed off: %d reads", calls)
	}
	// Never read OK + failing: nil, not a panic.
	if sn := st.snapshot(ctx, "other"); sn != nil {
		t.Error("expected nil without any good read")
	}
}

// The real binary against a real table with the schema agent-deck uses.
func TestStatuslineStoreRealSqlite(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("no sqlite3 binary")
	}
	home := t.TempDir()
	st := newStatuslineStore(home)
	db := st.dbPath("default")
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatal(err)
	}
	sql := `create table claude_statuslines (claude_session_id TEXT PRIMARY KEY, session_id TEXT, captured_at INTEGER NOT NULL, record TEXT NOT NULL);
insert into claude_statuslines values ('c1','i1',` + itoa(slNow.UnixMilli()) + `, '` + strings.ReplaceAll(slRecA, `'`, `''`) + `');
insert into claude_statuslines values ('c2',NULL,` + itoa(slNow.UnixMilli()) + `, '{}');`
	if out, err := exec.Command(bin, db, sql).CombinedOutput(); err != nil {
		t.Fatalf("seed: %v %s", err, out)
	}
	sn := st.snapshot(context.Background(), "default")
	if sn == nil {
		t.Fatal("no snapshot")
	}
	e, ok := sn.entryFor(sessionInfo{ID: "i1", ClaudeSessionID: "c1"})
	if !ok || ctxFor(e, slNow) == nil || ctxFor(e, slNow).Tokens != 44033 {
		t.Errorf("round trip: %+v ok=%v", e.rec.ContextWindow, ok)
	}
	if st.execSqlite.Load() != 1 {
		t.Errorf("execSqlite = %d", st.execSqlite.Load())
	}
	// Missing db: error, no exec attempted, no snapshot.
	if sn := newStatuslineStore(t.TempDir()).snapshot(context.Background(), "default"); sn != nil {
		t.Error("missing db produced a snapshot")
	}
}
