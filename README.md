# deck-remote

A private, phone-friendly **structured** remote-control for AI coding sessions
managed by [agent-deck](https://github.com/asheshgoplani/agent-deck), reached over
[Tailscale](https://tailscale.com). It is *not* a terminal — it's a one-handed
control surface: a session dashboard that leads with each agent's **real last
reply**, send-a-prompt → get-the-reply (async), slash commands, a **guarded**
permission/question handling, **live "what's it doing now"** (commentary + current
tool / subagents), queue/steer/interrupt controls, and push notifications.

It is **no-fork and CLI-first**: deck-remote shells the stock `agent-deck` CLI
(`list`, `session output|send|send-keys`, `capture-pane`) — it needs no patched
agent-deck and no long-running agent-deck web server. You stay on the
auto-updating stock binary.

> Status: early v0. Claude and Codex 0.144 are structured harnesses. Unknown
> Codex rollout formats and other/custom harnesses degrade to terminal-only.

## Why
Driving a coding agent from your phone usually means either a cloud relay (not
private) or a full terminal (miserable on a phone — broken resize, on-screen
keyboard covers half the screen). deck-remote gives a structured, private
alternative over your own tailnet.

## Architecture
```
 phone PWA ──HTTPS (tailscale serve)──> deck-remote (loopback :8781)
   (installed to Home Screen)             ├─ /api/rc/{sessions,reply,history,activity,attention,ask,slash,interrupt,events,push/*}
   same-origin → service worker + push    │     → shells the stock agent-deck CLI
                                          └─ optional reverse-proxy → an agent-deck web server
                                                (terminal escape-hatch; degrades if none running)
```
- **Single same-origin host** (required for the service worker + Web Push):
  front deck-remote with `tailscale serve` for HTTPS on your `*.ts.net` name.
- **Security boundary = network ACL + one bearer token.** Bind deck-remote to
  loopback and let a front expose it. Never bind a public address.
- **Optional off-tailnet LAN fallback** — keep the same `*.ts.net` origin
  working from your home LAN with Tailscale off; see
  [Off-tailnet LAN fallback](#off-tailnet-lan-fallback).

## Features
- **Detail-first dashboard** — sessions grouped by tree, each leading with its
  real last reply (read from the transcript). agent-deck's status is *not* shown
  (it can be unreliable); see [the operational note](#operational-note).
- **Ask → reply (async)** — send a prompt; the reply streams back over SSE when
  the turn finishes. Busy Codex sessions queue with `Tab` by default; choose
  **Steer current** to inject with `Enter`.
- **Slash commands** — `/clear`, `/compact`, `/context`, … sent via the CLI's
  slash-registration-gated path; shown as a "sent" confirmation (no phantom reply).
- **Guarded attention** — approval controls appear only when deck-remote actually
  detects a permission dialog on the pane, and shows you the real request text
  before confirm. Codex offers approve-once, approve-prefix, and decline;
  `request_user_input` calls render as answerable forms. Every response is
  revalidated against the live request before keys are sent.
- **Interrupt** — hold to send Codex `Esc`; success is reported only after the
  rollout records an abort/idle transition.
- **Live activity** — when an agent is working, see its thinking line
  (e.g. `Channelling… (1m 12s · ↓ 2.1k tokens)`), the in-progress tool, and
  subagent progress (`N/M agents done`), parsed best-effort from the pane.
- **Event-driven Web Push** — deck-remote owns its VAPID keys and pushes on
  *reliable* events (a reply settling, a real permission dialog), not on status.
  Presence-aware; iOS requires the PWA Added to Home Screen (secure context).

## Requirements
- macOS or Linux with [`agent-deck`](https://github.com/asheshgoplani/agent-deck)
  installed and on `PATH`, plus `tmux`.
- Go ≥ 1.23 to build.
- Tailscale, with **HTTPS Certificates + MagicDNS** enabled in the admin console
  (required for iOS Web Push).
- **Run a single agent-deck instance per profile** — see the operational note.

## Build & run (dev)
```sh
go build -o deck-remote .
./deck-remote --listen 127.0.0.1:8781
# token defaults to ~/.agent-deck/web-token; or pass --token / DECK_REMOTE_TOKEN
# open http://127.0.0.1:8781/?token=<token>
```

## Deploy (always-on, HTTPS, push)
1. One-time: enable **HTTPS Certificates + MagicDNS** in the Tailscale admin console.
2. Install a launchd/systemd unit for `deck-remote` (a launchd template is in
   [`deploy/`](deploy/)), then front it with Tailscale:
   ```sh
   ./scripts/cutover.sh        # loads the service + runs `tailscale serve`, prints the phone URL
   ```
3. On the phone (Safari): open the printed URL → **Add to Home Screen** → launch
   from the icon → enable notifications in Settings.

## Off-tailnet LAN fallback

Optional. Lets the phone reach deck-remote at home **without Tailscale running**,
while keeping everything else identical.

The constraint that shapes the design: the service worker and the Web Push
subscription are bound to the **origin**. A separate LAN hostname would be a
second PWA install with a second subscription. So the origin stays
`https://<host>.ts.net` (no port) and only the *path to it* changes:

```
 Tailscale ON    phone ─ MagicDNS ─> 100.x:443 ─pf rdr─> 127.0.0.1:8443 (deck-remote, TLS)
 Tailscale OFF   phone ─ LAN DNS  ─> 192.x:443 ─pf rdr─> 127.0.0.1:8443 (same process)
```

- deck-remote terminates TLS itself (`--tls-cert`/`--tls-key`) with a
  `tailscale cert` pair — a real Let's Encrypt cert for the `*.ts.net` name, so
  there is nothing to trust manually on the phone. `tls.go` re-reads the pair
  when its mtime moves, so the ~90-day renewal needs no restart.
- It keeps listening on **loopback** and stays an unprivileged launchd *agent*;
  `pf` owns the privileged `:443`. The rdr's source tables (tailnet CGNAT range
  + your `/24`) are the access control — anything else finds nothing on `:443`.
- `tailscale serve` is dropped: pf now carries the tailnet path too.

```sh
./scripts/lan-fallback.sh          # cert, launchd, pf anchor, boot daemon (needs sudo)
./scripts/lan-fallback.sh --off    # roll back to tailscale serve
```

The one step it cannot do is on your router: a **DNS override**
`<host>.ts.net → <this Mac's LAN IP>` plus a DHCP reservation, so the name still
resolves when MagicDNS is not answering. The script prints the exact line for
ASUS/Merlin dnsmasq and for Pi-hole/AdGuard.

Web Push already works off-tailnet without any of this — the service worker
renders the notification from the payload and never calls back to the server.
This is only about *opening and using* the app.

Caveat: pf `rdr` does not apply to traffic this Mac originates to itself, so
`curl https://<host>.ts.net/healthz` **from the Mac** will fail. Test locally
with `curl -k https://127.0.0.1:8443/healthz` and from the phone for the real
path.

## Configuration (flags / env)
| flag | env | default |
|---|---|---|
| `--listen` | `DECK_REMOTE_LISTEN` | `127.0.0.1:8781` |
| `--token` | `DECK_REMOTE_TOKEN` | contents of `~/.agent-deck/web-token` |
| `--profile` | `AGENTDECK_PROFILE` | `default` |
| `--proxy-profile` | `DECK_REMOTE_PROXY_PROFILE` | `--profile` |
| `--agentdeck-url` | `DECK_REMOTE_AGENTDECK_URL` | `http://127.0.0.1:8420` (optional web proxy) |
| `--tls-cert` | `DECK_REMOTE_TLS_CERT` | empty (plain HTTP behind `tailscale serve`) |
| `--tls-key` | `DECK_REMOTE_TLS_KEY` | empty; must be set with `--tls-cert` |
| `--bin` | `DECK_REMOTE_BIN` | `agent-deck` |
| `--tmux-bin` | `DECK_REMOTE_TMUX_BIN` | `tmux` |
| `--web` | `DECK_REMOTE_WEB` | `web/` next to the binary |

## Operational note
deck-remote is CLI-first precisely so it needs **no second long-running agent-deck
process**. Do **not** run a headless `agent-deck web` alongside the interactive
TUI on the same profile: two writers to agent-deck's `state.db` corrupt the
registry (stale tmux names, sessions falsely shown as "error", session churn).
Keep `[instances] allow_multiple = false` and run one agent-deck instance per
profile. If you want the in-app terminal escape-hatch, run that single instance
as `agent-deck web` and point `--agentdeck-url` at it.

The PWA's header profile switcher scopes the **structured `/api/rc/*` surface**
(sessions, reply, history, ask, slash, approve) across all agent-deck profiles
via a per-call `-p` flag — safe because each call is an independent stock-CLI
process. The in-app **terminal** and **Web Push** stay bound to the single
proxied `agent-deck web` profile (`--proxy-profile`, default `--profile`); the
terminal affordance is disabled in the PWA whenever a different profile is
selected.

## Upstreaming
deck-remote's `/api/rc/*` endpoints work around one gap in agent-deck: there's no
HTTP endpoint to send input / get a reply / approve. The natural upstream is a
small in-tree PR adding `POST /api/sessions/{id}/message`, `/output`, `/approve`,
and a per-session SSE (matching agent-deck's web/CLI input-parity direction).
deck-remote would then opportunistically use those when present.

## License
[MIT](LICENSE).
