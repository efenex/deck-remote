# Handoff 2026-10-21 — morning bugs (task deckremote-morning-bugs)

Shipped: origin/main `0bbd253` (2 commits on `3862fac`), deployed to prod 06:04Z
(launchd `io.lunascens.deck-remote`, :8443; `/api/rc/version` server_sha = web_sha = 0bbd253).

## Bug 2 — AskUserQuestion menu read as idle
- Cause: `claudeAdapter.Attention` only recognised permission dialogs (`claudePermissionMarkers`). The menu has a
  `❯ 1.` selector but no marker. Never detected; NOT the tmux budget (`/attention` returned pending:false, not unavailable).
  deck-remote reads agent-deck's `substate` nowhere; the AskUserQuestion tool_use is not a history row.
- Fix (`question.go`): anchor on the footer "Enter to select … Esc to cancel" (last 10 non-blank lines), walk up to the
  `☐ Header`, parse question/options/descriptions. Result: `Kind:"question"`, `ReadOnly:true`, no Actions. ID hashes
  question+labels, not the cursor. Watcher pushes it (claude `question` only; approvals still go via watchPermissionPane).
- Deliberately read-only: no key is sent to a menu; `RespondAttention` requires Kind=approval. Answering from the phone
  (digit/Down+Enter, "Type something", "Chat about this") was NOT built because the key semantics were unverified against a
  live menu and ga-rdb was off-limits. Open follow-up if wanted; test against a scratch session, not an operator one.
- PWA: `renderQuestionReadOnly` card; the composer locks while it is shown (`syncComposerLock`).

## Bug 1 — stale "running" after a night in the background
- Cause: server drops falsy live fields (`omitempty`); `loadSessions` did `Object.assign(prev, fresh)`, so stale
  working/activity/needsAttention never cleared. Restart looked right because `saveDeckCache` stores raw server rows.
- Fix: `DeckThread.mergeSessionRow` (web/thread.js) resets working/stalled/needsAttention/activity/currentTool/state/
  degradedReason; lastReply/lastActivity are kept on purpose (snapshot blips would fold the group). Also: EventSource
  reopened on resume when CLOSED or away >30 s (visibility + pagehide), resync on every SSE reconnect, one 3 s retry.
  SW cache and `?v=` are 25 — bump both when app.js/thread.js change.

## Verification gaps
- Bug 2 is verified on the real ga-rdb pane capture + fixtures only; no live menu seen post-deploy. First real menu: check
  the card and a "question" push. Evidence from before the fix: /tmp/dr-bugs/ (pane.txt, api-*.json, report.md; may be gone).
- Bug 1 is verified by unit test, not on a device after a real overnight.

## Housekeeping
- Worktrees: /Users/rdesmet/src/deck-remote-bugs (branch fix/morning-bugs, merged), this handoff worktree. Remove with
  `git worktree remove` when done. Disk is tight on blackbird (~8 GiB).
- Not done: `omitempty` on the sessionInfo bools was left as is (client merge now handles it).
