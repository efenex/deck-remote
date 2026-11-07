package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// BACKLOG actions journal one open request (the same kind of record a picked suggestion
// writes), with tower's ids and data, then wake tower best-effort.
func TestTowerActBacklogJournalsTowerRequests(t *testing.T) {
	j := "/tower/scripts/journal.py"
	// data and wake are tower_tui.py's (start/defer/drop), field for field
	for _, c := range []struct {
		body, rid, data, wake string
	}{
		{`{"op":"backlog_start","id":"T-42.fix_Thing"}`, "pick-backlog-t-42-fix-thing",
			`{"text":"operator picked backlog task T-42.fix_Thing: dispatch now","task":"T-42.fix_Thing","from":"tui"}`, "[tui] picked backlog T-42.fix_Thing"},
		{`{"op":"backlog_defer","id":"T-42.fix_Thing","not_before":"2026-11-04T08:00:00Z"}`, "pick-defer-t-42-fix-thing",
			`{"text":"operator deferred backlog task T-42.fix_Thing until 2026-11-04","task":"T-42.fix_Thing","not_before":"2026-11-04T08:00:00Z","from":"tui"}`, "[tui] deferred backlog T-42.fix_Thing to 2026-11-04"},
		{`{"op":"backlog_drop","id":"T-42.fix_Thing"}`, "pick-drop-t-42-fix-thing",
			`{"text":"operator dropped backlog task T-42.fix_Thing","task":"T-42.fix_Thing","from":"tui"}`, "[tui] dropped backlog T-42.fix_Thing"},
	} {
		_, do, ran := towerActServer(t) // fresh server: no recent-action memory between cases
		w := do(c.body)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "queued") {
			t.Fatalf("%s: %d %s", c.body, w.Code, w.Body.String())
		}
		want := []string{"python3", "-I", j, "add", "--kind", "request", "--id", c.rid, "--to", "open", "--by", "operator", "--data", c.data}
		if len(*ran) != 2 || !reflect.DeepEqual((*ran)[0], want) {
			t.Fatalf("%s: argv\n got %q\nwant %q", c.body, *ran, want)
		}
		if wake := (*ran)[1]; wake[0] != "agent-deck" || wake[len(wake)-1] != c.wake {
			t.Fatalf("%s: wake %q", c.body, wake)
		}
		// a second tap before the overview shows the open request must not journal again
		*ran = nil
		if w := do(`{"op":"backlog_drop","id":"T-42.fix_Thing"}`); w.Code != 409 || len(*ran) != 0 {
			t.Fatalf("%s then drop: %d %s wrote %q", c.body, w.Code, w.Body.String(), *ran)
		}
	}
}

