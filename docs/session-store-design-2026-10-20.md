# Session store: decoupling deck-remote from Claude Code transcripts (design, 2026-10-20)

Task `deckremote-session-store-design`. Repo HEAD 2ebfe30. Evidence gathered 2026-10-20T17:25Z–18:10Z by reading code, and with read-only probes of local files. No code was changed and nothing was restarted. Input: `tower/reports/deckremote-input-history-2026-10-20.md` (below: "the input report").

## TL;DR

- **Half of the operator's idea already exists.** deck-remote already has a per-harness adapter boundary (`harnessAdapter`, harness.go:69-77) and one common row type (`histMsg`, history.go:47-57). Three of the four adapters already keep state in memory and read incrementally. What is missing: a turn model rich enough to say *who* wrote a row and *what kind* of row it is; stable ids and a cursor; push delivery; durability across restarts and `/clear`.
- **The dropped-input bug is in the Claude parser, not in the architecture.** No architecture choice alone brings back mid-turn prompts, peer messages or slash commands. Slices 2-5 of the input report are needed under every option. A store makes them cheaper to ship and to test, but it does not replace them.
- **Recommendation: build option A in steps, behind the endpoint that exists today.** (1) a richer turn model with stable ids, emitted by all four adapters through the current `History()`; (2) an in-memory per-session turn log with a `since=` cursor and a `turns` SSE event; (3) persist that log (append files or SQLite), which brings restart continuity and history across `/clear`; (4) native live ingest where the harness offers it (opencode `/api/event`, Claude hooks), with file tailing kept as the fallback. Pushing the stream into agent-deck itself (option C) is not worth doing now. §5 says why.

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
| claude | JSONL per conversation, append-only, `uuid`/`parentUuid` per line (tree: rewind/edit can branch, *not verified here*) | none for a TUI session | SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Notification, PermissionRequest, PreCompact, Stop, SessionEnd are all wired to `agent-deck hook-handler` in `~/.claude/settings.json` (read 17:3xZ). The payloads carry the prompt text and `transcript_path` | agent-deck keeps only status: `~/.agent-deck/hooks/<id>.events.jsonl` lines are `{at,event,status,session_id,pid}` with **no content**, and the file is a rolling buffer (tower's holds 200 lines, back to 14:21Z, so it **does not** preserve the `/clear` chain of session ids) |
| codex | rollout JSONL segments, with `event_msg` + `response_item` | (codex has an app-server/MCP mode; not used under agent-deck tmux) | agent-deck writes a `.sid` sidecar (codex_rollout.go:428) | turn-structured already (`turn_started`/`turn_complete`) |
| pi / omp | JSONL tree per session file | none used (pi has an RPC mode; not used under agent-deck) | none | branches are first-class: the store has to support "branch switched" |
| opencode 2.x | `~/.local/share/opencode/opencode.db` (SQLite, owned by opencode) | full HTTP API, and **`/api/event` exists** (found in the 2.0.30 binary strings 17:4xZ). The service was *stopped* at probe time, so the event shape is **not verified** | plugins | also ships `opencode acp` (Agent Client Protocol server) |

**Multi-host.** deck-remote runs per host. griffin's sessions are served by griffin's own deck-remote (tower/LEARNINGS.md:18-19). deck-remote reads `SSHHost` from agent-deck's web list but never uses it for transcripts (adeckweb.go:61,86; cli.go:130-133; the only tmux consumer is pane.go:69). Remote files are therefore out of reach today, under every option.

<!-- AD-SURVEY -->

### 1.4 Where Claude-specific assumptions leak today

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

One normalized row, emitted by every adapter. It is a superset of `histMsg`, so the current JSON stays valid:

```
turn {
  sid      string   // agent-deck instance id (the session as the operator sees it)
  seg      string   // harness conversation id: claude uuid / codex id / opencode sid / pi file
  seq      int64    // per-sid, monotonic, assigned by the store or adapter: THE cursor
  nid      string   // native id: claude uuid / opencode msg id / pi entry id / codex "<seg>:<byteoff>"
  kind     user | slash | bash | peer | reply | commentary | tool | notice | boundary
  origin   human | phone | peer:<from-id> | system
  queued   bool     // typed while the agent worked (claude queued_command, pi steer/follow-up)
  ts       int64    // ms
  text     string
  meta     {stdout, stderr, tool, args, requestId, boundary: clear|compact|new|branch}
  replaces nid      // pi branch switch / edit: hide the old rows from here on
}
```

- **Ordering** follows native append order inside a segment (input report §4: do not sort by `ts`). Segments are ordered by the chain (`boundary` rows separate them). `seq` never decreases. A branch switch appends a `boundary{branch}` row plus the new rows, with `replaces` pointing at the abandoned subtree root, so the client hides the abandoned rows and nothing has to be rewritten.
- **Turn kinds by harness:** user typed (all); mid-turn/queued (claude `attachment.queued_command`, pi steer/follow-up, opencode `delivery`); slash (claude `<command-name>`, others: shown as text); bash (claude only); peer (claude `isMeta` + `[agent-deck from:` and `queued_command` origin peer; others: plain user text with the `[agent-deck from:` prefix); reply/commentary (all); tool (summary rows: name + one-line arg, optional, off by default).
- **`/clear` / `/new`:** each harness conversation id is a `seg`. A `boundary{clear}` row is written when the adapter sees the id change (claude: tmux `CLAUDE_SESSION_ID` / hook `session_id`; pi: newest file changes; opencode: binding changes; codex `/new`).
- **Compaction** is a `boundary{compact}` row. The rows before it stay readable: the phone shows what was said, not what the model still remembers.
- **Phone-sent rows** go into the log at send time with `origin=phone, nid=requestId`. When the adapter later sees the native twin, it fills in `nid` on that row instead of adding a new one (match: same text within `SAME_TURN_SECS`). This fixes the "vanishes on reload" case without any client cleverness.

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

- **Shape:** this is the current architecture, completed. Widen `histMsg` to the §2 model, expose `seq/nid`, add an opencode cache (by `time.updated`, or keep the last page and re-pull only newer messages), add a `since=seq` cursor computed from the cached list, and optionally have the sweep compare `len(msgs)` and push a `turns` SSE hint.
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

### C. Push the normalized stream into agent-deck (session events), shared by deck-remote and the TUI

- **Shape:** agent-deck's hook-handler (Claude) and its tool integrations write normalized turns into agent-deck's `state.db` (SQLite, `~/.agent-deck/profiles/<p>/state.db`, present 17:3xZ). deck-remote reads them over the agent-deck web API or CLI.
- **For:** one source for the TUI, deck-remote, tower and `session output`. agent-deck already owns session identity, the hook stream and the `.sid` sidecars. It sees `/clear` because SessionStart fires with the new `session_id`.
- **Against:**
  1. Hooks exist only for Claude. agent-deck still has to tail codex rollouts and pi files and talk to the opencode service, so the parsers just move into the fork (Go to Go, which is feasible but is a fork divergence we would carry).
  2. Today's hook-handler persists no content, so a new data path is needed in the fork, and upstreaming it is uncertain.
  3. The operator's own `remote events follow` bus produced no frames on griffin (tower/FOLLOWUPS.md:5), which is a feasibility warning for remote use.
  4. deck-remote then depends on a fork build for its core feature, and every schema change spans two repos and two release trains.
  5. The hook path runs inside every Claude tool call (PreToolUse is synchronous, `AGENTDECK_PRETOOL_SYNC=1`), so adding content writes there costs latency on the operator's hot path.
- **Backfill / restart / growth:** the same mechanics as A, in agent-deck's DB. Multi-host: agent-deck has a remote concept, which is the only option that could serve griffin without another deck-remote, but see point 3 above.
- **Effort:** L (fork schema + hook capture + 3 non-hook ingesters + API + deck-remote client), plus a cross-repo release.

### D. (alternative) Adopt a standard wire model instead of inventing one: ACP / harness-native servers

- **Agent Client Protocol** (`opencode acp` ships in 2.0.30; adapters exist for Claude Code and codex) defines `session/update` notifications: user message chunks, agent message chunks, tool calls, plans. Modelling §2's `turn` on ACP's update kinds would make a future "ACP ingest adapter" nearly free.
- **But ACP assumes the client owns the agent process (stdio).** Under agent-deck the agent is a TUI in tmux, and deck-remote is an observer. Driving sessions through ACP would mean a different launch model, a large departure (L+), and the operator would lose the terminal as the primary surface.
- **Use it as a vocabulary, not a transport:** name the §2 kinds after ACP's update kinds where they map. Cost ≈ 0, keeps the door open.

### E. (alternative) Client-side store (IndexedDB in the PWA)

The phone persists what it saw and only fetches deltas. This fixes reload loss for phone rows on *that device* only. It is not durable across devices, it does not chain `/clear`, and it is not shared with tower. **Rejected** as the primary store. It is fine later as an offline cache layered on A's cursors.

### Comparison

| | A store in deck-remote | B stateless + model | C in agent-deck | D ACP transport |
|---|---|---|---|---|
| harness decoupling | strong (ingest interface) | medium (adapters already exist) | strong, but in the fork | strongest, wrong process model |
| fixes dropped kinds | no (needs parser slices) | no | no | n/a |
| phone rows survive reload | **yes** | only via the native twin | yes | yes |
| history across /clear | **recorded** | heuristic | recorded | n/a |
| live push | yes (cursor + SSE) | hint | yes, if API | yes |
| restart | resume from cursor | re-derive | agent-deck's problem | – |
| multi-host | per host; federate later | per host | possible, bus unproven | – |
| first useful slice | M (A1) | M (B1) | L | L+ |
| repos touched | 1 | 1 | 2 (fork) | many |

## 4. Recommendation

**Do A in steps, where the first steps are B.** A1 and B1 are the same slice, so the decision between A and B (whether to persist) can wait until A3. agent-deck (C) stays the place for identity and status, not for content. Name the turn kinds after ACP (D) at no cost.

Why A over B in the end: the two things B cannot do well are exactly the operator's complaints. "My input history is missing" includes phone rows lost on reload and history stopping at `/clear`. B can only fix those with heuristics. A fixes them by recording what it saw, while the source files stay the truth for everything they contain.

## 5. Migration path (the current UI keeps working throughout)

| step | ships | UI impact | size | gate |
|---|---|---|---|---|
| **0 (this week)** | **A1a:** expose `seq`, `nid`, `kind`, `origin` on `/api/rc/history` rows (additive JSON; old clients ignore them). Claude: `nid=uuid`, `seq=line ordinal`. Codex/pi/opencode: the existing `Seq`/`TurnID` plus native id. **Ride the input report's slice 2 (Claude `queued_command` human) on it**, because it needs the same struct changes (`jsonlLine` gains `Uuid`, `Origin`, `Attachment`) | none until the client uses it; mid-turn prompts appear as `you` rows | M | `go test` on fixtures from real (redacted) entries; `node --test web/*.test.js` |
| 1 | client: dedup by `nid` when present, fall back to `turnKey`; scroll-back by `before=seq` (server keeps `offset` as an alias) | fixes the "two identical oks" merge and offset drift | S | JS tests |
| 2 | opencode incremental cache (B2): stop the 4000-message re-pull per poll | none (faster) | S | opencode test fixture |
| 3 | input report slices 3/5/4: label hook, slash rows, peer rows collapsed | new row kinds | S+S+M | operator: defaults for peer/slash |
| 4 | A2: in-memory turn log per sid + `GET /api/rc/turns?since=` + SSE `turns{sid,seq}`; the sweep ingests open sessions every 1-2 s | client still polls; the new endpoint is used behind a flag | M | |
| 5 | A3: the client switches to SSE+since; keeps the 3 s poll as a fallback when SSE is disconnected | lower latency, fewer requests | M | soak on the phone |
| 6 | A4: persist the log; phone-origin rows durable; restart resumes from the cursor | phone mid-turn sends survive reload/restart | M | operator: storage format |
| 7 | A5: record seg changes live → `/clear` / `/new` chaining, with lazy back-segments on scroll | history crosses `/clear` from the deploy onwards; earlier history needs a one-time backfill heuristic (input report slice 9) | M | operator: backfill old chains or not |
| 8 | A6: opencode `/api/event` subscribe (verify the shape first with the service running); optional Claude hook-payload capture (needs the fork's hook-handler to forward `prompt` + `transcript_path`, or a deck-remote-own hook) | lower latency | M each | operator: fork change or not |

Each step deploys alone, needs no data migration (the stores are derived; a schema bump means wipe and rebuild), and leaves `/api/rc/history` working until the client has moved off it.

## 6. Open decisions (the operator's)

1. **Persist or not** (A vs B after step 5). The recommendation is to persist. Without it, phone rows and recorded `/clear` chains are not possible.
2. **Storage format:** append-only JSONL per session (no new dependency; deck-remote has one direct dependency today, go.mod) vs SQLite (`modernc.org/sqlite`, pure Go but a large dependency, or cgo). The recommendation is JSONL plus an in-memory index. The access pattern is append + range-by-seq per session, and there are no cross-session queries.
3. **Peer and slash rows:** shown by default (collapsed) or behind a toggle. About 1.5k peer rows in the input report's corpus (§1), so they are noisy.
4. **Backfill of pre-deploy `/clear` chains:** heuristic sibling matching (input report slice 9) or "history starts at deploy".
5. **Claude hook capture:** change the agent-deck fork's hook-handler to forward content, or keep file tailing only for Claude (it works; ~3 s latency is the poll, not the parse).
6. **Option C long-term:** whether turn content should ever live in agent-deck (it would serve the TUI and tower too). Not recommended now; revisit if agent-deck upstream adds an event store.
7. **Multi-host:** keep per-host deck-remotes, or have blackbird federate griffin's `turns` API later.

## Caveats

- Unverified: Claude transcript branching on rewind/edit (whether `parentUuid` forks), the opencode `/api/event` payload shape (service stopped at probe time), codex/pi mid-turn input.
- The storage numbers come from 3 tower transcripts. The fleet-wide figure is an extrapolation.
- The anchors in the input report were taken at f0c3cc7. The ones used here were re-checked at 2ebfe30 where they are cited.
