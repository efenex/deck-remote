// Command deck-remote is a tiny, no-fork remote-control facade for agent-deck.
//
// It sits in front of a running `agent-deck web` server and closes the one gap
// that server has for a phone client: there is no HTTP endpoint to send a
// prompt, get the reply, trigger a slash command, or approve a permission
// prompt (input is WebSocket-only; approve is TUI-only). deck-remote provides
// those as small structured endpoints by shelling the stock agent-deck CLI,
// and reverse-proxies everything else (session list, Web Push, SSE, the
// terminal WebSocket) straight through to agent-deck so the phone talks to a
// single origin. Front it with `tailscale serve` for HTTPS over the tailnet.
//
// Security: the entire boundary is Tailscale + one bearer token. Bind loopback
// and let tailscale serve expose it; never bind a public address.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type config struct {
	listen       string // address deck-remote binds (loopback; tailscale serve fronts it)
	agentdeckURL string // upstream agent-deck web base URL
	token        string // shared bearer: phone -> deck-remote, and deck-remote -> agent-deck
	profile      string // agent-deck profile (default for the CLI surface; per-call override-able)
	proxyProfile string // profile the reverse-proxied agent-deck web is bound to (terminal/push)
	bin          string // path to the agent-deck binary
	webDir       string // static PWA directory
	pushSubject  string // VAPID "sub" contact URI (mailto:/https:); Apple rejects non-routable subjects
}

func loadConfig() config {
	home, _ := os.UserHomeDir()
	def := func(env, fallback string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return fallback
	}
	c := config{}
	flag.StringVar(&c.listen, "listen", def("DECK_REMOTE_LISTEN", "127.0.0.1:8765"), "address to bind (loopback; front with tailscale serve)")
	flag.StringVar(&c.agentdeckURL, "agentdeck-url", def("DECK_REMOTE_AGENTDECK_URL", "http://127.0.0.1:8420"), "upstream agent-deck web base URL")
	flag.StringVar(&c.token, "token", "", "shared bearer token (default: read DECK_REMOTE_TOKEN or ~/.agent-deck/web-token)")
	flag.StringVar(&c.profile, "profile", def("AGENTDECK_PROFILE", "default"), "agent-deck profile (default for the CLI surface)")
	flag.StringVar(&c.proxyProfile, "proxy-profile", def("DECK_REMOTE_PROXY_PROFILE", ""), "profile the reverse-proxied agent-deck web is bound to (default = --profile); the in-app terminal + push are scoped to it")
	flag.StringVar(&c.bin, "bin", def("DECK_REMOTE_BIN", "agent-deck"), "path to the agent-deck binary")
	flag.StringVar(&c.webDir, "web", def("DECK_REMOTE_WEB", filepath.Join(filepathDir(), "web")), "static PWA directory")
	flag.StringVar(&c.pushSubject, "push-subject", def("DECK_REMOTE_PUSH_SUBJECT", defaultPushSubject), "VAPID 'sub' contact URI (mailto: or https:); Apple rejects non-routable subjects")
	flag.Parse()

	if c.token == "" {
		c.token = os.Getenv("DECK_REMOTE_TOKEN")
	}
	if c.token == "" && home != "" {
		if b, err := os.ReadFile(filepath.Join(home, ".agent-deck", "web-token")); err == nil {
			c.token = strings.TrimSpace(string(b))
		}
	}
	// The reverse-proxied agent-deck web (the /api/*, /events/*, /ws/* terminal
	// targets) is a single instance bound to ONE profile (running a 2nd writer
	// corrupts the registry). Assume it serves the daemon's default profile
	// unless explicitly told otherwise.
	if c.proxyProfile == "" {
		c.proxyProfile = c.profile
	}
	return c
}

