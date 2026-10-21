package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #7: the PWA's pages carry the CSP, and no page the policy covers relies on inline script.
func TestCSPOnStaticPages(t *testing.T) {
	s := testServer(t, config{token: "tok", homeDir: t.TempDir(), webDir: "web"})
	h := s.routes()
	for _, path := range []string{"/", "/index.html", "/terminal.html", "/app.js"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "deck.example.ts.net"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		csp := w.Header().Get("Content-Security-Policy")
		for _, want := range []string{"script-src 'self';", "connect-src 'self' wss://deck.example.ts.net", "object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'", "default-src 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", path, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-eval") || strings.Contains(strings.Split(strings.Split(csp, "script-src")[1], ";")[0], "unsafe-inline") {
			t.Errorf("%s: script-src must not allow inline or eval: %q", path, csp)
		}
	}
	// A hostile Host header cannot widen the policy.
	if p := cspPolicy("evil.com; script-src *"); strings.Contains(p, "evil") {
		t.Fatalf("host injected into the policy: %q", p)
	}
	// Inline-script audit: no <script> without src, no on*= handler attribute, no javascript: URL.
	inline := regexp.MustCompile(`<script(\s[^>]*)?>`)
	handler := regexp.MustCompile(`(?i)<[^>]+\son[a-z]+\s*=`)
	files, _ := filepath.Glob("web/*.html")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range inline.FindAllString(string(b), -1) {
			if !strings.Contains(m, "src=") {
				t.Errorf("%s: inline script %q", f, m)
			}
		}
		if handler.Match(b) || strings.Contains(string(b), "javascript:") {
			t.Errorf("%s: inline event handler or javascript: URL", f)
		}
	}
	js, _ := filepath.Glob("web/*.js")
	for _, f := range js {
		if strings.HasSuffix(f, ".test.js") {
			continue
		}
		b, _ := os.ReadFile(f)
		if regexp.MustCompile(`\beval\(|new Function\(|javascript:|\son(click|error|load)=`).Match(b) {
			t.Errorf("%s: eval, new Function, javascript: or an inline handler string", f)
		}
	}
}
