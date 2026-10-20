# Session store: decoupling deck-remote from Claude Code transcripts (design, 2026-10-20)

Task `deckremote-session-store-design`. Repo HEAD 2ebfe30. Evidence gathered 2026-10-20T17:25Z–18:25Z by reading code, and with read-only probes of local files and of the running agent-deck binary. The recall probe used a throw-away `HOME` under /tmp, so no live config was touched. No code was changed and nothing was restarted. Input: `tower/reports/deckremote-input-history-2026-10-20.md` (below: "the input report").

## TL;DR

- **The store the operator described already exists, unused, inside the agent-deck binary we run.** The live binary is `v1.16.28+fork.3c4ef685`, built from `/Users/rdesmet/src/agent-deck-rebase/stealguard`, branch `local/deck-remote-support-2026-10-20`. It ships `agent-deck recall timeline|follow`: a typed per-session row stream (schema `agent-deck.recall.rows/v2`) for Claude, Codex, pi, Gemini, OpenCode and Hermes, with stable native ids, `since`/`before` cursors, `updates`/`removed` frames, queued-message handling and forwarding to remote hosts over ssh. agent-deck's `state.db` also records every native conversation id a session has had (`session_links`). For tower that is 6 ids across `/clear`, two more than the input report's sibling heuristic found. It is switched off (`[recall]` is not enabled), so nothing uses it. §1.4.
- **Half of the rest exists in deck-remote too.** `harnessAdapter` (harness.go:69-77) is the adapter boundary and `histMsg` (history.go:47-57) the common row. What is missing: ids and kinds on the wire, a cursor, push, and history across `/clear`.
- **The dropped-input bug is a parser problem under every option.** recall handles mid-turn (queued) prompts and slash commands, but it files peer messages as `system` or plain `user`, leaves `!` bash rows raw, and emits 2.2k `other` rows on a 108 MB transcript (measured, §1.4). Someone has to classify those, either in the fork or in deck-remote.
- **Recommendation (option F): deck-remote keeps a thin turn API of its own, and its turn model is a projection of recall rows/v2.** Claude and Codex are ingested through `recall` (local, and remote via `agent-deck remote HOST recall …`). The parsers deck-remote has today stay as the fallback for opencode 2.x, pi, recall switched off, and remotes without the fork. Persist only what has no native source: rows the phone sent. Building a new store in agent-deck (C1) or a full store in deck-remote (A) duplicates work that is already done.
- **First slice this week:** rows/v2-shaped `id`/`kind`/`queued` on `/api/rc/history`, plus the Claude `queued_command` parse. It needs no operator gate and no new dependency, and it makes the later switch to recall a change of source, not of schema. **Before anything is dispatched**, tower needs to know that fork-side deck-remote work already exists on that branch (§6.0).

## 1. Current state

### 1.1 The adapter boundary that exists

| piece | where | what it does |
|---|---|---|
| adapter interface | harness.go:69-77 | `Capabilities / Snapshot / History / Attention / Deliver / Interrupt / RespondAttention` |
| adapter dispatch | harness.go:117-132 | by `se.Tool`: claude, codex, pi, omp, opencode, generic |
| capability flags | harness.go:11-25 | the PWA hides controls a harness cannot do safely |
| row type | history.go:47-57 | `Role` ∈ user, reply, commentary, bash; `Content`, `Ts`, `Stdout/Stderr`. **`TurnID` and `Seq` are computed but hidden (`json:"-"`, history.go:55-56)** |
| history endpoint | history.go:466-505 | calls `adapter.History()` and gets the **full** list back; the window is `limit` rows ending `offset` rows **from the newest** (history.go:492-499) |
| live push | sse.go:43-56, 60+ | one hub, fire-and-forget (`default:` drops the event when the client buffer is full, sse.go:50-52); no event ids, no `Last-Event-ID` resume |
| watcher | watcher.go:17 (`watchInterval = 6s`), :86 sweep | pushes settled replies, approvals and stalls only (input report §2); there is no "new transcript row" event |

### 1.2 How history and live sync work, per harness

