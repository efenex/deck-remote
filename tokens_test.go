package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTokenMintVerifyRevoke covers the device-token lifecycle: mint returns a
// raw secret that verifies, the raw is never persisted, an unrelated token is
// rejected, persistence survives a reload, and revoke (by id and by name)
// invalidates the token.
func TestTokenMintVerifyRevoke(t *testing.T) {
	dir := t.TempDir()
	ts := newTokenStore(dir)

	// Mint two tokens.
	dt1, raw1, err := ts.mint("phone-a")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	dt2, raw2, err := ts.mint("phone-b")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if raw1 == "" || raw1 == raw2 {
		t.Fatalf("expected distinct non-empty secrets, got %q / %q", raw1, raw2)
	}
	if dt1.ID == dt2.ID {
		t.Fatalf("expected distinct ids, got %q", dt1.ID)
	}

	// Both raw secrets verify; an empty string and a random one do not.
	if !ts.verify(raw1) || !ts.verify(raw2) {
		t.Fatal("freshly minted tokens must verify")
	}
	if ts.verify("") {
		t.Fatal("empty token must never verify")
	}
	if ts.verify(raw1 + "x") {
		t.Fatal("a near-miss token must not verify")
	}

	// The on-disk file must NOT contain the raw secret (hash-only storage) and
	// must be 0600.
	path := filepath.Join(dir, "deck-remote-tokens.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(b), raw1) || strings.Contains(string(b), raw2) {
		t.Fatal("raw secret must never be persisted")
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("store perms = %v, want 0600", fi.Mode().Perm())
	}

	// list() exposes metadata but never the hash/secret.
	for _, m := range ts.list() {
		if _, ok := m["hash"]; ok {
			t.Fatal("list() leaked the hash")
		}
		if _, ok := m["token"]; ok {
			t.Fatal("list() leaked a token")
		}
	}

	// Reload from disk: tokens still verify (persistence works).
	reloaded := newTokenStore(dir)
	if !reloaded.verify(raw1) {
		t.Fatal("token must verify after reload")
	}

	// Revoke by id invalidates token 1 only.
	if removed, err := reloaded.revoke(dt1.ID); err != nil || !removed {
		t.Fatalf("revoke by id: removed=%v err=%v", removed, err)
	}
	if reloaded.verify(raw1) {
		t.Fatal("revoked token must not verify")
	}
	if !reloaded.verify(raw2) {
		t.Fatal("non-revoked token must still verify")
	}

	// Revoke by name invalidates token 2.
	if removed, err := reloaded.revoke("phone-b"); err != nil || !removed {
		t.Fatalf("revoke by name: removed=%v err=%v", removed, err)
	}
	if reloaded.verify(raw2) {
		t.Fatal("revoked-by-name token must not verify")
	}

	// Revoking a missing key is a no-op (no error, removed=false).
	if removed, err := reloaded.revoke("nope"); err != nil || removed {
		t.Fatalf("revoke missing: removed=%v err=%v", removed, err)
	}
}

// TestHashTokenStableAndDistinct guards the pure hash helper used by both store
// and verify (they must never drift).
func TestHashTokenStableAndDistinct(t *testing.T) {
	if hashToken("abc") != hashToken("abc") {
		t.Fatal("hashToken must be deterministic")
	}
	if hashToken("abc") == hashToken("abd") {
		t.Fatal("distinct inputs must hash differently")
	}
	if len(hashToken("abc")) != 64 { // hex SHA-256
		t.Fatalf("hash length = %d, want 64", len(hashToken("abc")))
	}
}
