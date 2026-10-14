package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// A push service that never answers must not hold up delivery (and with it
// the watcher sweep) beyond pushHTTPClient's timeout.
func TestSendToTimesOutOnHungPushService(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer hung.Close()
	defer close(release)

	old := pushHTTPClient
	pushHTTPClient = &http.Client{Timeout: 200 * time.Millisecond}
	defer func() { pushHTTPClient = old }()

	pm, err := newPushManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	pm.addSub(&webpush.Subscription{
		Endpoint: hung.URL + "/push/abc",
		Keys: webpush.Keys{
			P256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			Auth:   base64.RawURLEncoding.EncodeToString(auth),
		},
	})

	done := make(chan []sendResult, 1)
	go func() { done <- pm.sendTo(pushPayload{Title: "t", Body: "b"}) }()
	select {
	case res := <-done:
		if len(res) != 1 || res[0].Err == "" {
			t.Fatalf("want one transport error, got %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sendTo blocked on a hung push service")
	}
}
