package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair drops a self-signed cert/key pair for cn at the given paths.
func writePair(t *testing.T, certPath, keyPath, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commonName(t *testing.T, r *certReloader) string {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

// A renewal on disk must be picked up without a restart — that is the whole
// point of the reloader (tailscale cert rewrites the pair every ~90 days).
func TestCertReloaderPicksUpRenewal(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	writePair(t, certPath, keyPath, "old.example.ts.net")

	r, err := newCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %v", err)
	}
	if got := commonName(t, r); got != "old.example.ts.net" {
		t.Fatalf("initial CN = %q, want old.example.ts.net", got)
	}

	writePair(t, certPath, keyPath, "new.example.ts.net")
	// mtime has 1s granularity on some filesystems; force a distinct stamp.
	future := time.Now().Add(2 * time.Second)
	for _, p := range []string{certPath, keyPath} {
		if err := os.Chtimes(p, future, future); err != nil {
			t.Fatal(err)
		}
	}
	if got := commonName(t, r); got != "new.example.ts.net" {
		t.Fatalf("after renewal CN = %q, want new.example.ts.net", got)
	}
}

// A torn write (cert renewed, key not yet) must keep serving the last good
// pair rather than failing every handshake until the writer catches up.
func TestCertReloaderKeepsLastGoodOnTornWrite(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	writePair(t, certPath, keyPath, "good.example.ts.net")
	r, err := newCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("newCertReloader: %v", err)
	}

	if err := os.WriteFile(certPath, []byte("-----BEGIN CERTIFICATE-----\ntorn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatal(err)
	}
	if got := commonName(t, r); got != "good.example.ts.net" {
		t.Fatalf("torn write CN = %q, want the last good good.example.ts.net", got)
	}
}

// A missing pair at startup is fatal-worthy, not a silent plain-HTTP fallback.
func TestCertReloaderMissingPairErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := newCertReloader(filepath.Join(dir, "nope.pem"), filepath.Join(dir, "nope.key")); err == nil {
		t.Fatal("want an error for a missing cert pair, got nil")
	}
}
