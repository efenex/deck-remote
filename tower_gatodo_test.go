package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// GA TODO (S6) fixtures: exact schema-1 keys, a P1 band of two, a P3 row, unprioritized rows.
const (
	gaSHA    = "0123456789abcdef0123456789abcdef01234567"
	gaKeyA   = "back@aaaaaaaaaaaa"              // P1 rank 0
	gaKeyB   = "back@bbbbbbbbbbbb"              // P1 rank 1
	gaKeyC   = "arch@cccccccccccc/dddddddddddd" // child key, P0
	gaKeyU   = "todo@eeeeeeeeeeee"              // unprioritized, open
	gaKeyX   = "back@ffffffffffff"              // checked: not startable
	gaKeyQ   = "back@121212121212"              // already queued (ga_todo.pending)
	gaKeyF   = "back@343434343434"              // in flight
	gaKeyP3  = "arch@565656565656"              // P3
	gaJournl = "/tower/scripts/journal.py"
)

func gaRowJSON(key, title, state string, prio string) string {
	r := map[string]any{"key": key, "doc": "docs/TODO-backlog.md", "line_start": 10, "line_end": 12, "parent": nil, "title": title,
		"section": "S", "doc_state": state, "closable": true, "closing_subentries": 0, "body_sha": "00112233445566ff",
		"body_head": "body of " + key, "body_url": "https://git.example/blob/" + gaSHA + "/docs/TODO-backlog.md#L10", "view_url": "file:///x"}
	if key == gaKeyA {
		r["w_key"] = "back:12@aaaaaaaaaaaa"
	}
	if prio != "" {
		var p map[string]any
		_ = json.Unmarshal([]byte(prio), &p)
		r["priority"] = p
	}
	return jsonStr(r)
}

func gaBlock(builtAt time.Time, sha string, extra string) string {
	rows := []string{
		gaRowJSON(gaKeyC, "Child thing", "open", `{"p":"P0","rank":0,"pending":false}`),
		gaRowJSON(gaKeyA, "Model title A", "open", `{"p":"P1","rank":0,"pending":false}`),
		gaRowJSON(gaKeyB, "Model title B", "shipped-marked", `{"p":"P1","rank":1,"pending":false}`),
		gaRowJSON(gaKeyU, "Unprioritized", "open", ""),
		gaRowJSON(gaKeyX, "Done already", "checked", ""),
		gaRowJSON(gaKeyQ, "Queued", "open", ""),
		gaRowJSON(gaKeyF, "Flying", "open", ""),
		gaRowJSON(gaKeyP3, "Later", "open", `{"p":"P3","rank":0,"pending":false}`),
	}
	b := `{"schema_version":1,"source_sha":"` + sha + `","source_ref":"origin/main","built_at":"` + builtAt.UTC().Format(time.RFC3339) +
		`","input_digest":"` + strings.Repeat("a", 64) + `","total_units":100,"total_open":40,"more":12,"collision_count":0,"orphan_count":0,` +
		`"stale":false,"error":"","items":[` + strings.Join(rows, ",") + `],` +
		`"pending":[{"id":"pick-gatodo-design-x-1","key":"` + gaKeyQ + `","mode":"design","status":"open"},` +
		`{"id":"ga-prio-20261125T100000Z-aaaaaaaaaaaa","edits":[{"key":"` + gaKeyU + `","p":"P2"}],"status":"open"}],` +
		`"in_flight_keys":["` + gaKeyF + `"]`
	if extra != "" {
		b += "," + extra
	}
	return b + "}"
}

