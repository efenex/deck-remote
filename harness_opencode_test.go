package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProjectOpencode(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "opencode", "messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Data []ocMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	var snap harnessSnapshot
	projectOpencode(&snap, page.Data)

	var replies []string
	turnOf := map[string]string{}
	for _, m := range snap.Messages {
		if m.Seq == 0 || m.TurnID == "" {
			t.Fatalf("message without seq/turn: %+v", m)
		}
		if m.Role == "user" {
			turnOf[m.Content] = m.TurnID
		}
		if m.Role == "reply" {
			replies = append(replies, m.Content)
		}
	}
	if len(replies) < 3 || replies[0] != "OC-OK" {
		t.Fatalf("replies = %q", replies)
	}
	// The PHONE-OK reply belongs to the turn its prompt opened.
	for _, m := range snap.Messages {
		if m.Role == "reply" && m.Content == "PHONE-OK" && m.TurnID != turnOf["Use the bash tool to run: echo FROM-PHONE, then reply with exactly: PHONE-OK"] {
			t.Fatalf("PHONE-OK in turn %q", m.TurnID)
		}
	}
	// The fixture ends in a declined tool call (an aborted assistant message),
	// which must not surface as a reply.
	if strings.Contains(snap.LastReply, "declined") || snap.LastReply == "" {
		t.Fatalf("last reply = %q", snap.LastReply)
	}
	for i := 1; i < len(snap.Messages); i++ {
		if snap.Messages[i].Seq < snap.Messages[i-1].Seq {
			t.Fatal("seq not monotonic")
		}
	}
}

func TestOpencodeFormValue(t *testing.T) {
	sel := ocFormField{Key: "k", Type: "string"}
	sel.Options = append(sel.Options, struct {
		Value       string `json:"value"`
		Label       string `json:"label"`
		Description string `json:"description"`
	}{Value: "pg", Label: "Postgres"})
	if v, _ := opencodeFormValue(sel, "postgres"); v != "pg" {
		t.Fatalf("label → value: %v", v)
	}
	if v, _ := opencodeFormValue(sel, "free text"); v != "free text" {
		t.Fatalf("custom text: %v", v)
	}
	if v, _ := opencodeFormValue(ocFormField{Type: "boolean"}, "Yes"); v != true {
		t.Fatalf("boolean: %v", v)
	}
	if _, err := opencodeFormValue(ocFormField{Type: "number"}, "x"); err == nil {
		t.Fatal("number accepted a non-number")
	}
	ms := sel
	ms.Type = "multiselect"
	if v, _ := opencodeFormValue(ms, "Postgres, other"); len(v.([]string)) != 2 || v.([]string)[0] != "pg" {
		t.Fatalf("multiselect: %v", v)
	}
	if _, err := opencodeFormValue(ocFormField{Type: "external"}, "x"); err == nil {
		t.Fatal("external fields need the terminal")
	}
}

func TestInstanceCreated(t *testing.T) {
	if got := instanceCreated("794f558d-1792109210"); got.Unix() != 1792109210 {
		t.Fatalf("got %v", got)
	}
	if !instanceCreated("no-epoch").IsZero() {
		t.Fatal("non-epoch suffix must be zero")
	}
}

// fakeOpencode is a minimal 2.x service: one directory, a session list, a
// pending permission, and recorded POSTs.
type fakeOpencode struct {
	mu       sync.Mutex
	sessions []ocSession
	perms    []ocPermission
	active   bool
	posts    []string
}

