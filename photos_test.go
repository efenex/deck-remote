package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func photoJPEG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func photoRequest(t *testing.T, count int, data []byte, caption string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	for k, v := range map[string]string{"sessionId": "s1", "profile": "work", "text": caption} {
		if err := m.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < count; i++ {
		f, err := m.CreateFormFile("photos", "../../untrusted.exe")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/rc/ask", &b)
	req.Header.Set("Content-Type", m.FormDataContentType())
	return req
}

func photoServer(t *testing.T, tool string, flag bool) (*server, string) {
	t.Helper()
	bin, log, _ := fakeAdeckAskQueue(t)
	dir := os.Getenv("FAKE_ADECK_DIR")
	if flag {
		if err := os.WriteFile(filepath.Join(dir, "help"), []byte("-image value\n-transport string\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	src := readFile(t, bin)
	src = strings.ReplaceAll(src, `"tool":"claude"`, `"tool":"`+tool+`"`)
	// Emulate the CLI copy before the receipt; failure to read a staged file fails the send.
	src = strings.Replace(src, "  msg=$(cat)", `  previous=''
  for arg in "$@"; do
    if [ "$previous" = '--image' ]; then
      cat "$arg" >> "$FAKE_ADECK_DIR/photos.bytes" || exit 1
    fi
    previous="$arg"
  done
  msg=$(cat)`, 1)
	if err := os.WriteFile(bin, []byte(src), 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-n", bin).CombinedOutput(); err != nil {
		t.Fatalf("invalid fake CLI: %s %v", out, err)
	}
	return askTestServer(t, bin), log
}

func checkPhotoCleanup(t *testing.T, log string, count int) {
	t.Helper()
	n := 0
	for _, line := range strings.Split(readFile(t, log), "\n") {
		args := strings.Fields(line)
		for i, arg := range args {
			if arg != "--image" {
				continue
			}
			n++
			path := args[i+1]
			if !strings.HasPrefix(filepath.Base(path), "photo-") || filepath.Ext(path) != ".jpg" {
				t.Fatalf("unsafe name: %s", path)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("upload not cleaned: %s (%v)", path, err)
			}
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("directory not cleaned: %s", path)
			}
		}
	}
	if n != count {
		t.Fatalf("image flags=%d, want %d", n, count)
	}
}

func TestPhotoSendQueuesCopiesAndCleansUp(t *testing.T) {
	for _, tool := range []string{"claude", "gemini"} {
		t.Run(tool, func(t *testing.T) {
			s, log := photoServer(t, tool, true)
			caption := "--not-a-flag\nlook here"
			w := httptest.NewRecorder()
			s.handleAsk(w, photoRequest(t, 2, photoJPEG(t), caption))
			if w.Code != 202 || !strings.Contains(w.Body.String(), `"images":2`) {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			args := readFile(t, log)
			if !strings.Contains(args, "-p work session send s1 --message-file - --queue --transport tmux --image") {
				t.Fatalf("argv: %s", args)
			}
			if strings.Contains(args, "not-a-flag") {
				t.Fatal("caption in argv")
			}
			if got := readFile(t, filepath.Join(os.Getenv("FAKE_ADECK_DIR"), "msgs.log")); got != caption+"\n" {
				t.Fatalf("caption=%q", got)
			}
			got, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_ADECK_DIR"), "photos.bytes"))
			if err != nil || len(got) != 2*len(photoJPEG(t)) {
				t.Fatalf("CLI did not read both staged photos: %v", err)
			}
			checkPhotoCleanup(t, log, 2)
			s.sends.mu.Lock()
			defer s.sends.mu.Unlock()
			for _, send := range s.sends.items {
				if send.Images != 2 || send.Profile != "work" {
					t.Fatalf("tracker=%+v", send)
				}
			}
		})
	}
}

func TestPhotoRefusalsNeverSend(t *testing.T) {
	for _, tc := range []struct {
		name, tool, caption string
		count               int
		data                []byte
		flag                bool
		want                int
	}{
		{"unsupported", "codex", "caption", 1, photoJPEG(t), true, 400},
		{"bad format", "claude", "caption", 1, []byte("<script>not an image</script>"), true, 400},
		{"too many", "claude", "caption", 5, photoJPEG(t), true, 400},
		{"none", "claude", "caption", 0, nil, true, 400},
		{"oversized", "claude", "caption", 1, make([]byte, maxPhotoBytes+1), true, 413},
		{"slash", "claude", "/compact", 1, photoJPEG(t), true, 400},
		{"old CLI", "claude", "caption", 1, photoJPEG(t), false, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, log := photoServer(t, tc.tool, tc.flag)
			w := httptest.NewRecorder()
			s.handleAsk(w, photoRequest(t, tc.count, tc.data, tc.caption))
			if w.Code != tc.want {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			if b, _ := os.ReadFile(log); strings.Contains(string(b), "--queue") {
				t.Fatalf("refused photo was queued: %s", b)
			}
		})
	}
}

func TestPhotoEnqueueFailureCleansUp(t *testing.T) {
	s, log := photoServer(t, "claude", true)
	if err := os.WriteFile(filepath.Join(os.Getenv("FAKE_ADECK_DIR"), "fail"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleAsk(w, photoRequest(t, 1, photoJPEG(t), ""))
	if w.Code != 502 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	checkPhotoCleanup(t, log, 1)
}