// gaActServer: towerActServer whose model carries the given ga_todo block ("" = none).
func gaActServer(t *testing.T, block string) (*server, func(string) *httptest.ResponseRecorder, *[][]string, func(string)) {
	t.Helper()
	s, do, ran := towerActServer(t)
	set := func(block string) {
		var m map[string]any
		if err := json.Unmarshal([]byte(towerStubModel), &m); err != nil {
			t.Fatal(err)
		}
		if block != "" {
			m["ga_todo"] = json.RawMessage(block)
		}
		stub := filepath.Join(t.TempDir(), "stub_model.py")
		if err := os.WriteFile(stub, []byte("import sys\nsys.stdout.write("+jsonStr(jsonStr(m))+")\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		s.cfg.towerModelScript = stub
		s.towerM.invalidate()
	}
	set(block)
	return s, do, ran, set
}

func gaData(t *testing.T, argv []string) map[string]any {
	t.Helper()
	if len(argv) != 14 || argv[2] != gaJournl || argv[3] != "add" || argv[12] != "--data" {
		t.Fatalf("not a journal.py add argv: %q", argv)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(argv[13]), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGatodoStartJournalsExactKeyRequest(t *testing.T) {
	now := time.Now()
	for _, c := range []struct{ key, mode, wkey, title, state string }{
		{gaKeyA, "implement", "back:12@aaaaaaaaaaaa", "Model title A", "open"},
		{gaKeyC, "design", "", "Child thing", "open"},
		{gaKeyB, "verify", "", "Model title B", "shipped-marked"},
	} {
		_, do, ran, _ := gaActServer(t, gaBlock(now.Add(-10*time.Minute), gaSHA, ""))
		// a client-supplied title/body is ignored: dispatch data comes from the model row
		w := do(`{"op":"gatodo_start","key":"` + c.key + `","mode":"` + c.mode + `","view_sha":"` + gaSHA + `","body_sha":"00112233445566ff","title":"EVIL","body":"rm -rf"}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "queued") {
			t.Fatalf("%s: %d %s", c.key, w.Code, w.Body.String())
		}
		if len(*ran) != 2 {
			t.Fatalf("%s: want journal + wake, ran %q", c.key, *ran)
		}
		j := (*ran)[0]
		sum := sha256.Sum256([]byte(c.key))
		idRe := regexp.MustCompile(`^pick-gatodo-` + c.mode + `-` + hex.EncodeToString(sum[:])[:20] + `-[0-9a-f]{8}$`)
		want := []string{"python3", "-I", gaJournl, "add", "--kind", "request", "--id", j[7], "--to", "open", "--by", "operator", "--data", j[13]}
		if !reflect.DeepEqual(j, want) || !idRe.MatchString(j[7]) {
			t.Fatalf("%s: argv %q", c.key, j)
		}
		var wk any
		if c.wkey != "" {
			wk = c.wkey
		}
		wantData := map[string]any{"key": c.key, "w_key_seen": wk, "mode": c.mode, "title": c.title, "doc": "docs/TODO-backlog.md",
			"body_sha": "00112233445566ff", "view_sha": gaSHA, "from": "phone", "text": "GA TODO " + c.mode + ": " + c.title + " (" + c.key + ")"}
		if d := gaData(t, j); !reflect.DeepEqual(d, wantData) {
			t.Fatalf("%s: data\n got %v\nwant %v", c.key, d, wantData)
		}
		// field order is ga_todo.start_request's
		if !strings.HasPrefix(j[13], `{"key":`) || !strings.Contains(j[13], `"view_sha":"`+gaSHA+`","from":"phone","text":`) {
			t.Fatalf("%s: data order %s", c.key, j[13])
		}
		if wake := (*ran)[1]; wake[0] != "agent-deck" || wake[len(wake)-1] != "[tui] GA TODO request "+j[7]+"; read exact key/edits from journal" {
			t.Fatalf("%s: wake %q", c.key, wake)
		}
	}
}

func TestGatodoPrioJournalsEdit(t *testing.T) {
	now := time.Now()
	idRe := regexp.MustCompile(`^ga-prio-\d{8}T\d{6}Z-[0-9a-f]{12}$`)
	for _, c := range []struct{ body, edit string }{
		{`"key":"` + gaKeyU + `","p":"P1"`, `{"key":"` + gaKeyU + `","p":"P1"}`},                           // absent after: end of band
		{`"key":"` + gaKeyB + `","p":"P1","after":null`, `{"key":"` + gaKeyB + `","p":"P1","after":null}`}, // top of band
		{`"key":"` + gaKeyU + `","p":"P1","after":"` + gaKeyB + `"`, `{"key":"` + gaKeyU + `","p":"P1","after":"` + gaKeyB + `"}`},
		{`"key":"` + gaKeyA + `","p":"P3","after":"` + gaKeyP3 + `"`, `{"key":"` + gaKeyA + `","p":"P3","after":"` + gaKeyP3 + `"}`},
		{`"key":"` + gaKeyA + `","p":null`, `{"key":"` + gaKeyA + `","p":null}`}, // clear
		{`"id":"` + gaKeyC + `","p":"P2"`, `{"key":"` + gaKeyC + `","p":"P2"}`},  // id alias
	} {
		_, do, ran, _ := gaActServer(t, gaBlock(now.Add(-10*time.Minute), gaSHA, ""))
		w := do(`{"op":"gatodo_prio",` + c.body + `,"view_sha":"` + gaSHA + `"}`)
		if w.Code != 200 || len(*ran) != 2 {
			t.Fatalf("%s: %d %s ran %q", c.body, w.Code, w.Body.String(), *ran)
		}
		j := (*ran)[0]
		if !idRe.MatchString(j[7]) {
			t.Fatalf("%s: id %q", c.body, j[7])
		}
		want := `{"edits":[` + c.edit + `],"from":"phone","view_sha":"` + gaSHA + `","text":"GA TODO priority edits (1)"}`
		if j[13] != want {
			t.Fatalf("%s: data\n got %s\nwant %s", c.body, j[13], want)
		}
		gaData(t, j)
	}
}

func TestGatodoRefusals(t *testing.T) {
	now := time.Now()
	fresh := gaBlock(now.Add(-10*time.Minute), gaSHA, "")
	v := `,"view_sha":"` + gaSHA + `"`
	for _, c := range []struct {
		block, body string
		code        int
	}{
		{fresh, `{"op":"gatodo_start","key":"back@999999999999","mode":"design"` + v + `}`, 404},
		{fresh, `{"op":"gatodo_start","key":"../../state/x","mode":"design"` + v + `}`, 404},
		{fresh, `{"op":"gatodo_prio","key":"back@999999999999","p":"P1"` + v + `}`, 404},
		{"", `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design"` + v + `}`, 404}, // older tower: no block
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"ship"` + v + `}`, 400},
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyA + `"` + v + `}`, 400},
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design","p":"P1"` + v + `}`, 400},
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyA + `","p":"P4"` + v + `}`, 400},
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyA + `","p":"p1"` + v + `}`, 400},
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyA + `"` + v + `}`, 400}, // a clear must be explicit
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyA + `","p":1` + v + `}`, 400},
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyA + `","p":"P1","mode":"design"` + v + `}`, 400},
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyU + `","p":"P1","after":"back@999999999999"` + v + `}`, 400}, // unknown anchor
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyU + `","p":"P2","after":"` + gaKeyA + `"` + v + `}`, 400},    // anchor in another band
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyA + `","p":"P1","after":"` + gaKeyA + `"` + v + `}`, 400},    // self anchor
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyA + `","p":null,"after":"` + gaKeyB + `"` + v + `}`, 400},    // clear with anchor
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyA + `","p":"P1","after":7` + v + `}`, 400},
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyA + `","id":"` + gaKeyB + `","mode":"design"` + v + `}`, 400},
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design"}`, 400}, // no view_sha
		// stale / model-change
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design","view_sha":"` + strings.Repeat("9", 40) + `"}`, 409},
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design","body_sha":"ffffffffffffffff"` + v + `}`, 409},
		{gaBlock(now.Add(-121*time.Minute), gaSHA, ""), `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design"` + v + `}`, 409},
		{gaBlock(now.Add(-121*time.Minute), gaSHA, ""), `{"op":"gatodo_prio","key":"` + gaKeyA + `","p":"P0"` + v + `}`, 409},
		{gaBlock(now.Add(10*time.Minute), gaSHA, ""), `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design"` + v + `}`, 409},
		{strings.Replace(fresh, `"stale":false`, `"stale":true`, 1), `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design"` + v + `}`, 409},
		{strings.Replace(fresh, `"error":""`, `"error":"feed refused"`, 1), `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design"` + v + `}`, 409},
		{strings.Replace(fresh, `"schema_version":1`, `"schema_version":2`, 1), `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design"` + v + `}`, 409},
		{`{"schema_version":1,"items":"nope"}`, `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"design"` + v + `}`, 409},
		// not remaining / already queued / in flight
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyX + `","mode":"implement"` + v + `}`, 409},
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyQ + `","mode":"implement"` + v + `}`, 409}, // any mode: the exact key is queued
		{fresh, `{"op":"gatodo_start","key":"` + gaKeyF + `","mode":"verify"` + v + `}`, 409},
		{fresh, `{"op":"gatodo_prio","key":"` + gaKeyU + `","p":"P2"` + v + `}`, 409}, // the same edit is the key's last pending one
	} {
		_, do, ran, _ := gaActServer(t, c.block)
		if w := do(c.body); w.Code != c.code || len(*ran) != 0 {
			t.Fatalf("%s: %d want %d (%s), wrote %q", c.body, w.Code, c.code, w.Body.String(), *ran)
		}
	}
}

