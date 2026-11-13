# Subagent review carry-overs

Base: `8eea87b`, branch `codex/subagent-s2-followups-1113`. Scope: F4, F7 and resumed-child outcomes from the S0/S1 reviews. No drill-in or UI changes.

- F4: transient per-key locks serialize readers of the same scoped binding/snapshot. The global mutex protects cache maps only; tmux waits, source stats, decoding and summary aging run outside it. Waiting requests can cancel. Entries retain the six-second TTL, stat-only reuse and 128-root limit; active lock keys cannot be evicted and disappear when work ends.
- F7: native resolution tries the live environment, this scoped root's last confirmed native ID, then the list ID, accepting the first ID that resolves to a transcript. Confirmation is never borrowed from the Deck-ID-only reply cache. The shared tmux environment helper now supports named sockets and checks the corresponding live-session set. The existing default-socket callers retain their behavior and refresh cadence remains 60 seconds.
- Resumes: structural session-task notifications may close a linked child by matching task ID even without a verified invocation for their tool-use ID. Such notifications must be no older than verified child activity and strictly newer than the last terminal state. Calls verified for a different child remain rejected. Delivery ordering uses timestamps; resumed outcomes neither invent invocations nor rewrite the original invocation's outcome.

Regression coverage includes six concurrent cold root scans, same-key sharing across twelve readers, cancellation while waiting, an unrelated read/resolver progressing during a tmux wait, exact TTL/stat reuse, resolver precedence and both default/named-socket gone checks. Resume cases cover success/failure/cancellation, foreign task IDs, foreign verified invocations, unjoined Agent calls, older/equal terminal timestamps, and out-of-order deliveries.

All added fixture content is synthetic structure or `[REDACTED]`; existing fixture files are unchanged. No private transcripts were read. No live-service authenticated request, restart, deployment, push, merge, or other-worktree mutation was performed. Browser dependencies were copied from the existing installation into this worktree after matching lockfiles; no installation changed the source checkout.

## Verification (UTC, 2026-11-13)

| Completion | Check | Result |
|---|---|---|
| 22:02:15 | Focused `go test -race -count=1 -run TestSubagents .` | PASS before final resume guard refinement. |
| 22:06:06 | `GOCACHE=/tmp/deck-remote-go-cache make test` | PASS on final implementation: Go suite and 227 JavaScript tests. |
| 22:04–22:06 | New resolver/resume tests with original source supplied through Go overlays | Expected failures: missing-env fallbacks, gone checks, resumed outcomes and terminal ordering. Original cache overlay adds only an unused field so new test code compiles; behavior remains original. |
| 22:06:37 | Package-wide `go test -race -count=1 .` | Unrelated `TestAsk_TypedTransportWhenSupported/old` TempDir cleanup failure (`directory not empty`); no data-race report. The preceding whole-package race run passed at 22:04:34. |
| 22:08:27 | `GOCACHE=/tmp/deck-remote-go-cache go test -race -count=1 .` rerun | PASS on final implementation (42.684 seconds). |
| 22:08:18 | `GOCACHE=/tmp/deck-remote-go-cache make e2e` | 15/21 PASS, including subagents. Unrelated fixture asset connection resets/timeouts affected offline refresh, Tower backlog, persistent-list backoff, 401 recovery, deep-link cold start and Tower reply drafts. |
| 22:11:22 | `GOCACHE=/tmp/deck-remote-go-cache make e2e` (single full rerun) | 20/21 PASS, including subagents and all six initial failures. Sole failure: `phone photos: pick, preview, remove, failed upload, queued send and unsupported harness` (`photos.e2e.mjs:44`), asset `ERR_CONNECTION_RESET` followed by `#photoCamera` timeout. That test passed in the first run. The browser suite is not wholly green; no harness changes or further reruns were made. |

Initial sandboxed full-suite attempts could not bind fixture loopback sockets (`operation not permitted`); approved runs use temporary fixture servers only. Memory recall returned HTTP 500, so implementation relied on repository code and the supplied review reports.

Logs: `/tmp/dr-s2-followups-make-test-final.log`, `/tmp/dr-s2-followups-race-final.log`, `/tmp/dr-s2-followups-race-rerun.log`, `/tmp/dr-s2-followups-before-resolver.log`, `/tmp/dr-s2-followups-before-resume.log`, `/tmp/dr-s2-followups-e2e.log`, `/tmp/dr-s2-followups-e2e-rerun.log`.
