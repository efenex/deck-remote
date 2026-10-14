package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAdeckStdin writes a stub agent-deck that records its argv and stdin and
// prints a delivered status object.
func fakeAdeckStdin(t *testing.T) (bin, argvLog, stdinLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "agent-deck")
	argvLog = filepath.Join(dir, "argv.log")
	stdinLog = filepath.Join(dir, "stdin.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + argvLog + "\n" +
		"cat > " + stdinLog + "\n" +
		"echo '{\"delivery\":\"delivered\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog, stdinLog
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A message starting with "-" must reach agent-deck on stdin, not as a
// positional argument its flag parser would read as a flag.
func TestSendMessageGoesOnStdin(t *testing.T) {
	bin, argvLog, stdinLog := fakeAdeckStdin(t)
	s := testServer(t, config{bin: bin, token: "tok"})
	msg := "-x hello\n--message-file=/etc/passwd"
	if err := s.sendNoWait(context.Background(), "s1", msg); err != nil {
		t.Fatal(err)
	}
	argv := readFile(t, argvLog)
	if strings.Contains(argv, "-x hello") || strings.Contains(argv, "/etc/passwd") {
		t.Errorf("message leaked into argv: %q", argv)
	}
	if !strings.Contains(argv, "session send s1 --message-file - --no-wait --json") {
		t.Errorf("argv = %q", argv)
	}
	if got := readFile(t, stdinLog); got != msg {
		t.Errorf("stdin = %q, want %q", got, msg)
	}
}

// A slash send must not pass --json: a bare --json send is queued in the
// background and returns before anything is typed.
func TestRunSlashIsSynchronous(t *testing.T) {
	bin, argvLog, stdinLog := fakeAdeckStdin(t)
	s := testServer(t, config{bin: bin, token: "tok", homeDir: t.TempDir()})
	s.runSlash(sessionInfo{ID: "s1"}, "/compact", "r1", "default")
	argv := readFile(t, argvLog)
	if strings.Contains(argv, "--json") || strings.Contains(argv, "--no-wait") {
		t.Errorf("slash send must gate and send synchronously; argv = %q", argv)
	}
	if !strings.Contains(argv, "--message-file - --timeout 30s") {
		t.Errorf("argv = %q", argv)
	}
	if got := readFile(t, stdinLog); got != "/compact" {
		t.Errorf("stdin = %q", got)
	}
}
