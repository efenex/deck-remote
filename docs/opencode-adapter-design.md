# OpenCode harness adapter — design

Status: proposal (2026-10-16). Evidence gathered read-only against opencode 1.18.27,
agent-deck fork `~/src/agent-deck-rebase/new`, deck-remote `~/src/deck-remote-work`.

## 1. Current state

- `harness.go:adapterFor` switches on `se.Tool` (`claude`, `codex`); `opencode` falls to
  `genericAdapter`: snapshot `DegradedReason: "unsupported harness"`, no history, all
  controls return `errTerminalOnly`. The phone only gets the terminal view.
- agent-deck (fork) already does a lot for opencode:
  - **Session-id binding**: `Instance.OpenCodeSessionID` (`ses_…`), detected async by
    `queryOpenCodeSession` (HTTP `GET /session` on the managed port, else
    `opencode session list --format json`, or `opencode api session.list` for 2.x).
    Persisted in statedb; also exported to tmux env `OPENCODE_SESSION_ID`
    (`syncOpenCodeSessionEnvironment`).
  - **Launch** (1.x): `opencode [-s <id>] [-m model] [--agent a] --port N`, where N is a
    fresh 127.0.0.1 port (`buildOpenCodeSSEPortFlag`, issue #1614). 2.x rejects
    `--port/-m/--agent`; all TUIs share one host service (`opencode serve --service`).
  - **Status**: `OpenCodeSSEWatcher` (`internal/session/opencode_sse.go`) consumes
    `GET /event` (`session.status` busy/retry/idle, `session.idle`) seeded by
    `GET /session/status`; falls back to pane patterns (`tmux/patterns.go`: "esc interrupt",
    "thinking..."; `hasOpenCodeQuestionUI`: "enter submit"/"esc dismiss").
  - **Output**: `GetLastResponse` has no opencode branch → `getTerminalLastResponse` (pane).
  - **Recall reader** (`internal/recall/reader/opencode.go`) reads the legacy
    `storage/session/...` JSON tree; on this host that tree is gone (migrated to SQLite,
    `storage/` only holds `migration` + `session_diff/`).

## 2. Data sources, ranked

| Rank | Source | Gives | Cost / caveats |
|---|---|---|---|
| 1 | Per-TUI HTTP server (1.x `--port N`, agent-deck launched) | messages, live status, SSE events, **pending permissions/questions**, prompt, abort | Needs port discovery (not exposed by agent-deck). Unauthenticated unless `OPENCODE_SERVER_PASSWORD` set (server logs "server is unsecured"). Gone for custom/fork commands and 2.x. |
| 2 | `~/.local/share/opencode/opencode.db` (SQLite, WAL) | full history, last reply, approximate working state | 13 ms query on a 138 MB DB. No pending approvals (see §4). Schema is drizzle-migrated and churns. |
| 3 | `opencode export <id>` / `session list --format json` | same as DB, stable JSON shape | 1.3 s / 2.2 MB and 0.9 s per call: fine for on-demand History, not pollable. |
| 4 | Pane scraping | busy/prompt/dialog presence | Brittle; only for draft-detection and fallback. |

Verified API surface (OpenAPI from an isolated `opencode serve` with temp `XDG_DATA_HOME`):
`GET /session/status`, `GET /event`, `GET /session/{id}/message?limit&before`,
`POST /session/{id}/prompt_async`, `POST /session/{id}/abort`,
`GET /permission`, `POST /permission/{requestID}/reply {reply: once|always|reject, message?}`,
`GET /question`, `POST /question/{id}/reply {answers:[[label…]]}`, `POST /question/{id}/reject`,
`POST /tui/append-prompt {text}`, `/tui/submit-prompt`, `/tui/clear-prompt`.
`PermissionRequest` = `{id: per…, sessionID, permission, patterns[], metadata, always[], tool{messageID, callID}}`.

DB tables used (1.18.27):
- `session(id, directory, parent_id, title, version, time_updated, time_archived…)`
- `message(id, session_id, time_created, data JSON)`: `data.role`,
  `data.time.{created,completed}`, `data.finish` (`stop|tool-calls|length`), `data.error`.
- `part(id, message_id, session_id, data JSON)`: `type` ∈ text, reasoning, tool
  (`state.status` pending/running/completed/error, `state.input`), step-start/finish, patch.
- `event(aggregate_id, seq, type)` (`message.part.updated.1`, …) and `event_sequence`: a
  cheap change cursor (max seq per session) instead of hashing parts.
- `permission(project_id, action, resource)` = **persisted "always" rules**, not pending
  requests (0 rows here).

## 3. Session resolution

`sessionInfo` comes from `agent-deck list --json`, which does **not** carry
`opencode_session_id` (only `session show --json` and the web list's `opencodeSessionId`
do). Resolution order, mirroring `resolveCodex`:
1. `se.OpenCodeSessionID` (web list / future CLI field) → validate `^ses_` and that the
   row exists in `session`.
2. tmux env `OPENCODE_SESSION_ID` on `se.TmuxSession`.
3. Otherwise degrade (`DegradedReason: "opencode session not bound yet"`). Never guess by
   directory: `show` reported `path: /Users/rdesmet`, and several sessions share a directory.

Port discovery (1.x only): `tmux display -p -t <sess> '#{pane_pid}'` → walk descendants
(`ps -axo pid,ppid,args`) to the `opencode` process → parse `--port N` from argv; confirm
with `lsof -a -p <pid> -iTCP -sTCP:LISTEN`. Then confirm ownership: `GET /session/{id}`
must succeed on that port (guards against reused ports). Cache per (tmux session, pane pid).
Auth: if the pane env has `OPENCODE_SERVER_PASSWORD`, send Basic auth (user
`OPENCODE_SERVER_USERNAME` or `opencode`).

## 4. Per-method mapping

| Method | Primary (port known) | Fallback (DB / pane) |
|---|---|---|
| Capabilities | Structured, History, Replies, Activity, Ask, Queue, Interrupt, Attention, Approvals, Questions; Slash = a few TUI commands via `/tui/execute-command` later | DB-only: History, Replies, Activity (approx); controls off |
| Snapshot | `GET /session/status` (busy/retry → working) + last messages via `GET /session/{id}/message?limit=…`; Attention from `GET /permission` + `GET /question` filtered by sessionID | DB: last assistant message text parts → `LastReply`; `LastActivity` = `message.time_updated`. Working ≈ last assistant msg has no `time.completed` **or** `finish != "stop"` and no error and updated < N min ago (between steps `completed` is set, so `time.completed` alone flickers idle). Sequence = `max(event.seq)` for the session aggregate. |
| History | `GET /session/{id}/message` paginated | DB join message+part ordered by `time_created,id`; map text→reply, tool→tool summary, reasoning hidden. Skip child sessions (`parent_id`) unless asked. |
| Attention | pending `PermissionRequest` → `{Kind: approval, ID: per…, Text: permission + patterns + tool input from part by callID, Actions: approve-once, approve-prefix(=always), decline}`; `QuestionRequest` → `Kind: question` | Pane: `hasOpenCodeQuestionUI` → `Pending, Unavailable` (we know something is waiting but can't answer safely). DB cannot see pending requests at all. |
| Deliver | Idle: `POST /tui/append-prompt` + `/tui/submit-prompt` (TUI shows exactly what was sent). Busy: likely queued by opencode (`session_input.delivery` column suggests native queueing; verify in phase 0). Alternative `prompt_async` bypasses the composer (no TUI draft interaction, but TUI agent/model selection must be passed explicitly). | tmux `send-keys` text + Enter via `agent-deck session send-keys --primary`, after a composer-draft check like `codexComposerHasDraft` ("Ask anything" placeholder). |
| Interrupt | `POST /session/{id}/abort`, then poll `/session/status` until idle (4 s, like codex) | `Escape` (pane "esc interrupt"); verify via DB `finish`/error=`MessageAbortedError`. |
| RespondAttention | Re-fetch `GET /permission`, require same `id`, then `POST /permission/{id}/reply`; questions via `/question/{id}/reply`. Verify cleared via `/permission` and SSE `permission.replied`. | None (terminal only). |

Draft collision: `append-prompt` appends to whatever the desktop user is typing. Before
delivering, pane-check for a non-placeholder composer and reject (as codex does); never
call `/tui/clear-prompt` (destroys the user's draft).

## 5. Attention / approval feasibility

- Pending permissions and questions exist **only in the opencode process** (the `permission`
  table holds saved rules). So approvals require the HTTP server; DB and export can't do it.
- 1.x TUI launched by agent-deck has the server (agent-deck already consumes its `/event`).
  Whether that per-TUI server answers `/permission` and whether the TUI dialog clears on
  an API reply is very likely (same server module; `permission.replied` is an event) but
  **unverified**: the phase-3 gate is a manual test in a scratch agent-deck session.
- `always` writes a persistent project rule; label it clearly ("allow pattern always") and
  maybe hide it behind a confirm, like codex approve-prefix.
- Pane-keystroke approvals (Claude-style "press 1") are possible as a fallback but the
  opencode dialog layout isn't stable; not recommended.

## 6. Risks

- **Schema churn**: 1.18 already moved storage/ → SQLite and added `session_message`,
  `event`, `session_input` (v2 sync). Gate DB reads on `session.version` / the latest
  `__drizzle_migrations.name` against a known-good set; unknown → degrade to API/terminal,
  never misparse. Keep JSON decoding of `data` lenient.
- **WAL reads**: the DB is live WAL (675 KB WAL at rest). Open read-only via
  `file:…?mode=ro` (not `immutable=1`, which ignores the WAL and returns stale data);
  never copy only the `.db`. Read-only connections still need the `-shm` file writable
  dir-wise; handle `SQLITE_BUSY` with a short timeout. deck-remote has no SQLite
  dependency today: choose `modernc.org/sqlite` (pure Go, ~+10 MB binary) or shell out to
  `/usr/bin/sqlite3 -readonly -json` (no new dep, ~10 ms/exec; fine at current poll rates).
- **Port discovery**: ports are allocated bind-and-release, so a dead TUI's port can be
  reused; always verify via `GET /session/{id}`. Server is unauthenticated on loopback —
  same trust boundary as tmux, acceptable.
- **opencode 2.x**: no `--port`; a host-wide service. agent-deck drives it via
  `opencode api <op> --param …`. Its address/lock file is unknown (can't test locally). Make
  the adapter version-aware (reuse agent-deck's `--version` probe idea) and treat 2.x as
  DB/terminal-only until investigated.
- **Session rotation**: in-TUI `/new` changes the session id; agent-deck rebinding is
  heuristic (`findBestOpenCodeSession`). Re-resolve every snapshot; on mismatch with the
  port's active session, degrade rather than act.
- **Sub-agent sessions** (`parent_id`) share the directory and port; filter `/permission`
  by the *bound* session and its children (whether child-session permissions surface in the parent TUI is unverified).

## 7. Phased plan

**Phase 0 — spikes (0.5 d).** In a scratch agent-deck opencode session: confirm per-TUI
server serves `/permission`, `/session/{id}/message`, `/tui/append-prompt`; confirm API
permission reply clears the TUI dialog; measure `sqlite3 -readonly` under live writes.

**Phase 1 — read-only snapshot + history (2–3 d).** `harness_opencode.go` +
`opencode_store.go`: resolution (§3), DB reader (shell-out sqlite3 first), History/LastReply
mapping, approximate working state, Sequence from `event.seq`, version gate, pane fallback
for attention presence (`Unavailable`). Capabilities: History/Replies/Activity only.
Tests with a fixture DB built from a sanitized `opencode export`/schema dump in `testdata/`.

**Phase 2 — deliver + interrupt (2 d).** Port discovery + ownership check, `/session/status`
for authoritative working state (optionally one SSE subscription per live session feeding
`acts` and the watcher), `append-prompt`+`submit-prompt` with draft guard, `abort` with
verification; send-keys fallback when no port. Enable Ask/Queue/Interrupt.

**Phase 3 — approvals + questions (1.5–2 d).** `/permission` and `/question` → attention;
RespondAttention with re-validation; map once/always/reject; push notifications on
`permission.asked`. Enable Attention/Approvals/Questions only when a verified port exists.

Total ≈ 6–8 dev days plus review.

## 8. agent-deck upstream gaps

1. `list --json` omits `opencode_session_id` (only `session show --json` has it; codex has
   `codex_session_id` in list). Add it — removes the tmux-env fallback cost.
2. `OpenCodePort` is in-memory only (json tag on `Instance`, not in storage/statedb or any
   JSON output). Persist and expose `opencode_port` (and liveness) in list/show.
3. `session output` for opencode is pane-scraped (`GetLastResponse` has no opencode branch);
   add a DB/HTTP reader and `transcript_path`-equivalent (`opencode_db` + session id).
4. Recall reader targets the removed `storage/` JSON tree; needs an `opencode.db` reader.
5. 2.x: no documented way for third parties to find the shared service; worth exposing its
   URL in `session show --json` once agent-deck knows it.
