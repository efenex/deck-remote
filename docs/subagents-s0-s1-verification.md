# S0 + S1 verification and handoff

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
