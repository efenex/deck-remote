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
// Security: the entire boundary is the network ACL + one bearer token. Bind
// loopback and never bind a public address. Two fronting shapes are supported:
//   - `tailscale serve` (tailnet only) -> plain HTTP on 127.0.0.1;
//   - self-terminated TLS (--tls-cert/--tls-key, a `tailscale cert` pair) on a
//     loopback high port, reached over BOTH the tailnet and the home LAN via a
//     pf rdr from :443. That keeps one origin (the *.ts.net name) whether or
//     not Tailscale is up on the client, which the service worker + Web Push
//     subscription require. See README "Off-tailnet LAN fallback".
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
	"sync"
	"sync/atomic"
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
	tmuxBin      string // tmux binary used only to read the live Codex environment
	homeDir      string // resolved OS home; injectable in tests
	webDir       string // static PWA directory
	pushSubject  string // VAPID "sub" contact URI (mailto:/https:); Apple rejects non-routable subjects
	tlsCert      string // PEM cert (a `tailscale cert` fullchain); empty = plain HTTP behind tailscale serve
	tlsKey       string // PEM private key matching tlsCert
	publicURL    string // fixed tailnet origin (https://host.ts.net); the passkey rpId and only allowed origin (passkeys.go)

	// Optional tower overview tab (tower.go). Empty towerOverview = feature off.
	towerOverview    string // path to tower's state/overview/overview.json
	towerOperator    string // Tailscale login (Tailscale-User-Login) allowed to perform tower actions; empty = actions off
	towerSession     string // agent-deck session (id or title) that receives reply-to-ask messages
	towerRoot        string // tower checkout (journal.py, tower_tui.py); default derived from towerOverview
	towerModelScript string // scripts/tower_model.py; default next to the binary

	docExts  string // comma-separated extensions the doc viewer may read; empty = defaultDocExts
	docRoots string // comma-separated roots the doc viewer may read (doc.go); empty = defaultDocRoots
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
	flag.StringVar(&c.tmuxBin, "tmux-bin", def("DECK_REMOTE_TMUX_BIN", "tmux"), "path to tmux (Codex session metadata resolver)")
	flag.StringVar(&c.webDir, "web", def("DECK_REMOTE_WEB", filepath.Join(filepathDir(), "web")), "static PWA directory")
	flag.StringVar(&c.pushSubject, "push-subject", def("DECK_REMOTE_PUSH_SUBJECT", defaultPushSubject), "VAPID 'sub' contact URI (mailto: or https:); Apple rejects non-routable subjects")
	flag.StringVar(&c.tlsCert, "tls-cert", def("DECK_REMOTE_TLS_CERT", ""), "PEM cert to terminate TLS with (a `tailscale cert` fullchain); empty = plain HTTP behind tailscale serve")
	flag.StringVar(&c.tlsKey, "tls-key", def("DECK_REMOTE_TLS_KEY", ""), "PEM key matching --tls-cert")
	flag.StringVar(&c.publicURL, "public-url", def("DECK_REMOTE_PUBLIC_URL", ""), "phone PWA origin, e.g. https://host.tailnet.ts.net: required for pairing, device enrollment and passkeys; empty disables all three")
	flag.StringVar(&c.towerOverview, "tower-overview", def("DECK_REMOTE_TOWER_OVERVIEW", ""), "path to tower's overview.json; empty = no tower tab")
	flag.StringVar(&c.towerOperator, "tower-operator", def("DECK_REMOTE_TOWER_OPERATOR", ""), "Tailscale login allowed to perform tower actions (reply-to-ask); empty = actions refused")
	flag.StringVar(&c.towerSession, "tower-session", def("DECK_REMOTE_TOWER_SESSION", "tower"), "agent-deck session that receives tower replies")
	flag.StringVar(&c.towerRoot, "tower-root", def("DECK_REMOTE_TOWER_ROOT", ""), "tower checkout holding scripts/journal.py + tower_tui.py (default: derived from --tower-overview)")
	flag.StringVar(&c.towerModelScript, "tower-model-script", def("DECK_REMOTE_TOWER_MODEL_SCRIPT", ""), "path to scripts/tower_model.py (default: next to the binary)")
	flag.StringVar(&c.docRoots, "doc-roots", def("DECK_REMOTE_DOC_ROOTS", ""), "comma-separated directories the doc viewer (GET /api/rc/doc) may read; ~/ expands; default "+defaultDocRoots)
	flag.StringVar(&c.docExts, "doc-exts", def("DECK_REMOTE_DOC_EXTS", ""), "comma-separated file extensions the doc viewer may read; default "+defaultDocExts+" (add .log knowingly: logs carry tokens and env dumps)")
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
	c.homeDir = home
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
	srv.passkeys.startup(srv.tokens)

	// deck-remote-native push + the event watcher that drives it.
	home, _ := os.UserHomeDir()
	pm, err := newPushManager(stateDir(home), cfg.pushSubject)
	if err != nil {
		log.Fatalf("push init: %v", err)
	}
	srv.push = pm
	pm.onNotify = func(n notification) {
		srv.hub.publish(map[string]any{"type": "notification", "notification": n})
	}
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go srv.runWatcher(watchCtx)
	srv.resumeSends()
	go srv.runTowerWatch(watchCtx)
	go srv.warmSessions()
	go srv.warmTower()
	log.Printf("push + watcher started (subject=%s vapid pub %s…)", pm.subject, pm.vapidPub[:min(12, len(pm.vapidPub))])

	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE and the proxied terminal WS are long-lived.
	}

	// --tls-cert/--tls-key are all-or-nothing: a half-configured pair would
	// silently fall back to plain HTTP on a port the phone expects to be HTTPS.
	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		log.Fatalf("--tls-cert and --tls-key must be given together")
	}
	serve := httpSrv.ListenAndServe
	scheme := "http"
	if cfg.tlsCert != "" {
		reloader, err := newCertReloader(cfg.tlsCert, cfg.tlsKey)
		if err != nil {
			log.Fatalf("tls: %v", err)
		}
		httpSrv.TLSConfig = reloader.tlsConfig()
		// Certs come from TLSConfig.GetCertificate, so the file args are empty.
		serve = func() error { return httpSrv.ListenAndServeTLS("", "") }
		scheme = "https"
	}

	go func() {
		log.Printf("deck-remote listening on %s://%s -> agent-deck %s (profile=%s)", scheme, cfg.listen, cfg.agentdeckURL, cfg.profile)
		if err := serve(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	srv.recall.stopAll() // reap the recall follow processes
}

type server struct {
	cfg        config
	proxy      *httputil.ReverseProxy
	hub        *sseHub
	queue      *sessionQueues
	push       *pushManager
	recall     *recallStore   // Claude history from agent-deck recall rows/v2 (recall.go)
	acts       *activityCache // single source of parsed live-activity (watcher writes, endpoints read)
	codex      *codexRolloutStore
	pi         *piTranscriptStore
	oc         *opencodeClient
	attn       *attentionCache
	codexReqs  *codexRequestTracker
	tokens     *tokenStore    // per-device tokens (additive to the shared bearer)
	wsTickets  *wsTicketStore // terminal WebSocket tickets (ws_ticket.go)
	passkeys   *passkeyHub    // WebAuthn credentials, enrol code, run challenges (passkeys.go)
	pairs      *pairHub       // phone pairing QR offers (pair.go)
	tower      *towerStore    // optional tower overview (tower.go); nil = feature off
	towerMu    sync.Mutex
	webVer     webVersionCache                                             // git HEAD of the --web checkout (version.go)
	towerM     towerModel                                                  // cached tower_model.py output (tower_actions.go)
	towerActMu sync.Mutex                                                  // serialises journal writes from the phone
	towerExec  func(ctx context.Context, argv []string) (int, string)      // test seam; nil = exec
	towerLast  map[string]time.Time                                        // reply dedup (tower.go)
	sessBuild  func(ctx context.Context, all bool) ([]byte, string, error) // test seam; nil = buildSessions
	sess       *sessionsCache                                              // stale-while-revalidate session-list bodies (sessions.go)
	lists      *listCache                                                  // TTL-cached session list (agent-deck web HTTP first, CLI fallback)
	views      *viewCache                                                  // TTL-cached TUI view filter per profile (viewfilter.go)
	sends      *sendTracker                                                // queued phone messages being followed to delivery (askqueue.go)
	pulse      *statuslineStore                                            // context/quota records, one sqlite3 read per TTL (statusline.go)
	replies    *replyCache                                                 // per-session last-reply cache (transcript-first, mtime-gated)
	phones     *phoneLog                                                   // phone-sent log; history claims the peer rows that echo it (phonelog.go)

	// Subprocess counters (see the watcher's periodic exec-stats log). The
	// whole point of the polling rework is keeping execAdeck near zero at
	// steady state — these make a regression visible without powermetrics.
	execAdeck atomic.Int64
	execTmux  atomic.Int64
	tmux      *tmuxBudget // pacing, backoff and live-session set for every tmux exec (tmuxbudget.go)

	typedOnce sync.Once // probes `session send --help` once (askqueue.go typedSendFlags)
	typedOK   bool
	imageOK   bool
}

func newServer(cfg config, upstream *url.URL) *server {
	if cfg.homeDir == "" {
		cfg.homeDir, _ = os.UserHomeDir()
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.FlushInterval = -1 // stream immediately (SSE / chunked) instead of buffering
	baseDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		baseDirector(r)
		r.Host = upstream.Host
		// The route is already gated by s.auth (shared OR device token), so
		// agent-deck always gets the shared token, and the phone's own token
		// (header or ?token=, which EventSource/WebSocket must use) never
		// reaches it: agent-deck knows nothing of device tokens.
		r.Header.Set("Authorization", "Bearer "+cfg.token)
		// agent-deck only accepts its own origin on the WebSocket (and on API
		// mutations). sameOrigin has already checked the phone's origin
		// against deck-remote's host, so present agent-deck's.
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", upstream.Scheme+"://"+upstream.Host)
		}
		if q := r.URL.Query(); q.Has("token") || q.Has("ticket") {
			q.Del("token")
			q.Del("ticket")
			r.URL.RawQuery = q.Encode()
		}
	}
	home := cfg.homeDir
	s := &server{
		cfg:       cfg,
		proxy:     proxy,
		hub:       newSSEHub(),
		queue:     newSessionQueues(),
		sends:     newSendTracker(stateDir(home)),
		acts:      newActivityCache(),
		recall:    newRecallStore(),
		tmux:      newTmuxBudget(),
		codex:     newCodexRolloutStore(),
		pi:        newPiTranscriptStore(),
		oc:        newOpencodeClient(),
		attn:      newAttentionCache(),
		codexReqs: newCodexRequestTracker(),
		tokens:    newTokenStore(stateDir(home)),
		wsTickets: newWSTicketStore(),
		passkeys:  newPasskeyHub(stateDir(home)),
		pairs:     newPairHub(),
		tower:     newTowerStore(cfg.towerOverview),
		sess:      newSessionsCache(),
		lists:     newListCache(),
		views:     newViewCache(),
		replies:   newReplyCache(),
		pulse:     newStatuslineStore(cfg.homeDir),
	}
	s.phones = newPhoneLog(home)
	s.hub.tap = s.phones.tap
	return s
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
	mux.Handle("POST /api/rc/ask", s.auth(s.authTyped(http.HandlerFunc(s.handleAsk))))     // typed input (typed.go)
	mux.Handle("POST /api/rc/slash", s.auth(s.authTyped(http.HandlerFunc(s.handleSlash)))) // typed input (typed.go)
	mux.Handle("GET /api/rc/permission", s.auth(http.HandlerFunc(s.handlePermission)))
	mux.Handle("GET /api/rc/activity", s.auth(http.HandlerFunc(s.handleActivity)))
	mux.Handle("POST /api/rc/approve", s.auth(s.authTyped(http.HandlerFunc(s.handleApprove)))) // typed input (typed.go)
	mux.Handle("GET /api/rc/attention", s.auth(http.HandlerFunc(s.handleAttention)))
	mux.Handle("POST /api/rc/attention/respond", s.auth(s.authTyped(http.HandlerFunc(s.handleAttentionRespond)))) // typed input (typed.go)
	mux.Handle("POST /api/rc/interrupt", s.auth(s.authTyped(http.HandlerFunc(s.handleInterrupt))))                // typed input (typed.go)
	mux.Handle("GET /api/rc/events", s.auth(http.HandlerFunc(s.handleEvents)))
	mux.Handle("POST /api/rc/ws/ticket", s.auth(s.authTyped(http.HandlerFunc(s.handleWSTicket)))) // terminal ticket (ws_ticket.go)

	// deck-remote's OWN Web Push (event-driven via the watcher) — the PWA uses
	// these instead of agent-deck's status-driven push.
	mux.Handle("GET /api/rc/push/config", s.auth(http.HandlerFunc(s.handlePushConfig)))
	mux.Handle("POST /api/rc/push/subscribe", s.auth(http.HandlerFunc(s.handlePushSubscribe)))
	mux.Handle("POST /api/rc/push/presence", s.auth(http.HandlerFunc(s.handlePushPresence)))
	mux.Handle("POST /api/rc/push/test", s.auth(http.HandlerFunc(s.handlePushTest)))
	mux.Handle("POST /api/rc/push/notify", s.auth(http.HandlerFunc(s.handlePushNotify)))
	mux.Handle("POST /api/rc/push/prefs", s.auth(http.HandlerFunc(s.handlePushPrefs)))
	mux.Handle("GET /api/rc/notifications", s.auth(http.HandlerFunc(s.handleNotifications)))
	// tower overview tab (tower.go): reads on any token, actions on act-cap device token + tailscale identity
	mux.Handle("GET /api/rc/tower", s.auth(http.HandlerFunc(s.handleTower)))
	mux.Handle("GET /api/rc/tower/whoami", s.auth(http.HandlerFunc(s.handleTowerWhoami)))
	mux.Handle("GET /api/rc/doc", s.authDoc(http.HandlerFunc(s.handleDoc)))
	mux.Handle("GET /api/rc/version", s.auth(http.HandlerFunc(s.handleVersion)))
	mux.Handle("GET /api/rc/tower/model", s.auth(http.HandlerFunc(s.handleTowerModel)))
	mux.Handle("POST /api/rc/tower/act", s.auth(s.authAct(http.HandlerFunc(s.handleTowerAct))))
	mux.Handle("POST /api/rc/tower/reply", s.auth(s.authAct(http.HandlerFunc(s.handleTowerReply))))
	// run-bang phase 1 (tower_run.go): authAct + the run:ro cap, the runner is the only executor
	mux.Handle("POST /api/rc/tower/run", s.auth(s.authAct(http.HandlerFunc(s.handleTowerRun))))
	mux.Handle("GET /api/rc/tower/run/verify", s.auth(s.authAct(http.HandlerFunc(s.handleTowerRunVerify))))
	mux.Handle("GET /api/rc/tower/run/stream", s.auth(s.authAct(http.HandlerFunc(s.handleTowerRunStream))))
	mux.Handle("GET /api/rc/tower/run/log", s.auth(s.authAct(http.HandlerFunc(s.handleTowerRunLog))))
	// run-bang phase 2 #5 passkeys (passkeys.go). The enrol code is laptop-only and goes to a 0600
	// file, never into a response; the rest is the act chain.
	mux.Handle("POST /api/rc/passkey/enrol-code", s.auth(http.HandlerFunc(s.handleEnrolCode)))
	mux.Handle("POST /api/rc/passkey/register/begin", s.auth(s.authAct(http.HandlerFunc(s.handlePasskeyRegisterBegin))))
	mux.Handle("POST /api/rc/passkey/register/finish", s.auth(s.authAct(http.HandlerFunc(s.handlePasskeyRegisterFinish))))
	mux.Handle("GET /api/rc/passkey/list", s.auth(s.authAct(http.HandlerFunc(s.handlePasskeyList))))
	mux.Handle("POST /api/rc/tower/run/challenge", s.auth(s.authAct(http.HandlerFunc(s.handleRunChallenge))))

	// Per-device tokens (additive). "whoami" works with any valid token and
	// reports whether the caller is the shared (admin) token or a device token.
	// mint/list/revoke are admin-only (shared token) via authShared.
	mux.Handle("GET /api/rc/devices/whoami", s.auth(http.HandlerFunc(s.handleWhoami)))
	mux.Handle("GET /api/rc/devices", s.authShared(http.HandlerFunc(s.handleDevicesList)))
	mux.Handle("POST /api/rc/devices/mint", s.authShared(http.HandlerFunc(s.handleDeviceMint)))
	mux.Handle("POST /api/rc/devices/revoke", s.authShared(http.HandlerFunc(s.handleDeviceRevoke)))
	// Phone pairing (pair.go): the laptop makes a single-use QR code; the PWA scans it and redeems
	// it without a bearer (the code is the credential) for a fresh device token.
	mux.Handle("POST /api/rc/devices/pair", s.authShared(http.HandlerFunc(s.handlePairOffer)))
	mux.Handle("POST /api/rc/devices/pair/redeem", sameOrigin(http.HandlerFunc(s.handlePairRedeem)))

	// Everything else agent-deck already serves (session list API, Web Push,
	// /events/menu SSE, /ws/session/* terminal) is reverse-proxied so the phone
	// uses a single same-origin host — required for the service worker + push.
	// NOTE: the proxy targets ONE agent-deck web bound to cfg.proxyProfile, so —
	// unlike the per-call CLI surface above — these routes CANNOT follow the PWA
	// profile selector. The PWA gates the terminal affordance accordingly.
	// Bearer-gated like everything above: the proxy injects the shared token,
	// so an unauthenticated route here would hand any tailnet peer a shell.
	mux.Handle("/api/", s.auth(s.authProxyTyped(sameOrigin(s.proxy))))
	mux.Handle("/events/", s.auth(s.authProxyTyped(sameOrigin(s.proxy))))
	mux.Handle("/ws/", s.authWS(sameOrigin(s.proxy))) // ?ticket= only (ws_ticket.go), never a bearer

	// Serve the manifest with the correct MIME (Go's FileServer doesn't know
	// .webmanifest) so installability is clean.
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFile(w, r, filepath.Join(s.cfg.webDir, "manifest.webmanifest"))
	})

	// Static PWA (the structured client). Unauthenticated shell, like agent-deck.
	// no-cache: revalidate on every load. Without it browsers cache the PWA's
	// files heuristically (from Last-Modified), so a phone could keep running
	// an old page for days after a deploy.
	files := http.FileServer(http.Dir(s.cfg.webDir))
	mux.Handle("/", withCSP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})))

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

// sameOrigin refuses a browser request from another site (cross-site
// WebSocket hijacking, CSRF). The proxy rewrites Origin to agent-deck's own, so
// agent-deck's check no longer sees the browser's; this one stands in for it.
// No Origin (non-browser clients) passes, as it does upstream.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, `{"error":"cross-origin request blocked"}`, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// stateDir holds deck-remote's own state (push keys/subscriptions, inbox,
// device tokens). DECK_REMOTE_STATE_DIR lets a test instance run beside
// production without sharing its push subscriptions.
func stateDir(home string) string {
	if d := os.Getenv("DECK_REMOTE_STATE_DIR"); d != "" {
		return d
	}
	return filepath.Join(home, ".agent-deck")
}
