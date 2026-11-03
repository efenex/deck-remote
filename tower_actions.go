package main

// Tower tab parity with scripts/tower_tui.py (2026-10-19).
//
// READ: GET /api/rc/tower/model serves the TUI's own model (sections, folds, split_detail
// blocks, status glyphs/colours) as JSON. scripts/tower_model.py imports tower_tui.py from
// the tower checkout, so the phone cannot drift from the laptop; the result is cached on the
// (overview.json, journal.jsonl) mtimes.
//
// WRITE: POST /api/rc/tower/act {op, id[, kind][, not_before]} behind authAct (act-cap device token +
// Tailscale identity). The ONLY writer is `python3 -I <root>/scripts/journal.py add …` with
// the same argv the TUI uses (tower_tui.Actions); nothing here touches journal.jsonl or
// state/. Every op is validated against the CURRENT model (kind/id must exist there and be
// foldable/closable/pickable), so a stale tab or a hand-made request cannot journal an
// arbitrary record.

import (
	"context"
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
		OpenRequests []string `json:"open_requests"`
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
	Op        string `json:"op"` // fold | unfold | close | pick | dismiss | backlog_start | backlog_defer | backlog_drop
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	NotBefore string `json:"not_before"` // backlog_defer only: YYYY-MM-DDT08:00:00Z
}

// POST /api/rc/tower/act — see the file comment. Gated by authAct.
func (s *server) handleTowerAct(w http.ResponseWriter, r *http.Request) {
	var req towerActReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
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
	var wake bool // last step is a best-effort wake: its failure does not fail the action
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
