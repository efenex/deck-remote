# S0 + S1 verification and handoff

## Opus review fix round

Recorded 2026-11-13T18:51:04Z (UTC). Fix-round base: `f68fd6a` on `feat/subagent-view-s0s1`; its cancelled-read/cache protection is retained. Changes are confined to this worktree. All new cases use synthetic structure and existing `[REDACTED]` fixtures; no private transcript was read or copied during this round.

- **F1:** `complete` remains the discovery-coverage flag. Fresh available counts display independently, with a partial hint for bounded parent/discovery coverage. A normal 32 KiB child action tail lowers neither coverage nor freshness. Go regressions cover child files >32 KiB and parent files >2 MiB with a recent launch; JavaScript and browser coverage check that partial discovery still displays the running badge and expires locally.
- **F6:** Verified child assistant activity participates in terminal ordering and can reopen the same child without another Agent launch/result. Regression cases cover success/failure/cancel notifications, handback and older finished metadata, subsequent staleness, and a newer completion followed by an older delivery. No additional invocation is invented.
- **F2:** Unlinked terminal metadata retains `finished` and contributes no Unknown count, including after cache aging. The nonterminal unlinked control remains unknown.
- **F3:** Session and activity summaries require `childCapabilities(se).Summary` and a readable parent transcript. Fixture tests cover local supported, remote unsupported, absent-transcript and unbound Claude sessions.
- **F4/F7:** Optional cache-lock and native-resolver changes deferred; existing global ingestion lock and resolver ordering remain. No other fixes or refactors were included.
- Shell, service worker asset URLs/cache name, and recovery test expectation are consistently **v59**.

| UTC completion | Command/check | Result |
|---|---|---|
| 2026-11-13T18:46:43Z | New regression cases against pre-fix implementation | Expected failures reproduced F1/F6/F2/F3. Logs: `/tmp/dr-s0s1fix-red-go.log`, `/tmp/dr-s0s1fix-red-js.log`. |
| 2026-11-13T18:47:27Z | Targeted `go test -run 'TestSubagents' ./...` and `node --test web/subagents.test.js` | PASS after fixes; changed JavaScript syntax checks passed. |
| 2026-11-13T18:47:46Z | `GOCACHE=/tmp/deck-remote-go-cache go test -race -run 'Subagent\|Child\|Claude' ./...` | PASS. Log: `/tmp/dr-s0s1fix-race.log`. |
| 2026-11-13T18:48:42Z | `GOCACHE=/tmp/deck-remote-go-cache make test` | PASS: Go suite and all 227 JavaScript tests. Log: `/tmp/dr-s0s1fix-test-approved.log`. |
| 2026-11-13T18:49:17Z | `git diff --check`; JSON schema parse | PASS. |
| 2026-11-13T18:50:33Z | `GOCACHE=/tmp/deck-remote-go-cache make e2e` | 19/21 PASS, including the partial-count subagent browser regression. Two failures: `phone photos: pick, preview, remove, failed upload, queued send and unsupported harness` (fixture asset connection resets followed by undefined `createStore`); `a 401 keeps the credential and does not retry-loop the list` (fixture startup `waitForFunction` timeout). Log: `/tmp/dr-s0s1fix-e2e-approved.log`. |
| 2026-11-13T18:51:04Z | `GOCACHE=/tmp/deck-remote-go-cache node --test --test-concurrency=1 --test-name-pattern='phone photos:\|a 401 keeps' photos.e2e.mjs recovery.e2e.mjs` (in `e2e/`) | Required single rerun: 2/2 PASS. No photo/recovery behavior changed; recovery's shell-version assertion alone moved to v59. Log: `/tmp/dr-s0s1fix-e2e-rerun.log`. |
| 2026-11-13T18:51:03Z | Final whitespace/protected-file/ancestry checks | PASS: `f68fd6a` remains an ancestor; OpenCode adapter, parent history/reply resolution and Makefile unchanged. |

Initial sandboxed `make test` and `make e2e` attempts were blocked by localhost-bind `EPERM`; approved runs use temporary synthetic fixture servers and local Chrome only. No live-service authenticated API, restart, deployment, other-worktree change, push, merge or rebase was performed. Memory recall was unavailable (HTTP 500); repository/design evidence supplied context.

## Original S0/S1 implementation verification

