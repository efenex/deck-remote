package main

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// certReloader serves the TLS certificate from disk and picks up renewals
// without a restart. `tailscale cert` writes a Let's Encrypt cert that expires
// every ~90 days and is refreshed by a cron/launchd job (scripts/tls-renew.sh);
// re-reading on an mtime change means the renewal takes effect on the next
// handshake instead of requiring a service bounce.
type certReloader struct {
	certPath string
	keyPath  string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
}

func newCertReloader(certPath, keyPath string) (*certReloader, error) {
	r := &certReloader{certPath: certPath, keyPath: keyPath}
	if _, err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// stamp is the newest mtime across the pair; either file changing counts as a
// renewal (tailscale cert rewrites both).
func (r *certReloader) stamp() (time.Time, error) {
	ci, err := os.Stat(r.certPath)
	if err != nil {
		return time.Time{}, err
	}
	ki, err := os.Stat(r.keyPath)
	if err != nil {
		return time.Time{}, err
	}
	if ki.ModTime().After(ci.ModTime()) {
		return ki.ModTime(), nil
	}
	return ci.ModTime(), nil
}

func (r *certReloader) load() (*tls.Certificate, error) {
	mod, err := r.stamp()
	if err != nil {
		return nil, fmt.Errorf("stat tls pair: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cert != nil && !mod.After(r.modTime) {
		return r.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		// A renewal that is mid-write leaves a torn pair for a moment. Keep
		// serving the cert we already have rather than failing handshakes.
		if r.cert != nil {
			return r.cert, nil
		}
		return nil, fmt.Errorf("load tls pair: %w", err)
	}
	r.cert = &cert
	r.modTime = mod
	return r.cert, nil
}

// GetCertificate is the tls.Config hook; it reloads only when the files moved.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.load()
}

func (r *certReloader) tlsConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: r.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}