func TestGatodoDoubleTapAndConcurrency(t *testing.T) {
	now := time.Now()
	_, do, ran, _ := gaActServer(t, gaBlock(now.Add(-10*time.Minute), gaSHA, ""))
	start := `{"op":"gatodo_start","key":"` + gaKeyA + `","mode":"implement","view_sha":"` + gaSHA + `"}`
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- do(start).Code }()
	}
	wg.Wait()
	close(codes)
	n := map[int]int{}
	for c := range codes {
		n[c]++
	}
	if n[200] != 1 || n[409] != 11 || len(*ran) != 2 {
		t.Fatalf("concurrent taps: %v, ran %d steps", n, len(*ran))
	}
	// another mode for the same exact key is the same task: refused too
	if w := do(strings.Replace(start, "implement", "design", 1)); w.Code != 409 {
		t.Fatalf("second mode: %d", w.Code)
	}

	// priority: an identical second tap is refused, a different edit goes through, and going back
	// to the first edit afterwards is a real change (journal order decides), not a duplicate
	*ran = nil
	p1 := `{"op":"gatodo_prio","key":"` + gaKeyB + `","p":"P0","view_sha":"` + gaSHA + `"}`
	p2 := `{"op":"gatodo_prio","key":"` + gaKeyB + `","p":"P2","view_sha":"` + gaSHA + `"}`
	for i, c := range []struct {
		body string
		code int
	}{{p1, 200}, {p1, 409}, {p2, 200}, {p2, 409}, {p1, 200}} {
		if w := do(c.body); w.Code != c.code {
			t.Fatalf("step %d: %d want %d (%s)", i, w.Code, c.code, w.Body.String())
		}
	}
	if len(*ran) != 6 {
		t.Fatalf("three priority requests (journal + wake each), ran %d", len(*ran))
	}
}

