package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAskRequestProfileDecode confirms the profile field round-trips through the
// askRequest JSON body (and is trimmed by ask() the same way SessionID/Text are).
func TestAskRequestProfileDecode(t *testing.T) {
	var req askRequest
	if err := json.Unmarshal([]byte(`{"sessionId":"s1","text":"hi","profile":"  work  "}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.SessionID != "s1" || req.Text != "hi" {
		t.Fatalf("unexpected decode: %+v", req)
	}
	// ask() trims Profile; mirror that here so the contract is locked.
	if got := strings.TrimSpace(req.Profile); got != "work" {
		t.Errorf("trimmed profile = %q, want %q", got, "work")
	}

	// Absent profile decodes to the empty string (CLI falls back to cfg.profile).
	var req2 askRequest
	if err := json.Unmarshal([]byte(`{"sessionId":"s1","text":"hi"}`), &req2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req2.Profile != "" {
		t.Errorf("absent profile = %q, want empty", req2.Profile)
	}
}

// TestIsPrintingSlash covers the printing-slash allowlist: only /context,
// /cost, /status, /model, /help (case-insensitive, first token) are "printing"
// and thus eligible for pane capture; everything else (including /clear and
// unknown slashes) is treated as non-printing.
func TestIsPrintingSlash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"context", "/context", true},
		{"cost", "/cost", true},
		{"status", "/status", true},
		{"model", "/model", true},
		{"help", "/help", true},
		{"model with arg", "/model sonnet", true},
		{"uppercase", "/CONTEXT", true},
		{"leading/trailing space", "  /cost  ", true},
		{"tab-delimited arg", "/model\tsonnet", true},
		{"clear is non-printing", "/clear", false},
		{"compact is non-printing", "/compact", false},
		{"unknown slash", "/frobnicate", false},
		{"empty", "", false},
		{"no leading slash", "context", false},
		{"prefix only (not exact)", "/contextual", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPrintingSlash(c.in); got != c.want {
				t.Errorf("isPrintingSlash(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