func TestTowerActBacklogRefusals(t *testing.T) {
	_, do, ran := towerActServer(t)
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"op":"backlog_start","id":"not-in-backlog"}`, 404},
		{`{"op":"backlog_start","id":"sg_Next.Thing"}`, 404},  // a suggestion is not a backlog row
		{`{"op":"backlog_drop","id":"t-queued"}`, 409},        // pick-defer-t-queued is open in the overview
		{`{"op":"backlog_defer","id":"T-42.fix_Thing"}`, 400}, // no date
		{`{"op":"backlog_defer","id":"T-42.fix_Thing","not_before":"2026-11-04"}`, 400},
		{`{"op":"backlog_defer","id":"T-42.fix_Thing","not_before":"2026-11-04T09:00:00Z"}`, 400},
		{`{"op":"backlog_defer","id":"T-42.fix_Thing","not_before":"2026-02-30T08:00:00Z"}`, 400},
		{`{"op":"backlog_start","id":"T-42.fix_Thing","not_before":"2026-11-04T08:00:00Z"}`, 400},
	} {
		*ran = nil
		if w := do(c.body); w.Code != c.code || len(*ran) != 0 {
			t.Fatalf("%s: %d want %d (%s), wrote %q", c.body, w.Code, c.code, w.Body.String(), *ran)
		}
	}
}

// A suggestion folds with the same record an ask's Fold writes, keyed by the suggestion id.
func TestTowerActFoldSuggestion(t *testing.T) {
	_, do, ran := towerActServer(t)
	for _, to := range []string{"fold", "unfold"} {
		*ran = nil
		if w := do(`{"op":"` + to + `","id":"sg_Next.Thing"}`); w.Code != 200 {
			t.Fatalf("%s: %d %s", to, w.Code, w.Body.String())
		}
		want := []string{"python3", "-I", "/tower/scripts/journal.py", "add", "--kind", "fold", "--id", "sg_Next.Thing", "--to", to + "ed", "--by", "operator", "--ref", "operator via overview TUI"}
		if len(*ran) != 1 || !reflect.DeepEqual((*ran)[0], want) {
			t.Fatalf("%s argv\n got %q\nwant %q", to, *ran, want)
		}
	}
}

func TestTowerSlugMatchesTab(t *testing.T) {
	// web/backlog.test.js asserts the same pairs for DeckBacklog.slug
	for in, want := range map[string]string{"T-42.fix_Thing": "t-42-fix-thing", "sg_Next.Thing": "sg-next-thing", "--": "x", strings.Repeat("a", 70): strings.Repeat("a", 60)} {
		if got := towerSlug(in); got != want {
			t.Fatalf("towerSlug(%q) = %q want %q", in, got, want)
		}
	}
}

// fakeTowerTUI is the smallest tower_tui.py tower_model.py can import.
const fakeTowerTUI = `
FEED = "state/overview/overview.json"
STATUS = {"": ("o", "dim"), "suggest": ("*", "st_suggest")}
STATUS_LEGEND = []
PALETTE = {}
def split_detail(t): return [{"k": "p", "t": t}] if t else []
def run_choices(it): return it.get("run_choices", [])
class Model:
    def __init__(self, feed, journal):
        self.feed = feed
        self.error = ""
    def load(self):
        import json
        self.o = json.load(open(self.feed))
        return True
    def header(self): return "TOWER"
    def header_item(self, n, sid, title, items, folded): return {"count": str(len(items)), "urgent": 0, "folded": folded}
    def visible(self):
        if "model_sections" in self.o:
            return self.o["model_sections"]
        items = [dict(kind="suggest", id=s["id"], key="suggest:" + s["id"], head=s["id"], attr=0, folded=s.get("folded"), fold_ok=True,
                      confirm_ok=False, bangs=[], copy_text="", status="suggest", title=s["title"], meta="", prompt="", why="", action="")
                 for s in self.o.get("suggested", [])]
        return [("suggested", "SUGGESTED", items, False)]
`

func runTowerModelPy(t *testing.T, overview string) map[string]any {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "tower_tui.py"), []byte(fakeTowerTUI), 0o600); err != nil {
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

func TestTowerModelPassesBacklogThrough(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	doc := runTowerModelPy(t, `{"generated_at":"2026-11-03T08:00:00Z",
	 "counts":{"waiting":0,"backlog":2,"suggested":2,"suggested_folded":1},
	 "backlog":[{"id":"T-1","title":"a","age_days":12,"deps_open":["T-0"]},{"id":"T-2","title":"b","not_before":"2026-11-10T08:00:00Z"},{"title":"no id"}],
	 "requests":[{"id":"pick-backlog-t-1","status":"open"},{"id":"pick-drop-t-9","status":"done"},{"id":"pick-drop-t-8","status":"declined"},{"id":"pick-drop-t-7","status":"both ready, dispatch at 20:00Z"},{"id":"r-nostatus"}],
	 "suggested":[{"id":"sg-a","title":"A"},{"id":"sg-b","title":"B","folded":true}]}`)
	if bl, _ := doc["backlog"].([]any); len(bl) != 2 {
		t.Fatalf("backlog rows (the id-less one dropped): %v", doc["backlog"])
	}
	if !reflect.DeepEqual(doc["counts"], map[string]any{"backlog": 2.0, "suggested": 2.0, "suggested_folded": 1.0}) {
		t.Fatalf("counts: %v", doc["counts"])
	}
	if !reflect.DeepEqual(doc["open_requests"], []any{"pick-backlog-t-1"}) {
		t.Fatalf("open_requests: %v", doc["open_requests"])
	}
	items := doc["sections"].([]any)[0].(map[string]any)["items"].([]any)
	if items[0].(map[string]any)["folded"] != nil || items[1].(map[string]any)["folded"] == nil {
		t.Fatalf("suggested[].folded not honoured: %v", items)
	}
}

func TestTowerModelWithoutBacklogKeys(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	doc := runTowerModelPy(t, `{"generated_at":"2026-11-03T08:00:00Z","counts":{"waiting":0},"suggested":[{"id":"sg-a","title":"A"}]}`)
	for _, k := range []string{"backlog", "counts", "open_requests"} {
		if _, ok := doc[k]; ok {
			t.Fatalf("an older overview must not grow %q: %v", k, doc[k])
		}
	}
	if doc["error"] != nil || len(doc["sections"].([]any)) != 1 {
		t.Fatalf("model: %v", doc)
	}
}