func TestGatodoSnapshotChangeIsRevalidated(t *testing.T) {
	now := time.Now()
	_, do, ran, set := gaActServer(t, gaBlock(now.Add(-10*time.Minute), gaSHA, ""))
	body := `{"op":"gatodo_start","key":"` + gaKeyU + `","mode":"design","view_sha":"` + gaSHA + `"}`
	// GA refreshed: new source_sha, and the key is gone
	newSHA := strings.Repeat("b", 40)
	set(strings.Replace(gaBlock(now.Add(-time.Minute), newSHA, ""), gaKeyU, "todo@0f0f0f0f0f0f", -1))
	if w := do(body); w.Code != 409 || len(*ran) != 0 {
		t.Fatalf("old view_sha on a new snapshot: %d %s", w.Code, w.Body.String())
	}
	if w := do(strings.Replace(body, gaSHA, newSHA, 1)); w.Code != 404 || len(*ran) != 0 {
		t.Fatalf("gone key on the new snapshot: %d %s", w.Code, w.Body.String())
	}
	// the same snapshot ages past 120 minutes while the tab stays open
	set(gaBlock(now.Add(-119*time.Minute-55*time.Second), gaSHA, ""))
	if w := do(body); w.Code != 200 {
		t.Fatalf("fresh: %d %s", w.Code, w.Body.String())
	}
}