Recorded 2026-11-13T18:31:01Z (UTC). Base: `e1a2b94c5b26b1855e24362bfecff81c167a9aad` (`origin/main`, Merge pull request #14 from efenex/fix/pwa-resync-1111). Branch: `feat/subagent-view-s0s1`.

## Results

| UTC completion | Command/check | Result |
|---|---|---|
| 2026-11-13T18:16:46Z | Bounded project-local Claude structural check | Parent Agent shape and child sidechain/identity/result keys confirmed; no prompt/output text printed or committed. |
| 2026-11-13T18:31:01Z | `GOCACHE=/tmp/deck-remote-go-cache make test` | PASS: `go test ./...`, all 227 Node tests. Final log: `/tmp/dr-s0s1-final-tests.log`. |
| 2026-11-13T18:30:40Z | `GOCACHE=/tmp/deck-remote-go-cache go test -race ./... -run '^TestSubagents' -count=1` | PASS. Final log: `/tmp/dr-s0s1-final-race.log`. |
| 2026-11-13T18:29:46Z | `GOCACHE=/tmp/deck-remote-go-cache node --test --test-concurrency=1 runbang.e2e.mjs pair.e2e.mjs offline.e2e.mjs recovery.e2e.mjs photos.e2e.mjs composer.e2e.mjs tower-navigation.e2e.mjs tower-draft.e2e.mjs redeliver.e2e.mjs subagents.e2e.mjs` (in `e2e/`) | 20/21 PASS, including the new child-list test. Existing delivered-photo-caption test timed out waiting for history text. Log: `/tmp/dr-s0s1-browser-regression.log`. |
| 2026-11-13T18:30:39Z | `GOCACHE=/tmp/deck-remote-go-cache node --test --test-concurrency=1 photos.e2e.mjs` (in `e2e/`) | Required single rerun: 2/2 PASS. No photo implementation changed. This run observed a history-render timeout, not the separately known `DeckPair undefined` signature. Log: `/tmp/dr-s0s1-photos-rerun.log`. |

The first sandboxed full test/browser attempts could not bind localhost (`EPERM`). Approved reruns used only temporary synthetic fixture servers. Go cache was redirected to `/tmp` because the default cache directory was sandbox-blocked. Browser dependencies were read through a temporary local symlink to the existing checkout's installed package; the symlink was removed and no dependency installation or other-worktree mutation was performed. New scripts were syntax checked before execution. Final whitespace, schema-JSON and protected-file checks precede commit.

## Changed files

Contract/provider/API: `subagents.go`, `subagents_claude.go`, `harness.go`, `harness_claude.go`, `cli.go`, `main.go`, `sessions.go`, `activity.go`.

UI: `web/subagents.js`, `web/app.js`, `web/index.html`, `web/sw.js` (shell version 58).

Tests: `subagents_test.go`, `web/subagents.test.js`, `e2e/subagents.e2e.mjs`, `e2e/recovery.e2e.mjs` (shell-version expectation only), `Makefile` (new browser test included).

Redacted contracts/fixtures: `testdata/subagents/fixture.schema.json`, `testdata/subagents/summary.schema.json`, `testdata/subagents/opencode-2.json`, and `testdata/subagents/claude-2.1/{manifest.json,parent.jsonl,child.jsonl,meta.json,handback.jsonl,completion.jsonl}`.

Documentation: `docs/subagents-s0-s1.md`, this verification file.

## Scope and next work

S0 + S1 only. The child endpoint/list are read-only. OpenCode 2.x child linkage remains unavailable pending `dr-subagent-view-oc2-gate`; the existing adapter is untouched. Parent history filters, root binding filter and controls are untouched. No live-service authenticated requests, restart, deployment, push or merge were performed.

Conservative S1 limitations: 30-second provisional running evidence, 60-second native environment refresh, bounded tail/discovery reads with explicit incompleteness, first-page-only UI, no remote child reads, no legacy Task/progress claims. Parent source changes may reset the snapshot segment/cursor, including appends. Full incremental ingestion and child SSE invalidations are S4.

S2 next: separate launch-anchor projection preserving native parent message/tool/block IDs; verified historical segment membership; paged contained child history; read-only drill-in/back navigation keyed by root/segment/child/invocation; preservation of parent delivery reconciliation and history filtering. See `docs/subagents-s0-s1.md` for the acceptance map and resource limits.
