# Phone re-delivery R0

Contract: agent-deck `723f4af6`, `docs/redeliver.md` and
`cmd/agent-deck/session_redeliver_cmd.go`. No R1 matcher changes are included.

## Actions and receipts

`POST /api/rc/ask/redeliver` accepts the original phone `requestId`, `sendId`,
`sessionId`, `profile`, and a separate `operationId`. The existing typed-input
authorization remains in force. A tracked phone receipt establishes ownership;
otherwise a read-only status lookup verifies the session before acting.

Each manual operation calls exactly:

```text
agent-deck -p PROFILE session redeliver SEND_ID --request-id OPERATION_ID --json
```

agent-deck owns reconciliation, failed-send reopening, concurrency, idempotency,
and remote forwarding. No new message is created by deck-remote. Unknown local
commands and the owner's `remote_redeliver_unavailable` response become
`unsupported`; neither falls back to the old manual release path.

Before the request, the browser persists receipt IDs and the operation key in
`localStorage`, scoped by profile/session/send. Message content is not stored.
A timeout, failed response, or page reload retains that operation. A reload
restores a delivery-check notice with **Retry check**, without a duplicate message
bubble. A matching HTTP or SSE receipt acknowledges the operation; the next
deliberate tap gets a fresh UUID.
This allows a sticky refusal or `pending/reopen_busy` to be tried again.
Storage failure prevents the action rather than sending without a durable key.
**Start a new check** explicitly replaces an unfinished key, allowing recovery
from deterministic replay errors while still checking the same original send.
Replacement is persisted before posting; a failed write retains the old key.
Malformed or incorrectly scoped slots are ignored during reload and replaced
on the next tap for that send. They cannot hide other valid pending checks.

Reopened sends rejoin the persisted polling set. An operation timestamp does
not suppress a subsequent status read of an older queue-record timestamp.

## Display and reads

| CLI outcome | UI |
| --- | --- |
| `blocked_unknown` | may have landed; not re-sent · check again |
| `unsupported` | unverifiable |
| `already_landed`, `landed` | landed |
| `pending` | in progress, with the reason when provided |
| `conflict`, `not_found`, `refused` | re-delivery unavailable, with a short reason |

A replay's current landed snapshot overrides its older decision. Invalid or
mismatched receipts are uncertain errors; the browser keeps the operation key.
The operator can escape a repeatedly invalid receipt with **Start a new check**.
The same mappings work through both HTTP and SSE.

`GET /api/rc/ask/status` is authenticated but read-capable. Phone-triggered queue
status reads use `send-status --no-resume --json`, including the ownership lookup
and legacy POST `checkOnly` path. These never retry an older CLI without
`--no-resume`. The server's background poller uses plain `send-status --json`,
preserving agent-deck's ability to resume a dead worker for a non-final queued
send after a reboot or kill. It never re-types a typed or settled send.
Known non-row tools (OpenCode, shell, SSH, Pi, OMP) are marked unverifiable;
unknown/custom tool names retain ordinary unconfirmed delivery states until the
owner reports a capability result, since agent-deck resolves Claude/Codex-compatible
custom tools through `rowsHarness`. The already-loaded session list supplies
SSH-placeholder metadata absent from the
queue record. The owner primitive remains the capability authority on a tap.

Transcript reconciliation reserves `landed_row_id` before content matching.
Exact row identity wins over content/timestamp heuristics. A different native ID
cannot match. When IDs are synthesized (`queue:`, `codex:`, `line:`) or missing,
the existing one-for-one, time-bounded content match can reconcile a bubble;
owner-proven delivery stays **landed**. Content matches without owner proof say
**seen (unverified)** and retain the receipt so it can still be checked. A reload
notice has no saved content and can attach to history only by exact landed row ID.
The app-shell cache version is 62.

## Automatic queue nudges

The pre-existing bounded, readiness-gated automatic queue release remains
queue-only. It does not call `redeliver`: the record could become failed after
the queued snapshot, and then `redeliver` could reopen it without a user tap.
This automatic control is not a fallback from a manual operation. Failed or
possibly typed sends are never automatically reopened by deck-remote.

## Verification scope

Fake CLI tests cover every outcome, malformed/mismatched receipts, old local and
remote commands, lost connections, original ID/key arguments, ownership,
read-only authorization, tool/SSH labels, and following reopened sends.
Browser tests cover HTTP/SSE mappings, composer preservation, reload/key reuse,
fresh keys after refusals, and SSE acknowledgement after an HTTP response loss.
Web unit tests cover row-ID precedence and honest content-only attribution.

Tests use temporary directories, fake executables/backends and disposable
browser profiles. No live service, send queue, or installed agent-deck state is
used.

Observed on 2026-11-14 (UTC):

| Time | Check | Result / log |
| --- | --- | --- |
| 08:49Z | `go test -race ./...` (live TUI fixture disabled) | PASS; `/tmp/dr-r0-race.log` |
| 08:53Z | `make e2e` | 22/22 PASS; `/tmp/dr-r0-e2e-backlog.log` |
| 08:54Z | `make test` (live TUI fixture disabled) | Go PASS, web 234/234; `/tmp/dr-r0-final-unit.log` |
| 08:54Z | Affected browser flows after final row-identity changes | 4/4 PASS; `/tmp/dr-r0-final-focused-e2e.log` |
| 08:55Z | Web tests after final diagnostic-copy adjustment | 234/234 PASS; `/tmp/dr-r0-web-verified.log` |

The first full browser run passed 17/22; the requested rerun passed 18/22.
Failures were recovery-fixture startup timeouts, with static-asset connection
resets observed; the exact `DeckPair undefined` message did not appear.
An unmodified export of base `8eea87b` independently reproduced a startup timeout
(`/tmp/dr-r0-base-recovery.log`). The disposable Python mock server's accept
backlog was increased from five to 128; the full suite then passed. This is a
test-fixture mitigation, separate from the R0 implementation, with no change to
production serving or recovery behavior.
