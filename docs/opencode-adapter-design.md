# OpenCode adapter (2.x)

Implemented in `harness_opencode.go` (2026-10-16). OpenCode 1.x (per-TUI
`--port` servers) is not supported.

## Data source

OpenCode 2.x runs one host-wide background service (`opencode service`); every
TUI is a client of it. The service publishes `{url, password, pid, version}` in
`~/.local/state/opencode/service.json` and accepts HTTP Basic auth with user
`opencode`. The adapter uses only that API — no pane parsing, no keystrokes —
so a phone action is reflected in the TUI immediately (the TUI renders service
state; verified for prompts, model switches and permission replies).

| Need | Endpoint |
|---|---|
| messages / history | `GET /api/session/{id}/message` (`order=desc` on the first page only — the service rejects `order` with `cursor`) |
| working | `GET /api/session/active` (id present = running) |
| approvals | `GET /api/session/{id}/permission`, `POST …/permission/{req}/reply {decision: once\|always\|reject}` |
| questions | `GET /api/session/{id}/form`, `POST …/form/{form}/reply {answer: {key: value}}` |
| send | `POST /api/session/{id}/prompt {text, delivery: steer\|queue}` (no delivery when idle) |
| interrupt | `POST /api/session/{id}/interrupt` → `{interrupted}` |

Message model: `user {text}`, `assistant {content[text|tool|reasoning], finish}`
(`tool-calls` → commentary, otherwise reply), `idle {outcome}` (turn end:
succeeded / interrupted / failed). Seq = creation time in ms (stable across
pages, which request correlation needs).

## Session binding

agent-deck's `opencodeSessionId` (web list) wins when set. Otherwise the newest
root session in the instance's directory created since the instance (the
instance id's epoch suffix), excluding sessions bound to other instances; two
unbound instances in one directory with several candidates → degraded
("ambiguous"). Discovered bindings are re-resolved every 30 s so `/new` in the
TUI moves the binding. Paths are compared after resolving symlinks.

## Known gaps

- agent-deck did not bind `opencode_session_id` for a 2.x session started in
  `/tmp/octest` (`/tmp` → `/private/tmp`; its discovery passes the unresolved
  path as the `directory` filter). Not yet checked for a non-symlinked path.
- After an interrupt, prompts queued with `delivery: queue` stay in the
  session inbox ("1 queued" in the TUI) until a later turn completes; a new
  idle send goes first. That is OpenCode's semantics.
- Forms: `external` fields cannot be answered from the phone (error says so);
  `when` conditions are ignored (all non-hidden fields are asked).
- No stall detection (no progress signal finer than whole messages).
- Slash commands: only `/compact` is advertised; others are typed as text.