| harness | source of truth | how it is read | cache / incremental | ids and order | known gaps |
|---|---|---|---|---|---|
| **claude** | `~/.claude/projects/<dir>/<uuid>.jsonl` | `claudeAdapter.History` → `claudeReply` (to get the session id) → `findTranscript` → `parseTranscript` (harness_claude.go:47-56) | `parsedEntry{size,mtime,off,msgs}` parses only the appended tail (history.go:189-273); a cheap byte prefilter skips tool_result lines (history.go:292-297) | line order. Every line has `uuid` and `parentUuid` (checked on tower's newest file); the parser throws both away | mid-turn `queued_command`, `isMeta` peer messages, slash commands and image+text prompts are dropped (history.go:333, :336-349, :73-77, :444-447; input report §1). It reads **one** file: history stops at `/clear` |
| **codex** | `~/.codex/sessions/*/*/*/rollout-*<id>*.jsonl` (multiple segments) | `resolveCodex`: agent-deck metadata, then tmux env, then the hook `.sid` sidecar (codex_rollout.go:377-435); segments are checked against their metadata and sorted (:479-507) | `codexParseState`: offsets, tails and **checkpoints** per segment; it rebuilds cleanly on truncation, replacement or reordering (codex_rollout.go:34-52, 95-147) | `TurnID` from `turn_started`, a running `seq` (codex_rollout.go:232-271) | the most robust parser we have. Its mid-turn behaviour is untested (input report §3) |
| **pi / omp** | newest `*.jsonl` in `~/.<pi\|omp>/agent-deck/<instance-id>/` (harness_pi.go:15-19, 44-66) | full reparse whenever size or mtime changes; refuses files over 64 MB (harness_pi.go:318-360) | size/mtime cache only, no tail parse | entries form a **tree** (`id`/`parentId`); the active branch runs from the last appended leaf back to the root (harness_pi.go:387-438). `Seq` is the position on that branch (:452), so **it changes when the branch changes** | `/new` starts a new file, and history stops there |
| **opencode 2.x** | the host-wide `opencode service` HTTP API (harness_opencode.go:20-29) | `History` pages `/api/session/{id}/message` up to **4000 messages on every call** (harness_opencode.go:454-460, 305-336) | **none**. The PWA reconciles every 3 s per open sheet, so a long session costs up to 20 API pages every 3 s | `Seq` = message creation time in ms, `TurnID` = id of the opening user message (harness_opencode.go:379-431) | the session binding is re-resolved after a TTL so that `/new` takes over (harness_opencode.go:55-58, 207-266) |

**Web client** (input report §2; anchors re-checked at 2ebfe30): open-load `loadHistory` (app.js:1633) fetches a 30-row page. Live sync is `reconcileTranscriptTail` (app.js:1734), which polls `GET /api/rc/history?limit=12&offset=0` every `ACTIVITY_MS=3000` (app.js:175, 179, 1549). Rows are matched by `DeckThread.turnKey` multiset (role, kind, trimmed content), not by id (thread.js:12, app.js:1764-1765). `mergeFreshHistory` replaces `hist:true` rows and keeps live rows newer than `SAME_TURN_SECS=120` (thread.js:41-64). SSE (`new EventSource('/api/rc/events')`, app.js:2198) carries only replies to turns the phone started, plus approval and ask state.

**Consequences of this model:**
1. *Offset paging from the newest row is unstable.* When rows arrive between two scroll-back fetches, `offset` shifts. Content-key dedup hides most of the damage, but two identical short rows (two "ok"s) can merge or double. Stable ids plus a `before=`/`since=` cursor fix this. Every option below needs them.
2. *Every request recomputes the whole list.* That is cheap for claude, codex and pi (all cached) and expensive for opencode (no cache).
3. *Rows the phone sent mid-turn live only in browser memory* until the transcript produces a twin. For Claude, a mid-turn prompt never produces one, so the row disappears on reload (input report §2).

More detail (survey 17:5xZ, spot-checked):
- Rows carry no ids on the wire. The client already works around offset drift: `trimOverlap` exists because "turns appended since the last page shift the window" (thread.js:66-80, used by `loadOlderHistory` app.js:1679-1722).
- The `reply` SSE row does not store its `requestId` (app.js:2235-2250).
- SSE gives each client a 16-slot channel and drops events when it is full (sse.go:25, 43-56). It sends only `data:`, with no `id:` and no replay. After a reconnect, only the 3 s poll repairs gaps.
- For Claude, a turn not started from the PWA reaches the thread **only** through the poll. The watcher emits hub `reply` events for snapshot harnesses only (watcher.go:136-143).

### 1.3 What each harness natively offers

| harness | durable files | API / push | hooks | notes |
|---|---|---|---|---|
| claude | JSONL per conversation, append-only, `uuid`/`parentUuid` per line (tree: rewind/edit can branch, *not verified here*) | none for a TUI session | SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Notification, PermissionRequest, PreCompact, Stop, SessionEnd are all wired to `agent-deck hook-handler` in `~/.claude/settings.json` (read 17:3xZ). The payloads carry the prompt text and `transcript_path` | agent-deck keeps only status: `~/.agent-deck/hooks/<id>.events.jsonl` lines are `{at,event,status,session_id,pid}` with **no content**, and the file is a rolling buffer (tower's holds 200 lines, back to 14:21Z, so it **does not** preserve the `/clear` chain of session ids, but `session_links` does: §1.4) |
| codex | rollout JSONL segments, with `event_msg` + `response_item` | (codex has an app-server/MCP mode; not used under agent-deck tmux) | agent-deck writes a `.sid` sidecar (codex_rollout.go:428) | turn-structured already (`turn_started`/`turn_complete`) |
| pi / omp | JSONL tree per session file | none used (pi has an RPC mode; not used under agent-deck) | none | branches are first-class: the store has to support "branch switched" |
| opencode 2.x | `~/.local/share/opencode/opencode.db` (SQLite, owned by opencode) | full HTTP API, and **`/api/event` exists** (found in the 2.0.30 binary strings 17:4xZ). The service was *stopped* at probe time, so the event shape is **not verified** | plugins | also ships `opencode acp` (Agent Client Protocol server) |

**Multi-host.** deck-remote runs per host. griffin's sessions are served by griffin's own deck-remote (tower/LEARNINGS.md:18-19). deck-remote reads `SSHHost` from agent-deck's web list but never uses it for transcripts (adeckweb.go:61,86; cli.go:130-133; the only tmux consumer is pane.go:69). Remote files are therefore out of reach today, under every option.

### 1.4 What agent-deck already has (surveyed 17:5xZ–18:2xZ)

Two checkouts. The brief's `/Users/rdesmet/shared-work/git/ai-tooling/agent-deck` is at f8e8b27 (about v1.16.7). It is **1262 commits behind** the binary that actually runs (`agent-deck version` → `v1.16.28+fork.3c4ef685`). That binary is built from the worktree `/Users/rdesmet/src/agent-deck-rebase/stealguard` (below: `SG/`), branch **`local/deck-remote-support-2026-10-20`**, last commit 3c4ef685 at 2026-10-20T12:10Z. Paths without a prefix are relative to `SG/`.

- **Hooks carry status, not content.**
  - `agent-deck hook-handler` maps hook events to running/waiting/dead. It writes `hooks/<id>.json`, the `.sid` anchor and a rolling `hooks/<id>.events.jsonl` (200 lines, diagnostic only).
  - It never writes the DB. The TUI's fsnotify watcher picks the status up instead.
  - Hooks exist for Claude, Gemini, Hermes, Cursor and Copilot. Codex has only `notify`. pi has an extension.
  - (survey; the HEAD checkout's `claude_hooks.go:40-66` and `hook_handler.go:120-176,381-427`)
- **Session identity across `/clear`:**
  - The rebind gates (instance.go:6487-6574) accept a newer conversation id from any non-terminal hook.
  - The `session_links(session_id, harness, native_id, host, authoritative, first_seen, last_seen)` table keeps the whole history (internal/statedb/recall_hints.go:107-115).
  - **Verified read-only 18:1xZ:** tower (`f8593170-…`) has 6 claude ids in order: 17295b8f, f1062fa0, fcf66e7c, 31edd4d9, 8caea8db, 4cce9561 (the last one authoritative). The table holds 108 rows for 60 sessions.
- **The `recall` subsystem** (`internal/recall/`; `docs/recall-timeline.md`, `docs/remote-recall.md`):
  - `recall timeline <session> --json [--tail N | --since C | --before C --limit N]` and `recall follow <session> --after C --jsonl [--status]`.
  - The row ids are native and stable: Claude `uuid` (`uuid#n` per block), tool_use id, `queue:<ts>:<hash>` for typed-while-busy. Never a line index.
  - Kinds: `user, assistant, thinking, tool, bash, edit, read, subagent, todo, question, skill, command, system, compaction, turn_end, other`.
  - `--since` returns new rows plus `updates` and `removed`.
  - Follow polls the native file every 250 ms and emits `resync_required` when the source is rewritten.
  - The Claude queue mapping (enqueue, then absorbed or dequeued) is spelled out in rows.go:530+ and the docs. Classification is at internal/recall/query/rows.go:252-270: `isMeta` and `[INBOX]` become `system`, `<command-name>` becomes `command`.
  - Claude and Codex are read directly from the native file and need **no index**. pi, Gemini, OpenCode and Hermes are served only from the `recall.db` index (`source:"index"`), which needs a sweep.
  - `agent-deck remote HOST recall timeline|follow` forwards the same reads over ssh to the owning host. That covers griffin without copying files. Not verified against griffin: tower/FOLLOWUPS.md:5 recorded `remote events follow` returning no frames, which is a different verb over the same SSH path.
- **The switch:** `[recall] enabled` (internal/session/userconfig.go:3136-3178) is off in `~/.agent-deck/config.toml`, so `recall timeline` exits 2 (checked 18:1xZ). Turning it on also enables `backfill_on_enable` (a one-time background index of every transcript, default true) and `hook_sweep` (the SessionEnd hook indexes its transcript, default true). Both can be turned off, which keeps only the direct-read path.
- **Measured** with the binary in a temp `HOME` (`--transcript <file> --harness claude`), on tower's 108 MB `17295b8f`:

  | call | time | output | notes |
  |---|---|---|---|
  | `--tail 30` | 0.06 s | 3.2 MB | tool bodies (up to 32 KiB each) come along, and there is no kind or body filter flag (`recall timeline --help`) |
  | full read | 1.41 s | 250 MB | 5143 rows, all ids unique |

  Kinds in the full read: other 2209 (api-request 1049, api-request-blob 1128), system 708, bash (the Bash tool) 555, user 422, assistant 415, turn_end 374, thinking 368, tool 49, edit 29, subagent 10, command 2, compaction 1.

  Of the 422 `user` rows: 235 are queued, 115 plain, **54 are raw `<bash-input>` `!` lines** and **18 are peer messages** (`[agent-deck from:` / "Another Claude session") with no origin. Not tested: resolving a deck session id instead of `--transcript`, and `follow`.
- **Other agent-deck surfaces:**
  - `agent-deck web` (127.0.0.1:8420) serves sessions REST, `/events/menu` SSE and a tmux WebSocket. There is no transcript endpoint.
  - The event bus `events follow --jsonl --since` (`<data>/bus/<profile>/active.ndjson`) carries `session.status/turn/transcript/…` frames with numeric cursors (docs/events.md).
  - `session send --stream` tails the Claude JSONL only.
  - agent-deck's own opencode integration is the **1.x per-TUI `--port` model** (instance.go:2033-2046, `GET /event`). deck-remote targets the 2.x host-wide service (harness_opencode.go:20-29). Whether recall's opencode reader (`~/.local/share/opencode/storage/…`, internal/recall/reader/opencode.go:16-23) covers 2.x sessions is **unverified**: 2.x keeps `opencode.db`, but a `storage/` directory also exists.

### 1.5 Where Claude-specific assumptions leak today

Inside the adapter boundary, which is fine: `claudeAdapter` (harness_claude.go), the transcript parser (history.go:38-461), the pane permission-prompt parser.

Outside the boundary, which is the coupling the operator means:
- `history.go` is both the shared endpoint *and* the Claude parser in one file. `histMsg` is defined next to Claude's `jsonlLine` (history.go:47-67). The `bash` role and its `Stdout/Stderr` fields are a Claude `!`-mode concept that sits in the shared row type (history.go:51-54).
- Claude delivery does not go through `Deliver`. It uses the "compatibility turn runner" (harness_claude.go:73-75; `snapshotHarness` excludes claude, harness.go:101-110), so Claude replies come back over SSE by `requestId`, while the other harnesses use snapshot correlation. That gives two reply paths in the client.
- `cleanReplyContent` strips Anthropic content-block JSON (history.go:399-440) and is applied to every reply on the client too (app.js:1625).
- The comment in `loadHistory` says it all: "non-Claude sessions return {messages:[]}" (app.js:1631-1632). It is outdated but shows the original design.
- **The server gates on tool name, not on capabilities.** `structuredHarness`/`snapshotHarness` (harness.go:104-115) are checked at history.go:482, activity.go:166, permission.go:32, approve.go:84 and watcher.go:97. ask.go:116-134 sends every non-snapshot tool, unknown tools included, to the Claude turn runner (`session send --wait`). Of the 13 capability flags (harness.go:12-24), the client reads only `interrupt`, `steer`, `ask` and `slashCommands` (app.js:1315, 1329, 1362, 1458).
- **Claude TUI scraping outside the adapter:**
  - `parseActivity` (activity.go:11-14, 53-118) is used by `sessionBusy` on any pane (ask.go:284-297).
  - Printing slashes scrape the pane (ask.go:303-309, 387-402).
  - Watcher Claude branches: watcher.go:119-122, 156-157, 161-162.
  - The legacy approve path sends "1"+Enter (approve.go:22-57, 100-161).
  - `handleReply` falls back to `claudeReply` for every non-snapshot tool (sessions.go:232-257).
- **Client:**
  - `FALLBACK_SLASH_COMMANDS` is a copy of the Claude list (app.js:1436-1441).
  - `Task(` is labelled as a subagent (app.js:937-942).
  - Claude box glyphs are stripped in `permissionSummary` (app.js:2037-2042).
  - The agent name defaults to "Claude" (app.js:1908).
  - Hard-coded "Codex" strings (app.js:2093, 2223).
  - `claude_session_id` reaches the PWA wire (cli.go:131).
- **Remote sessions:**
  - `findTranscript` searches only local `~/.claude/projects` (history.go:203-228), so a remote Claude session shows an **empty thread**.
  - The empty thread gets no "open terminal" note, because that note is gated on `tool !== 'claude'` (app.js:1666).

The turn store removes only the *history* coupling (history.go, the poll, the row model). The tool-name gating, pane scraping and two delivery paths are a separate cleanup: route on `Capabilities`, and move the Claude turn runner behind `claudeAdapter.Deliver`. That cleanup should be listed as separate work and not folded into the store slices.

## 2. A common turn model (needed by every option)

**Do not invent a new schema. Project recall rows/v2** (§1.4), so that ingesting from recall is a field mapping and our own adapters emit the same shape:
- **Taken as is from rows/v2:** `id` (native, stable), `kind`, `ts`, `title`, `body`, `queued`, `meta.delivery`, and the `row`/`update`/`remove` frames.
- **Two additions on the deck-remote side:**
  - `origin` ∈ human | phone | peer:<from-id> | system. rows/v2 has no origin field: today peer messages come out as `system` or `user`.
  - `seg`: the native conversation id, so that a `/clear` chain is several segments.
- **Two post-filters on the deck-remote side until the fork does them:** `<bash-input>`/`<bash-stdout>` folded into a `bash` row (reuse `bashEntry`, history.go:89-108), and `other`/`thinking`/tool bodies dropped for the phone.

The sketch below is the projected shape (field names follow rows/v2 where one exists):

```
turn {
  sid      string   // agent-deck instance id (the session as the operator sees it)
  seg      string   // harness conversation id: claude uuid / codex id / opencode sid / pi file
  seq      int64    // per-sid, monotonic, assigned by the store or adapter: THE cursor
  id       string   // rows/v2 id: claude uuid(#n) / queue:<ts>:<hash> / codex item id / opencode msg id / pi entry id
  kind     user | slash | bash | peer | reply | commentary | tool | notice | boundary
  origin   human | phone | peer:<from-id> | system
  queued   bool     // typed while the agent worked (claude queued_command, pi steer/follow-up)
  ts       int64    // ms
  title    string   // one-line summary (rows/v2)
  body     string   // full text (rows/v2)
  meta     {stdout, stderr, tool, args, requestId, boundary: clear|compact|new|branch}
  replaces id       // pi branch switch / edit: hide the old rows from here on
}
```

- **Ordering** follows native append order inside a segment (input report §4: do not sort by `ts`). Segments are ordered by the chain (`boundary` rows separate them). `seq` never decreases. A branch switch appends a `boundary{branch}` row plus the new rows, with `replaces` pointing at the abandoned subtree root, so the client hides the abandoned rows and nothing has to be rewritten.
- **Turn kinds by harness:** user typed (all); mid-turn/queued (claude `attachment.queued_command`, pi steer/follow-up, opencode `delivery`); slash (claude `<command-name>`, others: shown as text); bash (claude only); peer (claude `isMeta` + `[agent-deck from:` and `queued_command` origin peer; others: plain user text with the `[agent-deck from:` prefix); reply/commentary (all); tool (summary rows: name + one-line arg, optional, off by default).
- **`/clear` / `/new`:** each harness conversation id is a `seg`. A `boundary{clear}` row is written when the adapter sees the id change (claude: tmux `CLAUDE_SESSION_ID` / hook `session_id`; pi: newest file changes; opencode: binding changes; codex `/new`).
- **Compaction** is a `boundary{compact}` row. The rows before it stay readable: the phone shows what was said, not what the model still remembers.
- **Phone-sent rows** go into the log at send time with `origin=phone, id=phone:<requestId>`. When the adapter later sees the native twin, it emits an `update` that re-keys that row to the native id instead of adding a new one (match: same text within `SAME_TURN_SECS`). This fixes the "vanishes on reload" case without any client cleverness.

## 3. Options

Effort: S ≤ ½ day, M ≈ 1 day, L > 1 day.

### A. Stateful turn store inside deck-remote, filled by per-harness ingest adapters (the operator's idea)

- **Shape:** `store` package: an append-only per-`sid` turn log, plus an in-memory index of `seq` → offset and the latest `seq`. Ingest adapters have a pull form (`Ingest(ctx, sid, since cursor) ([]turn, cursor)`: tail/parse, which is what claude/codex/pi do today) and an optional push form (`Subscribe`: opencode `/api/event`, Claude hook payloads). The web reads only the store: `GET /api/rc/turns?id=&before=seq&limit=` and `?since=seq`, with SSE `turns` events that carry `{sid, seq_from, seq_to}`, and the client then fetches `since`.
- **Backfill:** on the first open of a session, the adapter parses its sources from byte 0 (or the opencode API from the first message). That is already done today on every cold start: 108 MB reparses in ~0.3 s warm (input report §5, slice 9). History across `/clear`: backfill the older segments lazily when the user scrolls past the oldest row.
- **Live delivery:** the existing 6 s sweep (watcher.go:17,86) calls `Ingest` for sessions with open viewers, every 1-2 s for those. Each new range is published as one SSE `turns` event. The client stops polling and fetches `since=` on the event and on `visibilitychange`. Because SSE is lossy today (sse.go:50-52), the client must also refetch `since=lastSeq` when it reconnects. That works with no event ids: the cursor carries the state.
- **Crash/restart:** per `sid`, the log is the cache and the source files are the truth. On restart the store re-opens its log, and each adapter resumes from the cursor it saved with the last row (claude byte offset plus the checkpoint bytes, like codex, codex_rollout.go:115-128). If the checkpoint fails (file rewritten), it rebuilds that segment and writes a `boundary{rebuild}` row. Phone-origin rows exist **only** in the store, so persistence is what makes them survive.
- **Storage growth:** measured 17:5xZ on tower transcripts, the user, reply and queued text is 0.3-3 % of the file size (17295b8f: 108 MB → 0.87 MB; fcf66e7c: 97 MB → 0.33 MB; 4cce9561: 5.8 MB → 0.17 MB). Peer briefs (3-5 KB each) add to that. Tool output is not stored (one-line summaries only). Tower's whole 4-file chain is therefore about 1.5-2 MB. The whole `~/.claude/projects` (5.2 GB) would be about 50 MB at worst (extrapolated, not measured). Retention: drop the logs of archived or deleted sessions after N days.
- **Multi-host:** each host's deck-remote holds its own store, the same as today. A later "federation" (blackbird's deck-remote proxies `turns` for griffin sessions over tailnet) is clean because the API is cursor-based. Copying files remotely is not needed.
- **Pros:** one read path for the web; ids and a cursor; push; restart continuity; `/clear` chaining by recording seg changes *as they happen* (agent-deck keeps only the current id; the hook log rolls over); phone rows persist; opencode stops being re-pulled.
- **Cons:** a second copy of the data, which can go stale. Mitigation: cursor + checkpoint + rebuild, and keep the source files authoritative. More moving parts than B. Schema versioning: a version bump wipes and rebuilds (the stores are derived).

| slice | size |
|---|---|
| A1 turn model + ids/kind/origin on the existing `/history` JSON (additive), all 4 adapters | M |
| A2 in-memory turn log + `since/before` cursor endpoint + SSE `turns` | M |
| A3 client: switch reconcile from poll+turnKey to SSE+since; keep turnKey as fallback | M |
| A4 persistence (append JSONL per sid, or SQLite) + restart resume + phone rows durable | M |
| A5 `/clear`/`/new` chaining (seg change recorded live; lazy back-segments) | M |
| A6 native push ingest: opencode `/api/event`; Claude hook payload capture | M each |
| Claude parser kind fixes (input report slices 2-5) | M+S+M+S, under any option |

### B. Keep stateless parsing, with a common turn model and an adapter interface in front, plus a cache

- **Shape:** this is the current architecture, completed. Widen `histMsg` to the §2 model, expose `seq/id`, add an opencode cache (by `time.updated`, or keep the last page and re-pull only newer messages), add a `since=seq` cursor computed from the cached list, and optionally have the sweep compare `len(msgs)` and push a `turns` SSE hint.
- **Backfill:** the same as today (parse on demand).
- **Live delivery:** poll, or an SSE hint from the sweep. Both work because `seq` is stable *within one process lifetime*.
- **Crash/restart:** everything is re-derived from the source files, and that is correct for every row that has a native twin. **Phone-sent mid-turn rows still vanish** unless the harness writes them (it does once the Claude `queued_command` parse lands, so in practice this hole closes for Claude). `seq` must be *derived from the native data* (claude line ordinal, codex segment + offset, pi entry id order, opencode created-ms) so it stays stable across restarts. That is doable, except for pi branch switches.
- **Storage growth:** none (memory only, as today).
- **Multi-host:** the same as A (per host).
- **`/clear` chaining:** needs the heuristic from input report slice 9 (match siblings by title, cwd and timestamps), because nothing records the id chain. This is the weakest point of B.
- **Pros:** the smallest change, no new state to corrupt, and it ships in days. **Cons:** no durable phone rows, `/clear` chaining by heuristic, push only as a hint, and each adapter has to keep inventing its own stable `seq`.

| slice | size |
|---|---|
| B1 = A1 | M |
| B2 opencode incremental cache | S |
| B3 `since=seq` + SSE hint | S |
| B4 `/clear` sibling heuristic (input report slice 9) | M |

### C1. Build a new normalized stream inside agent-deck (as the brief framed C)

- **Shape:** agent-deck's hook-handler and its tool integrations write normalized turns into agent-deck's own store. deck-remote reads them over the agent-deck API or CLI.
- **Verdict: not needed.** The fork already has this as `recall` (§1.4). Building another one duplicates about 2.7k lines of row parser and tests (internal/recall/query/rows*.go). It also puts content writes on the synchronous hook path (PreToolUse/Stop run on every Claude tool call), where the existing design deliberately keeps them off: the Stop hook "appends one queue line and returns" (userconfig.go:3161-3164).

### F. (found) Consume agent-deck `recall` rows/v2 as the ingest for Claude and Codex, with deck-remote's own parsers as the fallback

- **Shape:**
  - A new ingest path behind `harnessAdapter` (`recallSource`) runs `agent-deck recall timeline <id> --json --tail N` on open, `--before C --limit N` for scroll-back, and one long-lived `agent-deck recall follow <id> --after C --jsonl --status` per **open** session for live rows.
  - It maps rows/v2 onto the §2 projection and publishes `turns` SSE events with the recall cursor.
  - A `remote HOST` prefix serves sessions whose host is not local.
  - The current parsers stay, and are used when recall is off, unsupported (exit 2 or an `unsupported remote command` error) or not native for the harness (opencode 2.x, pi).
- **Data model:**
  - ids, kinds, queued/absorbed and compaction come from the fork.
  - `/clear` boundaries come from `session_links`: one timeline per `native_id` in `first_seen` order, joined with a `boundary` row.
  - Edits and rewrites arrive as `update`/`remove` frames and `resync_required`.
- **Backfill:** `--tail`/`--before` paging. It is direct-read, so it needs no index for Claude or Codex. Each older `/clear` segment is opened lazily when scroll-back reaches its start.
- **Live delivery:** `follow` (250 ms native poll) → deck-remote → SSE. The PWA's 3 s poll becomes a fallback only.
  - A 3 s `--tail` poll is **not** viable as the main path: 3.2 MB per call on a large transcript, because tool bodies come along and there is no filter flag.
  - Only `follow` or `--since` deltas are viable.
- **Crash/restart:** nothing to recover on deck-remote's side except phone-origin rows. Cursors are opaque and replayable, and `resync_required` covers rewrites. If deck-remote restarts, it re-opens with `--tail`.
- **Storage growth:** none for history. The only new store is a small append log of phone-sent rows (`origin=phone`, keyed by requestId and reconciled against the native twin within `SAME_TURN_SECS`), measured in KB.
- **Multi-host:** `agent-deck remote HOST recall …` is the only option in this doc that reaches griffin. It is still unverified there.
- **`/clear` across sessions:** `session_links` has the chain (verified). Two ways for deck-remote to read it, and the choice is the operator's:
  - add a CLI verb in the fork (`session links <id> --json`, about S); or
  - read `state.db` read-only, which adds a SQLite dependency to a module with one direct dependency (go.mod).
- **Gaps to close** (measured in §1.4):

  | gap | where | size |
  |---|---|---|
  | peer origin | fork: `origin` on rows. deck-remote stopgap: prefix match on `[agent-deck from:` | S |
  | `!` bash rows left raw | deck-remote post-filter (reuse `bashEntry`), or a fork kind | S |
  | `other` noise from newer CC record types | fork reader | S |
  | tool bodies inflate pages | deck-remote drops them; or a fork `--fields` flag | S |
  | `History()` returns the full list | adapter contract change: windowed history, `(before/since cursor) → page` (a 250 MB full read is not an option) | M |
  | process cost: one `follow` subprocess per open sheet | bounded by open viewers; reap on SSE disconnect | S |

- **Pros:** the parser and turn model are already built and tested. It covers six harnesses (two of them direct), handles `/clear` without guessing, reaches remote hosts, and tower and the TUI's `G` search use the same source. This is the operator's "one stateful backend" without a second copy.
- **Cons:** the core deck-remote feature then depends on a fork build plus a config switch. Classification fixes land in another repo. Subprocess and JSON overhead. pi and opencode 2.x stay on deck-remote's own adapters until recall reads them natively.

| slice | size |
|---|---|
| F0 rows/v2-shaped `id/kind/queued/origin` on `/history` from today's parsers, plus Claude `queued_command` parse (= A1a) | M |
| F1 windowed `History` contract (`before`/`since` cursor) and client paging by cursor | M |
| F2 `recallSource` for Claude and Codex behind a flag (timeline + follow, fallback on exit 2) | M |
| F3 phone-origin row log (persist) | S |
| F4 `/clear` chain via `session_links` (CLI verb or read-only DB) | S+S |
| F5 `remote HOST` routing for non-local sessions | M (needs griffin verification) |
| fork: `origin` for peer rows, bash kind, newer CC record types, optional `--fields` | S each, in the fork |

### D. (alternative) Adopt a standard wire model instead of inventing one: ACP / harness-native servers

- **Agent Client Protocol** (`opencode acp` ships in 2.0.30; adapters exist for Claude Code and codex) defines `session/update` notifications: user message chunks, agent message chunks, tool calls, plans. Modelling §2's `turn` on ACP's update kinds would make a future "ACP ingest adapter" nearly free.
- **But ACP assumes the client owns the agent process (stdio).** Under agent-deck the agent is a TUI in tmux, and deck-remote is an observer. Driving sessions through ACP would mean a different launch model, a large departure (L+), and the operator would lose the terminal as the primary surface.
- **Use it as a vocabulary, not a transport:** name the §2 kinds after ACP's update kinds where they map. Cost ≈ 0, keeps the door open.

### E. (alternative) Client-side store (IndexedDB in the PWA)

The phone persists what it saw and only fetches deltas. This fixes reload loss for phone rows on *that device* only. It is not durable across devices, it does not chain `/clear`, and it is not shared with tower. **Rejected** as the primary store. It is fine later as an offline cache layered on A's cursors.

### Comparison

| | A store in deck-remote | B stateless + model | C1 new store in agent-deck | **F consume recall** |
|---|---|---|---|---|
| harness decoupling | strong | medium | strong, in the fork | strong (6 readers exist, 2 direct) |
| fixes dropped kinds | no (parser slices) | no | no | partly (queued, slash yes; peer and bash need fixes) |
| phone rows survive reload | yes | only via the native twin | yes | yes (small local log, F3) |
| history across /clear | recorded from deploy onward | heuristic | recorded | **already recorded** (`session_links`) |
| live push | yes | hint | yes | yes (`follow`) |
| multi-host | per host | per host | possible | **`remote HOST recall`** (unverified on griffin) |
| new code | most | least | most, duplicate | least for what it buys |
| operator gate | none | none | fork work | enable `[recall]` |
| repos touched | 1 | 1 | 2 | 2 (small fork fixes) |

## 4. Recommendation

**F, entered through B's first slice.**
1. Make deck-remote's wire model a projection of recall rows/v2 now, emitted by the parsers we have today (F0). The current UI keeps working, and the mid-turn input fix rides along.
2. Then switch Claude and Codex ingest to `recall timeline/follow` behind a flag once the operator enables `[recall]`, and keep the current parsers as the fallback.
3. Persist only phone-sent rows.
4. Take `/clear` chains from `session_links`.

A (a full store in deck-remote) is the fallback plan if the fork dependency proves unacceptable. Its slices A2-A5 then replace F2-F5 and reuse F0/F1 unchanged.

Why not A first: the expensive parts of A already exist and are tested in the fork: the turn model, stable ids, cursors, the `/clear` chain and remote reads. What A would add on top is a second derived copy to keep consistent.

Why not leave it at B: B cannot record `/clear` chains or reach griffin.

**F0 first** because it ships this week with no operator gate and no new dependency. Every later step reuses its JSON.

## 5. Migration path (the current UI keeps working throughout)

| step | ships | UI impact | size | gate |
|---|---|---|---|---|
| **0 (this week)** | **F0:** add `id` (Claude `uuid`, codex/opencode/pi native ids), `kind`, `queued`, `origin`, `seg` to `/api/rc/history` rows, using rows/v2 names. The JSON change is additive. Ride the input report's slice 2 on it (Claude `queued_command` human), with ids following recall's `queue:<ts>:<hash>` convention so a later switch keeps the client's dedup keys | none until the client uses them; mid-turn prompts appear | M | `go test` with redacted real fixtures; `node --test web/*.test.js` |
| 1 | client dedups by `id` when present (fallback `turnKey`); input report slices 3/5/4 (label hook, slash rows, peer rows collapsed) | new row kinds | S+S+S+M | operator: peer/slash defaults |
| 2 | opencode incremental cache (stop the 4000-message re-pull per poll, harness_opencode.go:454-460) | faster | S | |
| 3 | F1: windowed `History` contract + `before`/`since` endpoint params (`offset` kept as alias) | stable scroll-back | M | |
| 4 | F2: `recallSource` for Claude/Codex behind `--recall` flag; `follow` per open sheet → SSE `turns`; client keeps 3 s poll as fallback | lower latency; the same rows | M | **operator: enable `[recall]`** (suggest `backfill_on_enable=false`, `hook_sweep=false` to start) |
| 5 | F3 phone-row log; F4 `/clear` chain | history crosses `/clear` (all of tower's 6 segments); phone mid-turn sends survive reload | S+S+S | operator: CLI verb vs read-only `state.db` |
| 6 | fork fixes upstreamed to the fork branch: peer `origin`, bash kind, newer CC record types; deck-remote drops its stopgaps | cleaner rows | S each | coordinate with whoever owns `local/deck-remote-support-2026-10-20` |
| 7 | F5 remote routing (griffin) | griffin sessions readable from blackbird | M | verify `remote griffin recall timeline` first |
| 8 | opencode 2.x `/api/event` push (verify the shape with the service running) or a recall opencode-2.x reader | lower latency for opencode | M | |
| — | separate cleanup (not part of the store): route on `Capabilities` instead of tool name, and move the Claude turn runner behind `claudeAdapter.Deliver` (§1.5) | none | M | |

Each step deploys on its own and needs no data migration. `/api/rc/history` keeps answering in its current shape until the client has moved off it.

## 6. Open decisions

0. **(tower, before dispatch)** The live agent-deck binary is built from the branch `local/deck-remote-support-2026-10-20` (3c4ef685, 2026-10-20). Find out who owns that branch and what "deck-remote support" it already covers before assigning F2/F4/fork slices, so they do not collide.
1. **(operator) Enable `[recall]` in agent-deck?** It is required for F2+. Suggested: `enabled = true`, `backfill_on_enable = false`, `hook_sweep = false` to start (direct reads only, no 5.2 GB background index). Its cost is otherwise a one-time throttled backfill.
2. **(operator) F (depend on the fork's recall) vs A (own store in deck-remote).** Recommended: F, with A as the fallback.
3. **(operator) How deck-remote reads the `/clear` chain:** a new fork CLI verb, or reading `state.db` read-only (adds a SQLite dependency).
4. **(operator) Peer and slash rows:** shown by default (collapsed) or behind a toggle. Peer rows are numerous (input report: about 1.5k in its corpus).
5. **(operator) Fork fixes:** land peer `origin`/bash kind/newer record types in the fork (cleaner, and tower benefits too), or keep the stopgaps on the deck-remote side.
6. **(operator) Multi-host:** federate griffin into blackbird's deck-remote via `remote HOST recall`, or keep per-host deck-remotes.
7. **(operator) Storage format for the phone-row log** (JSONL recommended; it stays tiny).

## Caveats

- Unverified:
  - Claude transcript branching on rewind/edit.
  - The opencode `/api/event` shape (the service was stopped).
  - recall against opencode 2.x and pi.
  - `recall follow` and deck-session-id resolution (the probe used `--transcript`).
  - `remote griffin recall …`.
  - codex/pi mid-turn input.
- The recall numbers come from one 108 MB transcript, measured with a temp `HOME` and config. The storage numbers in option A come from 3 tower transcripts, and the fleet-wide figure is extrapolated.
- Plain `file:line` citations in §1.4 refer to `/Users/rdesmet/src/agent-deck-rebase/stealguard` @3c4ef685, except the hook-handler lines, which come from the f8e8b27 checkout (survey). The input report's anchors are from f0c3cc7, and the ones cited here were re-checked at 2ebfe30.
