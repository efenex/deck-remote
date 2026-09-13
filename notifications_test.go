package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNotificationsRecordFullBodyAndPersist(t *testing.T) {
	dir := t.TempDir()
	pm, err := newPushManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	pm.prefs = pushPrefs{QuietStart: -1, QuietEnd: -1, Approve: true}
	var live []notification
	pm.onNotify = func(n notification) { live = append(live, n) }
	pm.setFocus(true) // foreground: the OS push is suppressed, the record is not

	long := strings.Repeat("word ", 100) // 500 runes, far past the 140-rune wire preview
	pm.send(pushPayload{Title: "t", Body: long, SessionID: "s1", Kind: "approval"})

	got := pm.listNotifs(10, "")
	if len(got) != 1 || got[0].Body != strings.TrimSpace(long) || got[0].SessionID != "s1" || got[0].Kind != "approval" {
		t.Fatalf("inbox entry = %+v", got)
	}
	if len(live) != 1 || live[0].ID != got[0].ID {
		t.Fatalf("live hook = %+v", live)
	}

	reloaded, err := newPushManager(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := reloaded.listNotifs(10, ""); len(r) != 1 || r[0].ID != got[0].ID {
		t.Fatalf("not persisted: %+v", r)
	}
}

func TestNotificationsFilteredKindNotRecorded(t *testing.T) {
	pm, err := newPushManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	pm.prefs = pushPrefs{QuietStart: -1, QuietEnd: -1, Finished: false}
	pm.send(pushPayload{Title: "t", Body: "b", Kind: "reply"})
	if n := len(pm.listNotifs(10, "")); n != 0 {
		t.Fatalf("a kind the user turned off must not reach the inbox, got %d", n)
	}
}

func TestNotificationsTrimOrderAndPaging(t *testing.T) {
	pm, err := newPushManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < notifKeep+5; i++ {
		pm.record(pushPayload{Title: "t", Body: strings.Repeat("x", i+1), Kind: "test"})
	}
	all := pm.listNotifs(notifKeep+50, "")
	if len(all) != notifKeep {
		t.Fatalf("kept %d, want %d", len(all), notifKeep)
	}
	if len(all[0].Body) != notifKeep+5 || len(all[len(all)-1].Body) != 6 {
		t.Fatalf("want newest first and the oldest 5 dropped: first=%d last=%d", len(all[0].Body), len(all[len(all)-1].Body))
	}
	page := pm.listNotifs(3, all[1].ID)
	if len(page) != 3 || page[0].ID != all[2].ID {
		t.Fatalf("before= paging broken: %+v", page)
	}
}

func TestHandleNotifications(t *testing.T) {
	pm, err := newPushManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	pm.record(pushPayload{Title: "a", Body: "first", Kind: "test"})
	pm.record(pushPayload{Title: "b", Body: "second", Kind: "test"})
	s := &server{push: pm}

	w := httptest.NewRecorder()
	s.handleNotifications(w, httptest.NewRequest("GET", "/api/rc/notifications?limit=1", nil))
	var resp struct {
		Notifications []notification `json:"notifications"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil || len(resp.Notifications) != 1 || resp.Notifications[0].Body != "second" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	(&server{}).handleNotifications(w, httptest.NewRequest("GET", "/api/rc/notifications", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"notifications":[]`) {
		t.Fatalf("no push manager: status=%d body=%s", w.Code, w.Body.String())
	}
}
