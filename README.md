# deck-remote

A private, phone-friendly **structured** remote-control for AI coding sessions
managed by [agent-deck](https://github.com/asheshgoplani/agent-deck), reached over
[Tailscale](https://tailscale.com). It is *not* a terminal — it's a one-handed
control surface: a session dashboard that leads with each agent's **real last
reply**, send-a-prompt → get-the-reply (async), slash commands, a **guarded**
permission-approve, **live "what's it doing now"** (thinking line + current tool /
subagents), and push notifications.

It is **no-fork and CLI-first**: deck-remote shells the stock `agent-deck` CLI
(`list`, `session output|send|send-keys`, `capture-pane`) — it needs no patched
agent-deck and no long-running agent-deck web server. You stay on the
auto-updating stock binary.

> Status: early v0. Claude is the first-class harness for reply-capture / approve;
> session listing + activity work across harnesses on a best-effort basis.

## Why
Driving a coding agent from your phone usually means either a cloud relay (not
private) or a full terminal (miserable on a phone — broken resize, on-screen
keyboard covers half the screen). deck-remote gives a structured, private
alternative over your own tailnet.

## Architecture
```
 phone PWA ──HTTPS (tailscale serve)──> deck-remote (loopback :8781)
   (installed to Home Screen)             ├─ /api/rc/{sessions,reply,activity,permission,ask,slash,approve,events,push/*}
   same-origin → service worker + push    │     → shells the stock agent-deck CLI
                                          └─ optional reverse-proxy → an agent-deck web server
                                                (terminal escape-hatch; degrades if none running)
```
- **Single same-origin host** (required for the service worker + Web Push):
  front deck-remote with `tailscale serve` for HTTPS on your `*.ts.net` name.
- **Security boundary = Tailscale ACL + one bearer token.** Bind deck-remote to
  loopback and let `tailscale serve` expose it on the tailnet. Never bind a
  public address.

## Features
- **Detail-first dashboard** — sessions grouped by tree, each leading with its
  real last reply (read from the transcript). agent-deck's status is *not* shown
  (it can be unreliable); see [the operational note](#operational-note).
- **Ask → reply (async)** — send a prompt; the reply streams back over SSE when
  the turn finishes (handles multi-minute turns).
- **Slash commands** — `/clear`, `/compact`, `/context`, … sent via the CLI's
  slash-registration-gated path; shown as a "sent" confirmation (no phantom reply).
- **Guarded approve** — an Approve control appears only when deck-remote actually
  detects a permission dialog on the pane, and shows you the real request text
  before a press-and-hold confirm. Never driven by status. (Claude only.)
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

## Configuration (flags / env)
| flag | env | default |
|---|---|---|
| `--listen` | `DECK_REMOTE_LISTEN` | `127.0.0.1:8781` |
| `--token` | `DECK_REMOTE_TOKEN` | contents of `~/.agent-deck/web-token` |
| `--profile` | `AGENTDECK_PROFILE` | `default` |
| `--agentdeck-url` | `DECK_REMOTE_AGENTDECK_URL` | `http://127.0.0.1:8420` (optional web proxy) |
| `--bin` | `DECK_REMOTE_BIN` | `agent-deck` |
| `--web` | `DECK_REMOTE_WEB` | `web/` next to the binary |

## Operational note
deck-remote is CLI-first precisely so it needs **no second long-running agent-deck
process**. Do **not** run a headless `agent-deck web` alongside the interactive
TUI on the same profile: two writers to agent-deck's `state.db` corrupt the
registry (stale tmux names, sessions falsely shown as "error", session churn).
Keep `[instances] allow_multiple = false` and run one agent-deck instance per
profile. If you want the in-app terminal escape-hatch, run that single instance
as `agent-deck web` and point `--agentdeck-url` at it.

## Upstreaming
deck-remote's `/api/rc/*` endpoints work around one gap in agent-deck: there's no
HTTP endpoint to send input / get a reply / approve. The natural upstream is a
small in-tree PR adding `POST /api/sessions/{id}/message`, `/output`, `/approve`,
and a per-session SSE (matching agent-deck's web/CLI input-parity direction).
deck-remote would then opportunistically use those when present.

## License
[MIT](LICENSE).
