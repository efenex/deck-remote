package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeAdeckSend writes a stub agent-deck that prints body on stdout and exits
// with code. It also records its argv so tests can assert --json was passed.
func fakeAdeckSend(t *testing.T, body string, code int) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "agent-deck")
	argvLog = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + argvLog + "\n" +
		"cat <<'EOF'\n" + body + "\nEOF\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

// The --wait path: agent-deck prints its JSON delivery status and THEN the raw
// reply body. adeckSend must hand back only the body.
func TestAdeckSend_WaitPathSplitsStatusFromReplyBody(t *testing.T) {
	body := `{
  "success": true,
  "delivery": "submitted",
  "submitted": true,
  "session_id": "s1"
}
the actual reply text
spanning two lines`
	bin, argvLog := fakeAdeckSend(t, body, 0)
	s := testServer(t, config{bin: bin, token: "tok"})

	out, err := s.adeckSend(context.Background(), "session", "send", "s1", "hi", "--wait")
	if err != nil {
		t.Fatalf("submitted send must succeed: %v", err)
	}
	if out != "the actual reply text\nspanning two lines" {
		t.Fatalf("reply body not split cleanly from the status object: %q", out)
	}
	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "--json") {
		t.Fatalf("adeckSend must pass --json; argv was %q", argv)
	}
}

// `typed` means the bytes reached the pane but the submit was never confirmed.
// It must surface as a retryable failure carrying agent-deck's own message —
// never as a success, and never auto-retried.
func TestAdeckSend_TypedIsRetryableFailure(t *testing.T) {
	body := `{"success":false,"code":"delivery_failed","delivery":"typed","submitted":false,` +
		`"error":"message reached 'deck' but was never confirmed submitted"}`
	bin, _ := fakeAdeckSend(t, body, 1)
	s := testServer(t, config{bin: bin, token: "tok"})

	_, err := s.adeckSend(context.Background(), "session", "send", "s1", "hi", "--no-wait")
	if err == nil {
		t.Fatal("an unconfirmed submit must not be reported as delivered")
	}
	var se *sendError
	if !errors.As(err, &se) {
		t.Fatalf("want a classified *sendError, got %T", err)
	}
	if se.Delivery != "typed" || !se.Retryable {
		t.Fatalf("typed must classify as retryable: %+v", se)
	}
	if !strings.Contains(se.Error(), "never confirmed submitted") {
		t.Fatalf("agent-deck's own message must survive: %q", se.Error())
	}
	if f := sendErrorFields(err); f["retryable"] != true || f["delivery"] != "typed" {
		t.Fatalf("hub fields must carry the classification: %+v", f)
	}
}

// line_too_long is non-retryable: nothing was typed and the same body fails
// identically, so the PWA must not offer a resend.
func TestAdeckSend_LineTooLongIsNotRetryable(t *testing.T) {
	body := `{"success":false,"code":"delivery_failed","delivery":"line_too_long",` +
		`"submitted":false,"error":"message too long for 'deck' to receive as one line"}`
	bin, _ := fakeAdeckSend(t, body, 1)
	s := testServer(t, config{bin: bin, token: "tok"})

	_, err := s.adeckSend(context.Background(), "session", "send", "s1", "hi", "--no-wait")
	var se *sendError
	if !errors.As(err, &se) {
		t.Fatalf("want a classified *sendError, got %v", err)
	}
	if se.Retryable {
		t.Fatal("line_too_long must not be advertised as retryable")
	}
}

// An agent-deck predating the --json delivery object prints no status header.
// The send must still succeed and the whole stream be treated as the body.
func TestAdeckSend_OlderAgentDeckWithoutStatusObject(t *testing.T) {
	bin, _ := fakeAdeckSend(t, "plain reply text", 0)
	s := testServer(t, config{bin: bin, token: "tok"})

	out, err := s.adeckSend(context.Background(), "session", "send", "s1", "hi", "--wait")
	if err != nil {
		t.Fatalf("a status-less older CLI must still succeed: %v", err)
	}
	if out != "plain reply text" {
		t.Fatalf("body should pass through verbatim, got %q", out)
	}
}

// A non-zero exit with no parseable payload (crash, exec failure) must still
// be an error rather than a silent empty reply.
func TestAdeckSend_UnparseableFailureStillErrors(t *testing.T) {
	bin, _ := fakeAdeckSend(t, "panic: something broke", 1)
	s := testServer(t, config{bin: bin, token: "tok"})

	if _, err := s.adeckSend(context.Background(), "session", "send", "s1", "hi"); err == nil {
		t.Fatal("a non-zero exit must never read as a delivered send")
	}
}