func TestGatodoActsWithoutActCapability(t *testing.T) {
	s, _, ran, _ := gaActServer(t, gaBlock(time.Now().Add(-10*time.Minute), gaSHA, ""))
	_, tok, err := s.tokens.mint("reader")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/rc/tower/act", strings.NewReader(`{"op":"gatodo_start","key":"`+gaKeyA+`","mode":"design","view_sha":"`+gaSHA+`"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Tailscale-User-Login", "ruben@example.com")
	req.RemoteAddr = "127.0.0.1:5555"
	w := httptest.NewRecorder()
	s.authAct(http.HandlerFunc(s.handleTowerAct)).ServeHTTP(w, req)
	if w.Code == 200 || len(*ran) != 0 {
		t.Fatalf("read token acted: %d %q", w.Code, *ran)
	}
}

// --- tower_model.py passthrough ---

// fakeGaTowerTUI: a TUI whose GA TODO section, when unfolded, lazily loads a schema-2 "full view"
// into the model and rewrites o["ga_todo"], as tower_tui.Model.load_gatodo does.
const fakeGaTowerTUI = fakeTowerTUI + `
class Model(Model):
    def __init__(self, feed, journal):
        super().__init__(feed, journal)
        self.sfold = {}
        self.sec = {"gatodo": []}
    def load_gatodo(self, force=False):
        self.o["ga_todo"] = {"schema_version": 2, "units": [{"key": "FULLVIEW", "body": "FULL BODY"}]}
        self.sec["gatodo"] = [dict(kind="gatodo", id="FULLVIEW", key="gatodo:FULLVIEW", head="FULL", attr=0, folded=None, fold_ok=False,
                                   confirm_ok=False, bangs=[], copy_text="FULL BODY", status="")]
        return True
    def visible(self):
        out = Model.__mro__[1].visible(self)
        if "ga_todo" in self.o:
            if "gatodo" not in self.sfold:
                self.load_gatodo()
            out.append(("gatodo", "GA TODO", self.sec["gatodo"], "gatodo" in self.sfold))
        return out
`