// filepathDir returns the directory of the running executable (so the default
// web dir is next to the binary in dev runs from the repo root).
func filepathDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func main() {
	cfg := loadConfig()
	if cfg.token == "" {
		log.Fatal("no token: set --token, DECK_REMOTE_TOKEN, or ~/.agent-deck/web-token")
	}
	up, err := url.Parse(cfg.agentdeckURL)
	if err != nil {
		log.Fatalf("bad --agentdeck-url %q: %v", cfg.agentdeckURL, err)
	}

	srv := newServer(cfg, up)

	// deck-remote-native push + the event watcher that drives it.
	home, _ := os.UserHomeDir()
	pm, err := newPushManager(filepath.Join(home, ".agent-deck"), cfg.pushSubject)
	if err != nil {
		log.Fatalf("push init: %v", err)
	}
	srv.push = pm
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go srv.runWatcher(watchCtx)
	log.Printf("push + watcher started (subject=%s vapid pub %s…)", pm.subject, pm.vapidPub[:min(12, len(pm.vapidPub))])

	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE and the proxied terminal WS are long-lived.
	}

	go func() {
		log.Printf("deck-remote listening on %s -> agent-deck %s (profile=%s)", cfg.listen, cfg.agentdeckURL, cfg.profile)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

type server struct {
	cfg    config
	proxy  *httputil.ReverseProxy
	hub    *sseHub
	queue  *sessionQueues
	push   *pushManager
	acts   *activityCache // single source of parsed live-activity (watcher writes, endpoints read)
	tokens *tokenStore    // per-device tokens (additive to the shared bearer)
}

func newServer(cfg config, upstream *url.URL) *server {
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.FlushInterval = -1 // stream immediately (SSE / chunked) instead of buffering
	baseDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		baseDirector(r)
		r.Host = upstream.Host
		// Ensure agent-deck sees the shared token even if the phone passed it
		// only as a query param (e.g. EventSource / WebSocket can't set headers).
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+cfg.token)
		}
	}
	home, _ := os.UserHomeDir()
	return &server{
		cfg:    cfg,
		proxy:  proxy,
		hub:    newSSEHub(),
		queue:  newSessionQueues(),
		acts:   newActivityCache(),
		tokens: newTokenStore(filepath.Join(home, ".agent-deck")),
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	// Health check (no auth) — used by launchd / tailscale serve probes.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// deck-remote's own structured endpoints (the gap-closers). Bearer-gated.
	mux.Handle("GET /api/rc/profiles", s.auth(http.HandlerFunc(s.handleProfiles)))
	mux.Handle("GET /api/rc/sessions", s.auth(http.HandlerFunc(s.handleSessions)))
	mux.Handle("GET /api/rc/reply", s.auth(http.HandlerFunc(s.handleReply)))
	mux.Handle("GET /api/rc/history", s.auth(http.HandlerFunc(s.handleHistory)))
	mux.Handle("GET /api/rc/status", s.auth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("POST /api/rc/ask", s.auth(http.HandlerFunc(s.handleAsk)))
	mux.Handle("POST /api/rc/slash", s.auth(http.HandlerFunc(s.handleSlash)))
	mux.Handle("GET /api/rc/permission", s.auth(http.HandlerFunc(s.handlePermission)))
	mux.Handle("GET /api/rc/activity", s.auth(http.HandlerFunc(s.handleActivity)))
	mux.Handle("POST /api/rc/approve", s.auth(http.HandlerFunc(s.handleApprove)))
	mux.Handle("GET /api/rc/events", s.auth(http.HandlerFunc(s.handleEvents)))

	// deck-remote's OWN Web Push (event-driven via the watcher) — the PWA uses
	// these instead of agent-deck's status-driven push.
	mux.Handle("GET /api/rc/push/config", s.auth(http.HandlerFunc(s.handlePushConfig)))
	mux.Handle("POST /api/rc/push/subscribe", s.auth(http.HandlerFunc(s.handlePushSubscribe)))
	mux.Handle("POST /api/rc/push/presence", s.auth(http.HandlerFunc(s.handlePushPresence)))
	mux.Handle("POST /api/rc/push/test", s.auth(http.HandlerFunc(s.handlePushTest)))
	mux.Handle("POST /api/rc/push/prefs", s.auth(http.HandlerFunc(s.handlePushPrefs)))

	// Per-device tokens (additive). "whoami" works with any valid token and
	// reports whether the caller is the shared (admin) token or a device token.
	// mint/list/revoke are admin-only (shared token) via authShared.
	mux.Handle("GET /api/rc/devices/whoami", s.auth(http.HandlerFunc(s.handleWhoami)))
	mux.Handle("GET /api/rc/devices", s.authShared(http.HandlerFunc(s.handleDevicesList)))
	mux.Handle("POST /api/rc/devices/mint", s.authShared(http.HandlerFunc(s.handleDeviceMint)))
	mux.Handle("POST /api/rc/devices/revoke", s.authShared(http.HandlerFunc(s.handleDeviceRevoke)))

	// Everything else agent-deck already serves (session list API, Web Push,
	// /events/menu SSE, /ws/session/* terminal) is reverse-proxied so the phone
	// uses a single same-origin host — required for the service worker + push.
	// NOTE: the proxy targets ONE agent-deck web bound to cfg.proxyProfile, so —
	// unlike the per-call CLI surface above — these routes CANNOT follow the PWA
	// profile selector. The PWA gates the terminal affordance accordingly.
	mux.Handle("/api/", s.proxy)
	mux.Handle("/events/", s.proxy)
	mux.Handle("/ws/", s.proxy)

	// Serve the manifest with the correct MIME (Go's FileServer doesn't know
	// .webmanifest) so installability is clean.
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFile(w, r, filepath.Join(s.cfg.webDir, "manifest.webmanifest"))
	})

	// Static PWA (the structured client). Unauthenticated shell, like agent-deck.
	mux.Handle("/", http.FileServer(http.Dir(s.cfg.webDir)))

	return logRequests(mux)
}

// bearer extracts the presented token from the Authorization header or the
// ?token= query param (EventSource / WebSocket can't set headers).
func bearer(r *http.Request) string {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	return tok
}

// auth enforces a bearer token on deck-remote's own endpoints: the legacy
// SHARED token OR any minted per-device token. Both compares are constant-time
// (subtleConstantEq / tokenStore.verify) and tokens are NEVER logged.
func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if subtleConstantEq(tok, s.cfg.token) || s.tokens.verify(tok) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	})
}

// authShared gates the device-token ADMIN endpoints (mint/list/revoke). Only the
// shared token may administer device tokens — a device token cannot mint or
// revoke peers, so a single leaked device secret can't escalate.
func (s *server) authShared(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtleConstantEq(bearer(r), s.cfg.token) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, `{"error":"admin token required"}`, http.StatusForbidden)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
