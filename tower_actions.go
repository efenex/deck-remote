package main

// Tower tab parity with scripts/tower_tui.py (2026-10-19).
//
// READ: GET /api/rc/tower/model serves the TUI's own model (sections, folds, split_detail
// blocks, status glyphs/colours) as JSON. scripts/tower_model.py imports tower_tui.py from
// the tower checkout, so the phone cannot drift from the laptop; the result is cached on the
// (overview.json, journal.jsonl) mtimes.
//
// WRITE: POST /api/rc/tower/act {op, id[, kind][, not_before]} (GA TODO: {op, key, mode | p[, after], view_sha[, body_sha]}) behind authAct (act-cap device token +
// Tailscale identity). The ONLY writer is `python3 -I <root>/scripts/journal.py add …` with
// the same argv the TUI uses (tower_tui.Actions); nothing here touches journal.jsonl or
// state/. Every op is validated against the CURRENT model (kind/id must exist there and be
// foldable/closable/pickable), so a stale tab or a hand-made request cannot journal an
// arbitrary record.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	towerConfirmResult = "confirmed by operator in TUI"
	towerCloseNote     = "closed by operator in TUI"
	towerFoldRef       = "operator via overview TUI"
	towerExecTimeout   = 60 * time.Second
)

// towerRoot is the tower checkout: --tower-root, else derived from
// <root>/state/overview/overview.json.
func (c config) towerRootDir() string {
	if c.towerRoot != "" {
		return c.towerRoot
	}
	if c.towerOverview == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(c.towerOverview)))
}

// towerModel is the cached tower_model.py output plus an index of its items.
type towerModel struct {
	mu  sync.Mutex
	key string // "<overview mtime>|<journal mtime/size>"
	raw []byte
	idx *towerIndex

	// backlog request ids journaled from here recently (guarded by server.towerActMu): the
	// overview may not show the new open request until tower regenerates it, and a second
	// tap in that window must not journal a second action for the same task.
	recentBacklog map[string]time.Time
	// GA TODO actions journaled from here recently (guarded by server.towerActMu), keyed
	// "start:<exact key>" / "prio:<exact key>": the same double-tap bridge until the overview's
	// ga_todo.pending shows the new request.
	recentGA map[string]gaRecent
}

const towerBacklogRecent = 15 * time.Minute

// towerIndex is what handleTowerAct validates against: the model's items and sections, plus
// the overview's BACKLOG rows and the request ids that are still open (tower_model.py passes
// both through from overview.json; an older tower has neither, and the index is then empty).
type towerIndex struct {
	items    map[string]map[string]any // "kind:id" -> item
	secs     map[string]bool           // section ids
	backlog  map[string]bool           // backlog task ids
	openReqs map[string]bool           // open request ids (model request items + overview requests)
	ga       *gaSnap                   // the bounded GA TODO block (nil: an older tower/overview has none)
}

func fileStamp(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d.%d", st.ModTime().UnixNano(), st.Size())
}