func (f *fakeOpencode) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			f.posts = append(f.posts, path+" "+string(body))
			if strings.HasSuffix(path, "/reply") {
				f.perms = nil
			}
			if strings.HasSuffix(path, "/interrupt") {
				json.NewEncoder(w).Encode(map[string]any{"interrupted": f.active})
				return
			}
			w.Write([]byte(`{"data":{}}`))
			return
		}
		switch {
		case path == "/api/session":
			json.NewEncoder(w).Encode(map[string]any{"data": f.sessions})
		case path == "/api/session/active":
			act := map[string]any{}
			if f.active {
				act["ses_b"] = map[string]string{"type": "running"}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": act})
		case strings.HasSuffix(path, "/message"):
			w.Write([]byte(`{"data":[{"id":"m2","type":"assistant","time":{"created":2000},"finish":"stop","content":[{"type":"text","text":"hi"}]},{"id":"m1","type":"user","time":{"created":1000},"text":"hello"}],"cursor":{}}`))
		case strings.HasSuffix(path, "/permission"):
			json.NewEncoder(w).Encode(map[string]any{"data": f.perms})
		case strings.HasSuffix(path, "/form"):
			w.Write([]byte(`{"data":[]}`))
		case strings.HasPrefix(path, "/api/session/"):
			w.Write([]byte(`{"data":{"id":"x"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newOpencodeTestServer(t *testing.T, f *fakeOpencode, sessions []sessionInfo) *server {
	t.Helper()
	ts := httptest.NewServer(f.handler(t))
	t.Cleanup(ts.Close)
	state := filepath.Join(t.TempDir(), "service.json")
	b, _ := json.Marshal(opencodeService{URL: ts.URL, Password: "pw", PID: 1})
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
	oc := newOpencodeClient()
	oc.path = state
	s := &server{oc: oc, lists: newListCache(), acts: newActivityCache()}
	s.lists.m[""] = listCacheEntry{sessions: sessions, at: time.Now().Add(time.Hour)}
	return s
}

func TestOpencodeAdapterBindingAndAttention(t *testing.T) {
	dir := t.TempDir()
	created := time.Now().Add(-time.Minute)
	se := sessionInfo{ID: "abc-" + itoa(created.Unix()), Tool: "opencode", Path: dir}
	f := &fakeOpencode{
		sessions: []ocSession{
			mkOcSession("ses_old", dir, created.Add(-time.Hour)),    // predates the instance
			mkOcSession("ses_b", dir, created.Add(time.Second)),     // ours
			mkOcSession("ses_child", dir, created.Add(time.Second)), // subagent child
		},
		perms:  []ocPermission{{ID: "per_1", Action: "shell", Resources: []string{"rm -rf x"}, Save: []string{"rm *"}}},
		active: true,
	}
	f.sessions[2].ParentID = "ses_b"
	s := newOpencodeTestServer(t, f, []sessionInfo{se})
	a := opencodeAdapter{s: s}
	ctx := context.Background()

	snap, _ := a.Snapshot(ctx, se, "", false)
	if snap.DegradedReason != "" {
		t.Fatal(snap.DegradedReason)
	}
	if !snap.Activity.Working || snap.LastReply != "hi" || len(snap.Messages) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !snap.Attention.Pending || snap.Attention.ID != "per_1" || len(snap.Attention.Actions) != 3 {
		t.Fatalf("attention = %+v", snap.Attention)
	}
	if _, err := a.RespondAttention(ctx, se, attentionResponseRequest{AttentionID: "per_other", Action: "approve-once"}); err == nil {
		t.Fatal("a stale attention id must be refused")
	}
	res, err := a.RespondAttention(ctx, se, attentionResponseRequest{AttentionID: "per_1", Action: "approve-prefix"})
	if err != nil || !res.Responded || !res.Cleared {
		t.Fatalf("respond = %+v, %v", res, err)
	}
	if res, err := a.Deliver(ctx, se, "next", "auto", "", false); err != nil || res.State != "queued" {
		t.Fatalf("deliver while busy = %+v, %v", res, err)
	}
	f.mu.Lock()
	posts := strings.Join(f.posts, "\n")
	f.mu.Unlock()
	for _, want := range []string{`/api/session/ses_b/permission/per_1/reply {"decision":"always"}`, `/api/session/ses_b/prompt {"delivery":"queue","text":"next"}`} {
		if !strings.Contains(posts, want) {
			t.Fatalf("missing POST %s in:\n%s", want, posts)
		}
	}
}

func TestOpencodeAdapterAmbiguousDirectory(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	a1 := sessionInfo{ID: "a-" + itoa(now.Add(-time.Hour).Unix()), Tool: "opencode", Path: dir}
	a2 := sessionInfo{ID: "b-" + itoa(now.Add(-time.Hour).Unix()), Tool: "opencode", Path: dir}
	f := &fakeOpencode{sessions: []ocSession{mkOcSession("ses_1", dir, now), mkOcSession("ses_2", dir, now)}}
	s := newOpencodeTestServer(t, f, []sessionInfo{a1, a2})
	snap, _ := opencodeAdapter{s: s}.Snapshot(context.Background(), a1, "", false)
	if !strings.Contains(snap.DegradedReason, "ambiguous") {
		t.Fatalf("want ambiguity, got %+v", snap)
	}
	// agent-deck's own binding wins.
	a1.OpenCodeSessionID = "ses_1"
	snap, _ = opencodeAdapter{s: s}.Snapshot(context.Background(), a1, "", false)
	if snap.DegradedReason != "" || snap.LastReply != "hi" {
		t.Fatalf("bound snapshot = %+v", snap)
	}
}

// TestOpencodeSingleCandidateWithSameDirInstance: one candidate session is not
// enough to bind while another unbound instance shares the directory, unless
// the session's title or start time ties it to this instance.
func TestOpencodeSingleCandidateWithSameDirInstance(t *testing.T) {
	dir := t.TempDir()
	start := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name      string
		title     string // instance title
		ocTitle   string
		created   time.Time // opencode session creation
		wantBound bool
	}{
		{"unrelated start and title", "work", "other", start.Add(20 * time.Minute), false},
		{"no title, late start", "", "", start.Add(20 * time.Minute), false},
		{"start time matches", "work", "other", start.Add(5 * time.Second), true},
		{"title matches", "work", "Work", start.Add(20 * time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a1 := sessionInfo{ID: "a-" + itoa(start.Unix()), Tool: "opencode", Path: dir, Title: tc.title}
			a2 := sessionInfo{ID: "b-" + itoa(start.Unix()), Tool: "opencode", Path: dir}
			oc := mkOcSession("ses_1", dir, tc.created)
			oc.Title = tc.ocTitle
			f := &fakeOpencode{sessions: []ocSession{oc}}
			s := newOpencodeTestServer(t, f, []sessionInfo{a1, a2})
			snap, _ := opencodeAdapter{s: s}.Snapshot(context.Background(), a1, "", false)
			if tc.wantBound {
				if snap.DegradedReason != "" || snap.LastReply != "hi" {
					t.Fatalf("want bound, got %+v", snap)
				}
			} else if !strings.Contains(snap.DegradedReason, "ambiguous") {
				t.Fatalf("want ambiguity, got %+v", snap)
			}
		})
	}
}

func mkOcSession(id, dir string, created time.Time) ocSession {
	var s ocSession
	s.ID = id
	s.Location.Directory = dir
	s.Time.Created = float64(created.UnixMilli())
	s.Time.Updated = float64(created.UnixMilli())
	return s
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