func runGaTowerModelPy(t *testing.T, overview string) map[string]any {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "tower_tui.py"), []byte(fakeGaTowerTUI), 0o600); err != nil {
		t.Fatal(err)
	}
	ov := filepath.Join(root, "overview.json")
	if err := os.WriteFile(ov, []byte(overview), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("python3", "-I", "scripts/tower_model.py", "--root", root, "--file", ov).Output()
	if err != nil {
		t.Fatalf("tower_model.py: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return doc
}

func TestTowerModelPassesBoundedGaTodo(t *testing.T) {
	var rows []string
	for i, p := range []string{"P0", "P1", "P1", "P2", "P2", "P2"} {
		rows = append(rows, gaRowJSON(fmt.Sprintf("back@%012x", i), "prio "+p, "open", `{"p":"`+p+`","rank":`+fmt.Sprint(i)+`,"pending":false}`))
	}
	for i := 0; i < 30; i++ { // 28 unprioritized + 2 P3: 25 survive, 5 are added to `more`
		prio := ""
		if i >= 28 {
			prio = `{"p":"P3","rank":0,"pending":true}`
		}
		rows = append(rows, gaRowJSON(fmt.Sprintf("todo@%012x", 0x100+i), fmt.Sprintf("other %d", i), "open", prio))
	}
	secret := "ghp_" + strings.Repeat("A", 30)
	rows[0] = strings.Replace(rows[0], `"title":"prio P0"`, `"title":"leak `+secret+`","meta":{"title":"v2 only"},"status":"open"`, 1)
	block := `{"schema_version":1,"source_sha":"` + gaSHA + `","source_ref":"origin/main","built_at":"2026-11-25T10:00:00Z","input_digest":"` +
		strings.Repeat("a", 64) + `","total_units":400,"total_open":90,"more":50,"collision_count":0,"orphan_count":1,"stale":false,"error":"",` +
		`"items":[` + strings.Join(rows, ",") + `],"pending":[{"id":"ga-prio-1","edits":[{"key":"k","p":"P1"}],"note":"Bearer ` + strings.Repeat("x", 30) + `"},{"id":"other-1"}],` +
		`"pending_errors":[],"pending_unverifiable":[],"units":[{"key":"FULL"}]}`
	doc := runGaTowerModelPy(t, `{"generated_at":"2026-11-25T10:00:00Z","in_flight":[{"id":"t1","ga_key":"back@000000000001"},{"id":"t2"}],"ga_todo":`+block+`}`)
	g, _ := doc["ga_todo"].(map[string]any)
	if g == nil {
		t.Fatalf("no ga_todo: %v", doc)
	}
	items := g["items"].([]any)
	explicit, others := 0, 0
	for _, it := range items {
		m := it.(map[string]any)
		if _, ok := m["meta"]; ok {
			t.Fatalf("schema-2 field passed through: %v", m)
		}
		if p, _ := m["priority"].(map[string]any); p != nil && p["p"] != "P3" {
			explicit++
		} else {
			others++
		}
	}
	if explicit != 6 || others != 25 || g["more"] != 55.0 {
		t.Fatalf("explicit %d others %d more %v", explicit, others, g["more"])
	}
	raw, _ := json.Marshal(g)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "Bearer "+strings.Repeat("x", 30)) || !strings.Contains(string(raw), "[redacted]") {
		t.Fatalf("not redacted: %s", raw)
	}
	if strings.Contains(string(raw), "FULL") || g["units"] != nil {
		t.Fatalf("full view leaked: %s", raw)
	}
	if p := g["pending"].([]any); len(p) != 1 || !reflect.DeepEqual(g["in_flight_keys"], []any{"back@000000000001"}) {
		t.Fatalf("pending %v in_flight_keys %v", p, g["in_flight_keys"])
	}
	if b, _ := g["payload_bytes"].(float64); b <= 0 || g["payload_budget"] != 81920.0 {
		t.Fatalf("payload accounting: %v / %v", g["payload_bytes"], g["payload_budget"])
	}
	for _, sec := range doc["sections"].([]any) {
		if sec.(map[string]any)["id"] == "gatodo" {
			t.Fatalf("the TUI's GA TODO section (full view) reached the phone model: %v", sec)
		}
	}
}

func TestTowerModelGaTodoSchemaAndAbsence(t *testing.T) {
	doc := runGaTowerModelPy(t, `{"generated_at":"2026-11-25T10:00:00Z","ga_todo":{"schema_version":2,"units":[{"key":"x","body":"B"}]}}`)
	g := doc["ga_todo"].(map[string]any)
	if g["stale"] != true || !strings.Contains(g["error"].(string), "schema 1 only") || len(g["items"].([]any)) != 0 {
		t.Fatalf("schema 2 block: %v", g)
	}
	doc = runGaTowerModelPy(t, `{"generated_at":"2026-11-25T10:00:00Z"}`)
	if _, ok := doc["ga_todo"]; ok {
		t.Fatalf("an older overview must not grow ga_todo: %v", doc["ga_todo"])
	}
}