// model returns the model JSON, regenerating it only when overview.json or journal.jsonl moved.
func (s *server) towerModelJSON(ctx context.Context) ([]byte, *towerIndex, error) {
	m := &s.towerM
	root := s.cfg.towerRootDir()
	key := fileStamp(s.cfg.towerOverview) + "|" + fileStamp(filepath.Join(root, "journal.jsonl"))
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raw != nil && m.key == key {
		return m.raw, m.idx, nil
	}
	script := s.cfg.towerModelScript
	if script == "" {
		script = filepath.Join(filepathDir(), "scripts", "tower_model.py")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "python3", "-I", script, "--root", root, "--file", s.cfg.towerOverview).Output()
	if err != nil {
		return nil, nil, fmt.Errorf("tower_model.py: %w", err)
	}
	var doc struct {
		Error    string `json:"error"`
		Sections []struct {
			ID    string           `json:"id"`
			Items []map[string]any `json:"items"`
		} `json:"sections"`
		Backlog []struct {
			ID string `json:"id"`
		} `json:"backlog"`
		OpenRequests []string        `json:"open_requests"`
		GaTodo       json.RawMessage `json:"ga_todo"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, nil, fmt.Errorf("tower_model.py output: %w", err)
	}
	if doc.Error != "" {
		return nil, nil, fmt.Errorf("%s", doc.Error)
	}
	idx := &towerIndex{items: map[string]map[string]any{}, secs: map[string]bool{}, backlog: map[string]bool{}, openReqs: map[string]bool{}}
	for _, sec := range doc.Sections {
		idx.secs[sec.ID] = true
		for _, it := range sec.Items {
			k, _ := it["kind"].(string)
			id, _ := it["id"].(string)
			idx.items[k+":"+id] = it
			if st, _ := it["status"].(string); k == "request" && st != "done" {
				idx.openReqs[id] = true
			}
		}
	}
	for _, b := range doc.Backlog {
		if b.ID != "" {
			idx.backlog[b.ID] = true
		}
	}
	for _, id := range doc.OpenRequests {
		idx.openReqs[id] = true
	}
	idx.ga = parseGaSnap(doc.GaTodo)
	if g := idx.ga; g != nil && g.PayloadBudget > 0 && g.PayloadBytes > g.PayloadBudget {
		// monitored, not enforced: explicit P0-P2 rows are never dropped to fit a cap
		log.Printf("tower: ga_todo block is %d bytes (target %d)", g.PayloadBytes, g.PayloadBudget)
	}
	m.key, m.raw, m.idx = key, out, idx
	return out, idx, nil
}

func (m *towerModel) invalidate() {
	m.mu.Lock()
	m.key, m.raw = "", nil
	m.mu.Unlock()
}

// GET /api/rc/tower/model
func (s *server) handleTowerModel(w http.ResponseWriter, r *http.Request) {
	if s.tower == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	t0 := time.Now()
	raw, _, err := s.towerModelJSON(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Server-Timing", "model;dur="+ms(time.Since(t0)))
	actions := "false"
	if s.cfg.towerOperator != "" {
		actions = "true"
	}
	_, _ = w.Write([]byte(`{"enabled":true,"actions":` + actions + `,"model":`))
	_, _ = w.Write(raw)
	_, _ = w.Write([]byte("}"))
}

// --- argv builders: identical to tower_tui.Actions ---------------------------

func (s *server) journalArgv(args ...string) []string {
	return append([]string{"python3", "-I", filepath.Join(s.cfg.towerRootDir(), "scripts", "journal.py"), "add"}, args...)
}

func (s *server) argvFold(id, to string) []string {
	return s.journalArgv("--kind", "fold", "--id", id, "--to", to, "--by", "operator", "--ref", towerFoldRef)
}

func jsonStr(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s *server) argvClose(kind, id string) ([]string, bool) {
	base := []string{"--kind", kind, "--id", id}
	switch kind {
	case "request":
		return s.journalArgv(append(base, "--to", "done", "--by", "operator", "--data", jsonStr(map[string]string{"result": towerConfirmResult}))...), true
	case "ask":
		return s.journalArgv(append(base, "--to", "closed", "--by", "operator", "--data", jsonStr(map[string]string{"answer": towerCloseNote}))...), true
	case "task":
		return s.journalArgv(append(base, "--to", "done", "--by", "operator", "--data", jsonStr(map[string]string{"done_evidence": towerCloseNote}))...), true
	}
	return nil, false
}

var towerSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func towerSlug(id string) string {
	s := strings.Trim(towerSlugRe.ReplaceAllString(strings.ToLower(id), "-"), "-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = "x"
	}
	return s
}

// pickArgvs: request record, suggest -> picked, then the (non-blocking) wake of tower.
func (s *server) pickArgvs(it map[string]any) [][]string {
	id, _ := it["id"].(string)
	title, _ := it["title"].(string)
	type pickData struct {
		Text       string `json:"text"`
		Suggestion string `json:"suggestion"`
		Task       any    `json:"task"`
		From       string `json:"from"`
	}
	d := pickData{Text: "operator picked suggestion " + id + ": " + title, Suggestion: id, Task: it["task"], From: "tui"}
	return [][]string{
		s.journalArgv("--kind", "request", "--id", "pick-"+towerSlug(id), "--to", "open", "--by", "operator", "--data", jsonStr(d)),
		s.journalArgv("--kind", "suggest", "--id", id, "--to", "picked", "--by", "operator"),
		append([]string{s.cfg.bin}, adeckArgs(s.cfg.profile, "session", "send", "--no-wait", s.cfg.towerSession, "[tui] picked suggestion "+id)...),
	}
}

// BACKLOG actions (tower overview-backlog): each journals one open request record that tower
// acts on, the same kind of record pickArgvs writes for a suggestion (`--kind request --to open
// --by operator`), then wakes tower best-effort. The request ids and data are tower's contract:
// pick-backlog-/pick-defer-/pick-drop-<slug(task)>, data {"text","task"[,"not_before"],"from":"tui"}.
var backlogReqPrefix = map[string]string{"backlog_start": "pick-backlog-", "backlog_defer": "pick-defer-", "backlog_drop": "pick-drop-"}

// backlogReqIDs: every request id a backlog action may write for this task; one of them
// already open means tower has the task queued, so a second action is refused.
func backlogReqIDs(task string) []string {
	return []string{"pick-backlog-" + towerSlug(task), "pick-defer-" + towerSlug(task), "pick-drop-" + towerSlug(task)}
}

// notBeforeRe is the only not_before shape the tab sends: a date at 08:00 UTC.
var notBeforeRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T08:00:00Z$`)

func (s *server) backlogArgvs(op, task, notBefore string) [][]string {
	// data and wake wording are the TUI's, byte for byte (tower_tui.py start/defer/drop): the
	// REQUESTS rows on every surface render data.text, and /tick reads data.task/not_before.
	type backlogData struct {
		Text      string `json:"text"`
		Task      string `json:"task"`
		NotBefore string `json:"not_before,omitempty"`
		From      string `json:"from"`
	}
	d := backlogData{Task: task, From: "tui"}
	var wake string
	switch op {
	case "backlog_defer":
		day := notBefore[:10] // validated YYYY-MM-DDT08:00:00Z
		d.Text, d.NotBefore = "operator deferred backlog task "+task+" until "+day, notBefore
		wake = "[tui] deferred backlog " + task + " to " + day
	case "backlog_drop":
		d.Text = "operator dropped backlog task " + task
		wake = "[tui] dropped backlog " + task
	default:
		d.Text = "operator picked backlog task " + task + ": dispatch now"
		wake = "[tui] picked backlog " + task
	}
	return [][]string{
		s.journalArgv("--kind", "request", "--id", backlogReqPrefix[op]+towerSlug(task), "--to", "open", "--by", "operator", "--data", jsonStr(d)),
		append([]string{s.cfg.bin}, adeckArgs(s.cfg.profile, "session", "send", "--no-wait", s.cfg.towerSession, wake)...),
	}
}

func (s *server) argvDismiss(id string) []string {
	return s.journalArgv("--kind", "suggest", "--id", id, "--to", "dismissed", "--by", "operator")
}

// towerRun finishes an accepted action step even if the phone disconnects. Each
// subprocess keeps its own timeout; injection sees the same context as production.
func (s *server) towerRun(argv []string) (int, string) {
	cctx, cancel := context.WithTimeout(context.Background(), towerExecTimeout)
	defer cancel()
	if s.towerExec != nil {
		return s.towerExec(cctx, argv)
	}
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = s.cfg.towerRootDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), strings.TrimSpace(string(out))
		}
		return -1, err.Error()
	}
	return 0, strings.TrimSpace(string(out))
}

type towerActReq struct {
	Op        string `json:"op"` // fold | unfold | close | pick | dismiss | backlog_start | backlog_defer | backlog_drop | gatodo_start | gatodo_prio
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	NotBefore string `json:"not_before"` // backlog_defer only: YYYY-MM-DDT08:00:00Z

	// GA TODO (gatodo_start / gatodo_prio). Key is the exact GA stable key (id is accepted as
	// an alias). ViewSHA is the snapshot source_sha the tab rendered and BodySHA the row body
	// hash it showed: compare-only preconditions, never journaled from the client.
	Key     string          `json:"key"`
	Mode    string          `json:"mode"`  // gatodo_start: design | implement | verify
	P       json.RawMessage `json:"p"`     // gatodo_prio: "P0".."P3", or null to clear
	After   json.RawMessage `json:"after"` // gatodo_prio: absent = end of band, null = top, "<key>" = after that row
	ViewSHA string          `json:"view_sha"`
	BodySHA string          `json:"body_sha"`
}

// POST /api/rc/tower/act — see the file comment. Gated by authAct.
func (s *server) handleTowerAct(w http.ResponseWriter, r *http.Request) {
	var req towerActReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json")
		return
	}
	req.ID, req.Key = strings.TrimSpace(req.ID), strings.TrimSpace(req.Key)
	if req.Key != "" {
		if req.ID != "" && req.ID != req.Key {
			httpError(w, http.StatusBadRequest, "id and key differ")
			return
		}
		req.ID = req.Key
	}
	if req.ID == "" || len(req.ID) > 200 {
		httpError(w, http.StatusBadRequest, "id required")
		return
	}
	// Validate and write under one lock: a double tap must see the model the first
	// tap already changed (the suggestion is gone), not pass validation twice.
	s.towerActMu.Lock()
	defer s.towerActMu.Unlock()
	_, idx, err := s.towerModelJSON(r.Context())
	if err != nil {
		httpError(w, http.StatusBadGateway, "overview unavailable: "+err.Error())
		return
	}
	items, secs := idx.items, idx.secs
	var steps [][]string
	var gaMemo string // gatodo_*: the recentGA entry to record once the journal write succeeded
	var wake bool     // last step is a best-effort wake: its failure does not fail the action
	switch req.Op {
	case "fold", "unfold":
		to := map[string]string{"fold": "folded", "unfold": "unfolded"}[req.Op]
		if sec, ok := strings.CutPrefix(req.ID, "section:"); ok {
			if !secs[sec] {
				httpError(w, http.StatusNotFound, "unknown section")
				return
			}
		} else {
			it := items["ask:"+req.ID]
			if it == nil {
				it = items["request:"+req.ID]
			}
			// an open suggestion folds like an ask (same fold record, keyed by the suggestion id);
			// tower's own fold_ok stays the gate
			if it == nil {
				it = items["suggest:"+req.ID]
			}
			if it == nil || it["fold_ok"] != true {
				httpError(w, http.StatusNotFound, "not a foldable open ask/request/suggestion in the current overview")
				return
			}
		}
		steps = [][]string{s.argvFold(req.ID, to)}
	case "close":
		it := items[req.Kind+":"+req.ID]
		if it == nil || it["confirm_ok"] != true {
			httpError(w, http.StatusNotFound, "not closable in the current overview")
			return
		}
		argv, ok := s.argvClose(req.Kind, req.ID)
		if !ok {
			httpError(w, http.StatusBadRequest, "cannot close a "+req.Kind)
			return
		}
		steps = [][]string{argv}
	case "pick", "dismiss":
		it := items["suggest:"+req.ID]
		if it == nil {
			httpError(w, http.StatusNotFound, "suggestion not open in the current overview")
			return
		}
		if req.Op == "pick" {
			steps, wake = s.pickArgvs(it), true
		} else {
			steps = [][]string{s.argvDismiss(req.ID)}
		}
	case "backlog_start", "backlog_defer", "backlog_drop":
		if !idx.backlog[req.ID] {
			httpError(w, http.StatusNotFound, "task not in the current BACKLOG")
			return
		}
		for _, rid := range backlogReqIDs(req.ID) {
			if at, ok := s.towerM.recentBacklog[rid]; idx.openReqs[rid] || (ok && time.Since(at) < towerBacklogRecent) {
				httpError(w, http.StatusConflict, "already queued with tower ("+rid+")")
				return
			}
		}
		if req.Op == "backlog_defer" {
			if !notBeforeRe.MatchString(req.NotBefore) {
				httpError(w, http.StatusBadRequest, "not_before must be YYYY-MM-DDT08:00:00Z")
				return
			}
			if _, err := time.Parse(time.RFC3339, req.NotBefore); err != nil {
				httpError(w, http.StatusBadRequest, "not_before: "+err.Error())
				return
			}
		} else if req.NotBefore != "" {
			httpError(w, http.StatusBadRequest, "not_before is for backlog_defer only")
			return
		}
		steps, wake = s.backlogArgvs(req.Op, req.ID, req.NotBefore), true
	case "gatodo_start", "gatodo_prio":
		var code int
		var msg string
		if steps, gaMemo, code, msg = s.gatodoSteps(req, idx.ga, time.Now()); steps == nil {
			httpError(w, code, msg)
			return
		}
		wake = true
	default:
		httpError(w, http.StatusBadRequest, "unknown op")
		return
	}

	device, _ := s.towerActionOK(r)
	msg := "ok"
	for i, argv := range steps {
		rc, out := s.towerRun(argv)
		last := i == len(steps)-1
		if rc != 0 && wake && last {
			msg = "queued in the journal; waking tower failed: " + oneLineGo(out, 120)
			break
		}
		if rc != 0 {
			s.towerLog(map[string]any{"action": "act", "op": req.Op, "kind": req.Kind, "id": req.ID, "allowed": true, "ok": false, "device": device, "login": tsLogin(r), "rc": rc})
			s.towerM.invalidate()
			httpError(w, http.StatusBadGateway, fmt.Sprintf("journal.py rc=%d: %s", rc, oneLineGo(out, 160)))
			return
		}
		if last && wake {
			msg = "queued → tower"
		}
	}
	s.towerM.invalidate()
	if p := backlogReqPrefix[req.Op]; p != "" {
		if s.towerM.recentBacklog == nil {
			s.towerM.recentBacklog = map[string]time.Time{}
		}
		for rid, at := range s.towerM.recentBacklog {
			if time.Since(at) >= towerBacklogRecent {
				delete(s.towerM.recentBacklog, rid)
			}
		}
		s.towerM.recentBacklog[p+towerSlug(req.ID)] = time.Now()
	}
	if gaMemo != "" {
		s.rememberGA(gaMemo)
	}
	s.towerLog(map[string]any{"action": "act", "op": req.Op, "kind": req.Kind, "id": req.ID, "allowed": true, "ok": true, "device": device, "login": tsLogin(r)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "op": req.Op, "id": req.ID, "msg": msg})
}

// warmTower builds the model once at startup so the first Tower tab load is a cache hit.
func (s *server) warmTower() {
	if s.tower == nil {
		return
	}
	if _, _, err := s.towerModelJSON(context.Background()); err != nil {
		log.Printf("tower: warm model: %v", err)
	}
}

// --- GA TODO (S6: GA TODO-view report §4) -------------------------------------------------------------
//
// The tab shows overview.json's bounded schema-1 `ga_todo` block (tower_model.py passes it through:
// all explicit P0-P2 rows plus at most 25 others). Two ops journal the same request records as the
// tower TUI (tower scripts/ga_todo.py start_request / priority_request), with from:"phone", then
// wake tower; tower's /tick re-resolves the exact key against a fresh GA read before anything runs.
// Nothing here reads or writes GA files. Validation is against the CURRENT bounded model only:
// exact key present, permitted mode / band, anchor in the destination band, a fresh snapshot
// (built_at within 120 min) and the snapshot the tab rendered (view_sha). Titles come from the
// model, never from the client.

const (
	gaStaleAfter = 120 * time.Minute
	gaSkew       = time.Minute // a laptop/server clock difference is not a "future" snapshot
	gaRecentFor  = 15 * time.Minute
)

var (
	gaBands = map[string]bool{"P0": true, "P1": true, "P2": true, "P3": true}
	gaModes = map[string]bool{"design": true, "implement": true, "verify": true}
	// doc_state values that can carry remaining work (shipped-marked may hold a residual); the
	// tick still checks the resolved status before dispatch.
	gaStartable = map[string]bool{"open": true, "shipped-marked": true}
)

type gaPrio struct {
	P    string `json:"p"`
	Rank int    `json:"rank"`
}

type gaRow struct {
	Key      string  `json:"key"`
	WKey     *string `json:"w_key"`
	Doc      string  `json:"doc"`
	Title    string  `json:"title"`
	BodySHA  string  `json:"body_sha"`
	DocState string  `json:"doc_state"`
	Priority *gaPrio `json:"priority"`
}

type gaPendingReq struct {
	ID    string            `json:"id"`
	Key   string            `json:"key"`
	Edits []json.RawMessage `json:"edits"`
}

type gaSnap struct {
	SchemaVersion any            `json:"schema_version"`
	SourceSHA     string         `json:"source_sha"`
	BuiltAt       string         `json:"built_at"`
	Stale         bool           `json:"stale"`
	Error         string         `json:"error"`
	Items         []gaRow        `json:"items"`
	Pending       []gaPendingReq `json:"pending"`
	InFlightKeys  []string       `json:"in_flight_keys"`
	PayloadBytes  int            `json:"payload_bytes"`
	PayloadBudget int            `json:"payload_budget"`

	bad   string // a block this server cannot read: every GA action is refused
	byKey map[string]*gaRow
}

type gaRecent struct {
	at   time.Time
	edit string // prio: the canonical edit last journaled for this key
}

func parseGaSnap(raw json.RawMessage) *gaSnap {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	g := &gaSnap{}
	if err := json.Unmarshal(raw, g); err != nil {
		return &gaSnap{bad: "unreadable GA TODO block: " + err.Error()}
	}
	if v, ok := g.SchemaVersion.(float64); !ok || v != 1 {
		g.bad = "GA TODO block is not schema 1"
	}
	g.byKey = map[string]*gaRow{}
	for i := range g.Items {
		if r := &g.Items[i]; r.Key != "" {
			g.byKey[r.Key] = r
		}
	}
	return g
}

// fresh: the reason a snapshot may not be acted on, "" when it may. Freshness is recomputed from
// built_at at action time: the block's own stale flag is only as new as overview.json.
func (g *gaSnap) fresh(now time.Time) string {
	if g.bad != "" {
		return g.bad
	}
	if g.Error != "" {
		return "GA TODO refused: " + g.Error
	}
	if g.Stale {
		return "GA TODO snapshot is stale; refresh first"
	}
	t, err := time.Parse(time.RFC3339, g.BuiltAt)
	if err != nil {
		return "GA TODO snapshot has no valid built_at"
	}
	if t.After(now.Add(gaSkew)) {
		return "GA TODO snapshot built_at is in the future"
	}
	if now.Sub(t) > gaStaleAfter {
		return "GA TODO snapshot older than 120 minutes; refresh first"
	}
	return ""
}

func (g *gaSnap) band(key string) string {
	if r := g.byKey[key]; r != nil && r.Priority != nil {
		return r.Priority.P
	}
	return ""
}

// gaEdit is one GA priority edit, {key, p, after?}. After: nil = absent (end of band),
// "null" = top of band, else the JSON string of the anchor key (GA's applier semantics).
type gaEdit struct {
	Key   string
	P     *string
	After json.RawMessage
}

func (e gaEdit) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"key":` + jsonStr(e.Key) + `,"p":`)
	if e.P == nil {
		b.WriteString("null")
	} else {
		b.WriteString(jsonStr(*e.P))
	}
	if e.After != nil {
		b.WriteString(`,"after":`)
		b.Write(e.After)
	}
	b.WriteString("}")
	return b.Bytes(), nil
}

// canonical identity of an edit (for the double-tap check); "" for an unreadable edit.
func (e gaEdit) canon() string { b, _ := e.MarshalJSON(); return string(b) }

func decodeGaEdit(raw json.RawMessage) (gaEdit, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return gaEdit{}, false
	}
	var e gaEdit
	if json.Unmarshal(m["key"], &e.Key) != nil {
		return gaEdit{}, false
	}
	if p, ok := m["p"]; ok && string(p) != "null" {
		var s string
		if json.Unmarshal(p, &s) != nil {
			return gaEdit{}, false
		}
		e.P = &s
	}
	if a, ok := m["after"]; ok {
		e.After = compactJSON(a)
	}
	return e, true
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

// lastEdit: the newest edit for key, from this server's recent memory or, failing that, the open
// ga-prio-* requests in journal order (each request's last edit per key wins, as in GA's applier).
func (s *server) lastGaEdit(g *gaSnap, key string, now time.Time) string {
	if r, ok := s.towerM.recentGA["prio:"+key]; ok && now.Sub(r.at) < gaRecentFor {
		return r.edit
	}
	last := ""
	for _, req := range g.Pending {
		if !strings.HasPrefix(req.ID, "ga-prio-") {
			continue
		}
		for _, raw := range req.Edits {
			if e, ok := decodeGaEdit(raw); ok && e.Key == key {
				last = e.canon()
			}
		}
	}
	return last
}

// gaPrioEdit validates a gatodo_prio request against the model: 400 for a bad enum or anchor.
func gaPrioEdit(req towerActReq, g *gaSnap) (gaEdit, string) {
	e := gaEdit{Key: req.ID}
	p := bytes.TrimSpace(req.P)
	switch {
	case len(p) == 0:
		return e, "p required: P0..P3, or null to clear"
	case string(p) != "null":
		var band string
		if json.Unmarshal(p, &band) != nil || !gaBands[band] {
			return e, "p must be P0, P1, P2, P3 or null"
		}
		e.P = &band
	}
	a := bytes.TrimSpace(req.After)
	if len(a) == 0 {
		return e, ""
	}
	if string(a) == "null" {
		if e.P == nil {
			return e, "after is for setting a band, not for a clear"
		}
		e.After = json.RawMessage("null")
		return e, ""
	}
	var anchor string
	if json.Unmarshal(a, &anchor) != nil {
		return e, "after must be a key or null"
	}
	switch {
	case e.P == nil:
		return e, "after is for setting a band, not for a clear"
	case anchor == req.ID:
		return e, "a row cannot follow itself"
	case g.byKey[anchor] == nil:
		return e, "after: anchor not in the current GA TODO model"
	case g.band(anchor) != *e.P:
		return e, "after: anchor must be in the destination band " + *e.P
	}
	e.After = json.RawMessage(jsonStr(anchor))
	return e, ""
}

func gaRandHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// gatodoSteps validates one GA TODO action and returns its argv (journal request, then the wake),
// or nil with the HTTP status and message. memo is what rememberGA records after the write.
func (s *server) gatodoSteps(req towerActReq, g *gaSnap, now time.Time) (steps [][]string, memo string, code int, msg string) {
	if g == nil {
		return nil, "", http.StatusNotFound, "no GA TODO block in the current overview"
	}
	if why := g.fresh(now); why != "" {
		return nil, "", http.StatusConflict, why
	}
	if req.ViewSHA == "" {
		return nil, "", http.StatusBadRequest, "view_sha required (the snapshot the tab shows)"
	}
	if req.ViewSHA != g.SourceSHA {
		return nil, "", http.StatusConflict, "GA TODO snapshot changed; review it and try again"
	}
	row := g.byKey[req.ID]
	if row == nil {
		return nil, "", http.StatusNotFound, "key not in the current GA TODO model"
	}
	if req.BodySHA != "" && req.BodySHA != row.BodySHA {
		return nil, "", http.StatusConflict, "GA TODO entry changed; review it and try again"
	}
	var rid string
	var data string
	if req.Op == "gatodo_start" {
		if len(req.P) != 0 || len(req.After) != 0 {
			return nil, "", http.StatusBadRequest, "p/after are for gatodo_prio"
		}
		if !gaModes[req.Mode] {
			return nil, "", http.StatusBadRequest, "mode must be design, implement or verify"
		}
		if !gaStartable[row.DocState] {
			return nil, "", http.StatusConflict, "not remaining work (doc_state " + row.DocState + ")"
		}
		for _, k := range g.InFlightKeys {
			if k == row.Key {
				return nil, "", http.StatusConflict, "exact GA key already in flight"
			}
		}
		for _, p := range g.Pending {
			if strings.HasPrefix(p.ID, "pick-gatodo-") && p.Key == row.Key {
				return nil, "", http.StatusConflict, "exact GA key already queued with tower (" + p.ID + ")"
			}
		}
		if r, ok := s.towerM.recentGA["start:"+row.Key]; ok && now.Sub(r.at) < gaRecentFor {
			return nil, "", http.StatusConflict, "exact GA key already queued with tower"
		}
		// tower ga_todo.start_request, field for field; the full key is the identity, the digest
		// keeps ids distinct for keys whose slugs would collide.
		sum := sha256.Sum256([]byte(row.Key))
		rid = "pick-gatodo-" + req.Mode + "-" + hex.EncodeToString(sum[:])[:20] + "-" + gaRandHex(8)
		type startData struct {
			Key      string  `json:"key"`
			WKeySeen *string `json:"w_key_seen"`
			Mode     string  `json:"mode"`
			Title    string  `json:"title"`
			Doc      string  `json:"doc"`
			BodySHA  string  `json:"body_sha"`
			ViewSHA  string  `json:"view_sha"`
			From     string  `json:"from"`
			Text     string  `json:"text"`
		}
		d := startData{Key: row.Key, WKeySeen: row.WKey, Mode: req.Mode, Title: row.Title, Doc: row.Doc, BodySHA: row.BodySHA,
			ViewSHA: g.SourceSHA, From: "phone", Text: "GA TODO " + req.Mode + ": " + row.Title + " (" + row.Key + ")"}
		data, memo = jsonStr(d), "start:"+row.Key+"\n"
	} else {
		if req.Mode != "" {
			return nil, "", http.StatusBadRequest, "mode is for gatodo_start"
		}
		e, bad := gaPrioEdit(req, g)
		if bad != "" {
			return nil, "", http.StatusBadRequest, bad
		}
		if s.lastGaEdit(g, row.Key, now) == e.canon() {
			return nil, "", http.StatusConflict, "the same priority edit is already pending with tower"
		}
		rid = "ga-prio-" + now.UTC().Format("20060102T150405Z") + "-" + gaRandHex(12)
		type prioData struct {
			Edits   []gaEdit `json:"edits"`
			From    string   `json:"from"`
			ViewSHA string   `json:"view_sha"`
			Text    string   `json:"text"`
		}
		data = jsonStr(prioData{Edits: []gaEdit{e}, From: "phone", ViewSHA: g.SourceSHA, Text: "GA TODO priority edits (1)"})
		memo = "prio:" + row.Key + "\n" + e.canon()
	}
	return [][]string{
		s.journalArgv("--kind", "request", "--id", rid, "--to", "open", "--by", "operator", "--data", data),
		// the TUI's wake wording (tower_tui.Actions.ga_request): tower reads the exact key/edits from the journal
		append([]string{s.cfg.bin}, adeckArgs(s.cfg.profile, "session", "send", "--no-wait", s.cfg.towerSession,
			"[tui] GA TODO request "+rid+"; read exact key/edits from journal")...),
	}, memo, 0, ""
}

// rememberGA records a journaled GA action, memo "<start|prio>:<key>\n<canonical edit>" (caller
// holds towerActMu), and prunes entries older than gaRecentFor.
func (s *server) rememberGA(memo string) {
	m := &s.towerM
	if m.recentGA == nil {
		m.recentGA = map[string]gaRecent{}
	}
	now := time.Now()
	for k, r := range m.recentGA {
		if now.Sub(r.at) >= gaRecentFor {
			delete(m.recentGA, k)
		}
	}
	k, edit, _ := strings.Cut(memo, "\n")
	m.recentGA[k] = gaRecent{at: now, edit: edit}
}
