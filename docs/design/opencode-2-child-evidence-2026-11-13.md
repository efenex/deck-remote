# OpenCode 2.x child-linkage evidence gate

Task: `dr-subagent-view-oc2-gate`. READ-ONLY investigation; application implementation is out of scope.

**Result: source gate passed for 2.0.22 and 2.0.30; retained native-child fixture gate remains open.** Both versions implement a `subagent` tool, parent-linked sessions, continuation, foreground/background execution, and structured lifecycle events. Use a separate `opencode-2` child reader. Do not transplant the 1.x `part.state.metadata.sessionId` decoder.

Observations were made **2026-11-13T18:13:00Z–18:22:27Z**. Source findings below inherit this UTC interval; the SQL snapshot has its own exact timestamp. References are pinned upstream source, not live HTTP captures. Upstream tests were inspected as corroborating evidence, not executed. No OpenCode binary/server was started; no service credentials, `.env`, session prompts or outputs were read into this report; no SSH or API request to griffin was made. Griffin's 2.0.22 version is an operator-supplied fact, not independently probed.

## Evidence identity

- **U22:** tag [`v2.0.22`](https://github.com/anomalyco/opencode/releases/tag/v2.0.22), SHA `527f0b931d1f9b3ebd34e106c51b31ce5db5b075`.
- **U30:** tag [`v2.0.30`](https://github.com/anomalyco/opencode/releases/tag/v2.0.30), SHA `f16ef9ad8878279ea2be3cfab7baeed97aef61aa`.
- Both full SHAs were independently resolved with `git ls-remote https://github.com/anomalyco/opencode.git refs/tags/v2.0.22 refs/tags/v2.0.30`; source archives were downloaded by those SHAs and inspected under `/private/tmp/opencode-<sha>/`. No source build or installation occurred.
- Laptop package: `/opt/homebrew/lib/node_modules/@opencode/cli/package.json:3–10` declares `opencode`/`opencode2` and **2.0.30**. `/Users/rdesmet/.opencode/node_modules/@opencode-ai/sdk/package.json:4` declares **1.14.41**; its `/v2` namespace is not evidence for CLI major 2. Installed binary equivalence to upstream source was not independently established.
- Deck-remote base: `e1a2b94c5b26b1855e24362bfecff81c167a9aad`, branch `analysis/oc2-gate`, initially clean. Existing 2.x wire subset/root exclusion are `harness_opencode.go:134–171,224–254`; paged messages and active-set reads are `:325–355,381–395`. Current child-reader capability is absent from `harness.go:11–26,50–81`.
- Design input: `/Users/rdesmet/shared-work/git/ai-tooling/tower/reports/dr-subagent-view-design-1111.md`, §§3,6–8. This note resolves its separate 2.x gate; it does not authorize implementation. Memory recall returned HTTP 500 and supplied no evidence.

## 1. Identity, storage and invocation linkage

The built-in tool name is **`subagent`**, with input fields `agent`, `description`, `prompt`, optional `model`, `sessionID`, `background`. A new child is created with `parentID=context.sessionID`. A continuation loads `input.sessionID` and rejects it unless its `parentID` equals the current parent. The same child can acquire multiple parent tool-call edges; it must be counted once. New children inherit the parent's location; subsequent movement is possible. Sources: [U30 tool/plugin/subagent.ts:17–54,153–213][tool], [core/session.ts:262–297][create]; the corresponding U22 tool differs only at dependency acquisition (`:73`). Upstream corroboration: [tool-subagent.test.ts:404–465,467–521,576–670][tooltests].

| Entity | Exact 2.x representation | Evidence |
|---|---|---|
| Child identity | `session_v2.id`; parent edge `session_v2.parent_id`; API `Session.Info.id,parentID`. `version` is a DB field, **not** a field of public `Session.Info`. | [core/session/sql.ts:22–76][sql]; [core/session/info.ts:16–28][info]; [schema/session.ts:30–59][session] |
| Ordinary fork | `fork_session_id`, `fork_boundary`; API `fork.sessionID,boundary`. Fork projection sets `parent_id=null`, even though the `session.forked` event calls the source `parentID`. Do not classify that event field as a subagent edge. | [core/session/projector.ts:120–173][fork] |
| Parent/child transcript | `session_message.id,session_id,type,seq,time_created,time_updated,data`; `data` omits row `id,type`, restored during decoding. Assistant tools live in `data.content[]`, not the legacy `part` table. API messages have `id,type,time,content`; the owning session is supplied by the route. | [sql:79–98][sql]; [history.ts:62–72][history]; [schema/session-message.ts:33–37,126–175,212–237][message] |
| Invocation anchor | Parent session ID + parent message `id` + `content[].id`, where `content[].type="tool"` and `name="subagent"`. The tool ID replaces the 1.x `callID`/part identity for this projection. | [schema/session-message.ts:161–175][message]; [schema/session-event.ts:468–543][events] |
| Child join | `content[].state.metadata.sessionID` (**capital D**); `metadata.status` is `running` or `completed`. Continuation input also carries `state.input.sessionID`. Validate the child's `parentID` before accepting either edge. | [tool:153–167,200–201,252–265][tool] |
| Background handback | Parent synthetic message metadata: `source="subagent"`, `childID`, `agent`, `state` (`completed/error/cancelled`). This is structured corroboration, not a new child or an exact tool-call ID. | [subagent-completion.ts:21–45][completion] |
| Recoverable job | Job ID equals child ID; recovery has `kind="subagent",parentSessionID,childSessionID`. Pending background notifications are stored in `kv` under `job.background/<notificationID>`; this is a notification/recovery mechanism, not a durable all-invocation table. | [subagent-job.ts:44–67][jobrunner]; [job.ts:9–50,171–207][job]; [kv/sql.ts:5–9][kv] |
| Pending input/notice | `session_inbox` holds typed `payload`, delivery and enqueue sequence; `session_pending` holds admitted data/sequence; both reference `session_v2`. Delivered synthetic notices project into `session_message`. | [sql.ts:100–145][sql]; [projector.ts:618–646][idle-projector] |

**Live anchor limitation:** `context.progress({sessionID,status:"running"})` emits ephemeral `session.tool.progress`; the persisted running-tool projection starts with `metadata:{}`. Message GETs read that projection without a live overlay. A newly launched foreground child can therefore be discoverable by parent while its exact launch anchor is unavailable to a reader that missed progress. Terminal success copies final metadata; failure snapshots preserve prior progress when available. Keep the child unlinked until corroborated; do not infer its tool from title, time proximity or array position. Sources: [tool:200–201][tool], [publisher:117–120,604–633][publisher], [updater:324–375][updater], [core/session.ts:378–384][create], [store.ts:140–177][store]. Upstream interrupted-link tests: [session-runner-tool-events.test.ts:222–248][tool-events-test]. This limitation exists in both inspected tags.

## 2. Read API and envelopes

These are **source-backed routes**, not requests performed in this investigation. Native contract is equivalent for these reads in U22/U30; differences in §4 do not rename them.

| Read | Response / use | Pinned source |
|---|---|---|
| `GET /api/info` | Bare `{version,pid,urls,paths,...}`, useful for future authorized version negotiation; it is an exception to the data envelope. | [protocol/groups/server.ts:5–19,41–51][serverinfo] |
| `GET /api/session?parentID=<parent>&limit=50&order=desc` | `{data:[Session.Info],cursor:{previous?,next?}}`; direct children only. `parentID=null` selects roots. Use returned opaque cursors. | [protocol/groups/session.ts:48–119,168–199][routes]; [handler/session.ts:60–102][handlers]; [store.ts:99–138][store] |
| `GET /api/session/{childID}` | `{data:Session.Info}`; includes last `outcome` and `time.idle`. | [routes:275–297][routes]; [schema/session.ts:30–59][session] |
| `GET /api/session/{childID}/message` | `{data:[message],cursor:{previous?,next?}}`; default 50, limit 1–200, default newest first. Supply `order` on first page, not with cursor; keep any `type` filter consistent. | [protocol/groups/message.ts:8–40,66–85][messages-route]; [handler/message.ts:9–61][messages-handler] |
| `GET /api/session/{childID}/message/{messageID}` | `{data:message}`; targeted refresh, ownership checked by session. | [routes:782–792][routes]; [handler/session.ts:632–642][handlers] |
| `GET /api/session/active` | `{data:{<sessionID>:{type:"running"}}}`; all executions owned by this process. Includes child drains because they use the same coordinator. Absence is inactive, not proof of successful completion. | [routes:123–125,275–283][routes]; [handler/session.ts:181–188][handlers]; [execution.ts:111–171][execution]; [subagent-job.ts:47–60][jobrunner] |
| `GET /api/event` | Host-wide SSE across locations. JSON event object is directly in each SSE `data:` frame; see §3. | [protocol/groups/event.ts:38–52][event-route]; [event-feed.ts:29–30,48–68][feed] |
| `GET /api/experimental/session/{childID}/log?after=<seq>&follow=true` | SSE of durable session events plus `log.synced {aggregateID,seq?}`; exclusive sequence watermark. **Do not assume event retention is enabled.** | [routes:725–742][routes]; [schema/event-log.ts:7–20][eventlog]; [core/session.ts:405–414][create] |

**There is no dedicated native `/children` route in either inspected tag.** Independent checks: parsed all **113 U22 / 118 U30** OpenAPI paths, zero containing `children`; inspected the session group/handler and its positive `parentID` → SQL `parent_id` implementation. The supported children operation is the filtered session list. Sources: [U22 protocol/openapi.json][openapi22], [U30 protocol/openapi.json][openapi30], [routes][routes], [store:110–113][store]. Do not carry over 1.x `/session/{id}/children`.

Discovery must follow parent identity without imposing the root's directory on children: moved children remain children. Bound depth/pages and verify every ancestry edge within the authorized host/profile/native segment. This is an implementation requirement inferred from the child continuation guard and the moved-child test, not an additional upstream authorization guarantee. [tool:164–167][tool]; [subagent-lifetime.test.ts:83–91][lifetime].

## 3. Lifecycle and event transport

There are three distinct states: the **parent tool invocation**, the **child execution**, and the **job/notification**. Tool `completed` is not child-finished: a background launch returns immediately with `metadata.status="running"`. Foreground interruption explicitly interrupts the child and cancels its job. Completed/failed/cancelled jobs can all produce parent handback notifications. [tool:229–265][tool]; [job.ts:210–236][job]; [completion:21–45][completion].

| State/evidence | Event and persisted reconciliation | Reader interpretation |
|---|---|---|
| Launch | `session.created.data.{sessionID,parentID}`; parent `session.tool.input.started/called` and ephemeral `session.tool.progress.data.{sessionID,assistantMessageID,id,metadata}` | Creation proves relationship, not current running status. `data.sessionID` on tool events is the **parent**; `data.metadata.sessionID` is the child. |
| Running | `session.execution.started.data.sessionID`; fresh `/api/session/active` | Running child; last stored `outcome` can still describe a previous turn. |
| Retry | `session.retry.scheduled.data.{sessionID,assistantMessageID,attempt,at,error}`; assistant `retry` projection | Retrying execution, not terminal failure; reconcile with current active state. |
| Finished | `session.execution.succeeded` / `.failed` (`data.error`) | Projected `idle` message `outcome=succeeded/failed`; `session_v2.idle_outcome,time_idle` → API `outcome,time.idle`. An assistant step ending alone is not the execution boundary. |
| Aborted/interrupted | `session.execution.interrupted.data.reason`; schema permits `user,shutdown,superseded,inactivity`. Native execution emits user/shutdown/inactivity. Tool failure may carry `error.type="aborted"`; job state is `cancelled`. | Non-shutdown interruption projects idle outcome `interrupted`. Preserve native reason and evidence source; do not conflate tool errors and child outcomes. |
| Shutdown/recovery | `interrupted(reason="shutdown")` preserves execution claim; no idle marker/outcome transition. Recoverable background job can resume child on restart; abandoned nonrecoverable child claims are cleared. | Recovering/unknown until fresh evidence, never automatic success or ordinary user cancellation. `time_suspended` is a claim, not proof of live activity. |
| Continued | New `subagent` call with `input.sessionID`; same child. Idle child starts execution; running child accepts steering and joins existing work. | New invocation edge, same child count. There is no separate `continued` status/event; execution-start is not guaranteed for every steering call. |

Table evidence: [session-event.ts:48–66,239–258,328–387,468–580][events]; [execution.ts:48–53,76–85,111–171][execution]; [projector.ts:412–438][idle-projector]; [message-updater.ts:143–148,404–411][updater]; [schema/session-message.ts:284–295][message]; [restart.ts:77–98,138–205][restart]; [store.ts:194–237][store]; [tool:153–213][tool]; [run-coordinator.ts:124–143][coordinator]. Upstream tests independently cover continuation, steering, failure and background completion at [tool-subagent.test.ts:467–670,729 onward][tooltests].

Public native event shape is `{id,type,created,data,location?,metadata?,durable?}`; durable events include `{aggregateID,seq,version}`. Session/tool identifiers are under **`data`**, not 1.x `properties` or a global `payload` wrapper. `session.tool.success/failed` identify `(sessionID,assistantMessageID,id)` and carry final metadata. A failed tool can retain `metadata.status="running"` from its last progress snapshot: that value is not fresh child liveness. `session.step.*`, `session.text.*`, `session.reasoning.*`, `session.tool.*`, `session.deleted`, and revert events can invalidate child views. [schema/event.ts:67–78,90–149][event-schema]; [session-event.ts:177–185,328–580,638–650][events]; [interrupted-link test:234–248][tool-events-test]. Compatibility `session.status` definitions exist, but this reader should use the native execution publisher above rather than importing 1.x busy/idle logic. [schema/session-status-event.ts:9–53][status-schema].

For **background completion delivery**, `SubagentCompletion.deliver` calls `sessions.synthetic`; that path emits `session.inbox.enqueued` with `data.{sessionID,inboxID,item}` and subagent metadata under `data.item.payload.metadata`. `session.inbox.delivered` identifies the admitted item, whose projection becomes the parent's synthetic message with `metadata.{source,childID,agent,state}`. Do not wait solely for the separate `session.synthetic` event: that event exists (e.g. restart notices), but it is not this normal inbox admission path. [completion:37–45][completion]; [session/session.ts:273–311][synthetic]; [inbox.ts:169–181][inbox]; [projector.ts:618–646][idle-projector]; [restart.ts:92–97][restart].

`/api/event` is volatile: no SSE replay ID is emitted, per-subscriber capacity is 4096, overflow fails that subscriber, and reconnection requires reconciliation. `server.connected` is an initial frame, heartbeat is every 15 seconds. [event-feed.ts:8,29–30,64–79][feed]; [handlers/event.ts:11–29][event-handler].

**Durable event classification does not guarantee retained payloads.** Bus `persist` defaults to `false`; projections and sequence advance still commit, while insertion into `event` is conditional. Server passes `options.events?.persist`. All four inspected local databases had **zero `event` rows**. The experimental log's follow mode also reads the retained event table after wakeups; it is not a replacement for `/api/event` when persistence is off. Treat it as optional; bootstrap/recover through session metadata, message projections and a fresh active set. Neither log replay nor a later message read can recover missed ephemeral progress while a foreground tool is still running. [bus.ts:174–203,395–430,828–869][bus]; [server/routes.ts:119][server-routes]; SQL evidence below.

## 4. Differences that matter: U22 → U30

The child parent/link fields, tool inputs/output metadata, direct-child list query, message pagination, execution events, and background completion metadata keep the same contract. `core/src/session/sql.ts`, `store.ts`, `execution.ts`, and `execution/restart.ts` were byte-identical between the two pinned archives. Focused diffs of the tool, schema and route implementations corroborate this; this is not a claim that the entire releases are equivalent.

| Change | Reader consequence / evidence |
|---|---|
| Subagent job/observer lifetime becomes global | U22 obtains `SubagentJob.make` within the plugin scope; U30 obtains `SubagentJob.Service`, forks with global context and uses a global node. This protects completion observers from location/plugin lifetime changes. Do not require the parent notice as the sole authority on 2.0.22. [U22 subagent-job.ts:16–33][jobrunner22] vs [U30:18–40,63–76][jobrunner]; new [subagent-lifetime.test.ts:55–143][lifetime] checks moved child completion/cancellation and location eviction. Source/test evidence, not a reproduced griffin failure. |
| Failed idle messages gain optional `error` | U22 idle has outcome but no error field; U30 forwards execution failure error into the idle projection. Decoder must tolerate absence; use outcome independently of error text. [U22 session-message.ts:284–294][message22] vs [U30:284–295][message]; [updater:62–74,143–148][updater]. |
| Session `time.updated` advances at assistant step start/end/failure | U30 adds `touch()` and updated-time usage writes; U22 leaves these activity updates unchanged. Child list order/cursors can move during activity; deduplicate by child ID and periodically refresh the first page. Never use session `time.updated` as invocation start/end. [U22 projector.ts:58–73,681–700][projector22] vs [U30:58–83,693–716][idle-projector]. |
| Subagent response text joins nonempty blocks with double newlines | No identity/lifecycle decoder change. Read child messages rather than parsing textual parent output; report contains no such output. [U22 subagent-completion.ts:10–17][completion22] vs [U30:10–18][completion]. |
| Job API now requires caller-supplied ID | No child identity change: both subagent runners already supply child session ID. [U22 job.ts:95–102,253–294][job22] vs [U30 job.ts:95–102,252–292][job]; [jobrunner22:37–40][jobrunner22]. |

Additional provider/tool error handling changed between releases (compare [U22 publisher][publisher22] with [U30 publisher][publisher]), so keep error types forward-compatible. These do not change the verified parent/link or lifecycle event schema. The essential reader does not need separate 2.0.22 and 2.0.30 wire implementations; it needs optional-field tolerance and the evidence rules above.

## 5. Retained local data and missing fixture

**SQL observation: 2026-11-13T18:17:31.958108Z.** Main DB opened only as `file:/Users/rdesmet/.local/share/opencode/opencode.db?mode=ro`, with `PRAGMA query_only=ON` and a read transaction. Schema was independently inspected through `sqlite_master`.

| Database scope | Native 2.0.30 sessions | Native 2.x children | `subagent` tool entries | Subagent completion notices | Retained events |
|---|---:|---:|---:|---:|---:|
| Main laptop DB | 35 | 0 | 0 | 0 | 0 |
| `tower/reports/oc-trials/sealed-1110/arm-01/opencode.db` | 1 | 0 | 0 | 0 | 0 |
| Same, `arm-02/opencode.db` | 2 | 0 | 0 | 0 | 0 |
| Same, `arm-03/opencode.db` | 1 | 0 | 0 | 0 | 0 |
| Throwaway capture run (§5 addendum), live laptop DB, 2026-11-13T19:02:27Z, *before* deletion at 19:03:35Z | 2 | 1 | 1 | 0 | 0 |

The sealed trial files are under `/Users/rdesmet/shared-work/git/ai-tooling/`. They had no adjacent WAL files and were inspected using `mode=ro&immutable=1` after ordinary read-only access could not initialize WAL bookkeeping in those read-only directories. This flag was **not** used on the live laptop DB. Counts are per copy, not a deduplicated total. No conclusion is claimed about unsearched locations or griffin.

Main `session_v2` also contains version groups `1.14.41:3/1`, `1.14.48:4/1`, `1.17.11:43/8`, `1.18.25:3/0`, `1.18.27:8/0` (sessions/children): **61 older-version sessions, ten valid parent joins**, not ten native 2.x child examples. Main native-2 transcripts have 1311 messages and 45 idle markers: 33 succeeded, 8 failed, 4 interrupted. This corroborates the idle-outcome vocabulary only, not child lifecycle. No native parent-tool metadata/input `sessionID` link or `job.background/` subagent recovery marker was found.

Reproducible content-free SQL (run against each relevant read-only DB; all results are counts/structural values):

```sql
SELECT version, count(*) AS sessions, sum(parent_id IS NOT NULL) AS children
FROM session_v2 GROUP BY version;
SELECT count(*) FROM session_v2 c JOIN session_v2 p ON c.parent_id=p.id
WHERE c.version LIKE '2.%';
SELECT count(*) FROM session_message m, json_each(m.data,'$.content') t
WHERE json_extract(t.value,'$.type')='tool'
  AND json_extract(t.value,'$.name')='subagent';
SELECT count(*) FROM session_message m JOIN session_v2 s ON s.id=m.session_id,
  json_each(m.data,'$.content') t WHERE s.version LIKE '2.%'
  AND (json_extract(t.value,'$.state.metadata.sessionID') IS NOT NULL
    OR json_extract(t.value,'$.state.input.sessionID') IS NOT NULL);
SELECT count(*) FROM session_message WHERE type='synthetic'
  AND json_extract(data,'$.metadata.source')='subagent';
SELECT count(*) FROM kv WHERE key LIKE 'job.background/%'
  AND json_extract(value,'$.recovery.kind')='subagent';
SELECT count(*) FROM event;
SELECT json_extract(m.data,'$.outcome'),count(*) FROM session_message m
JOIN session_v2 s ON s.id=m.session_id
WHERE s.version LIKE '2.%' AND m.type='idle' GROUP BY 1;
```

**Missing fixture:** a native 2.x parent assistant `content[].name="subagent"` with message/tool IDs and child `state.metadata.sessionID`; matching child `session_v2.parent_id`; child message/idle projection; and an allowlisted live capture of progress → active → terminal. No retained 2.0.22 data was observed. Background completion, continuation of the same child, interruption, and disconnect/restart remain additional runtime acceptance cases. Upstream test source proves intended behavior, not installed runtime/wire behavior.

! PROPOSED ONLY — Authorize one throwaway laptop OpenCode **2.0.30** session in a scratch directory, using an operator-approved model and existing authentication, to invoke the built-in **subagent** tool once for a trivial no-files task; capture only parent/child/message/tool IDs, field names, timestamps, status/counts and allowlisted SSE metadata before/during/after completion, then recheck the DB with `mode=ro`. Any service start needed is part of that future authorization, not this investigation. Do not use griffin. A subsequent background run, continuation and explicit child interruption would fill the remaining lifecycle fixtures. Nothing on this line was executed.

**Addendum — fixture captured 2026-11-13T19:02:06Z–19:03:35Z (task `dr-oc2-fixture-capture`).** Line 133 was executed once, unchanged except for the model: the laptop default `flashnext/qwen3.8-flash-next` returned `provider.transport ConnectionRefused` (11 retries, no subagent call; those sessions were interrupted and deleted by 19:00:19Z), so the operator approved `litellm/qwen38fn`. One foreground `subagent` call ran on the existing 2.0.30 background service (pid 2250, not restarted, no server started), scratch dir under `/tmp`, no files read or written. The DB was read with `mode=ro` at 19:02:27Z.

- **Observed, matching §1:** parent assistant `content[]` entry `type=tool,name=subagent`, `state.status=completed`, `state.input.{agent,description,prompt}`, `state.metadata.{sessionID,status:"completed",truncated}`; that `sessionID` equals the child `session_v2.id`, whose `parent_id` equals the parent; both sessions `version=2.0.30`, `fork_session_id` null, `time_suspended` null. Child messages: `user`, `assistant`, `idle(outcome=succeeded)`; parent also got a `system` message (`metadata.notice="instructions"`) after the tool returned, then a final `assistant` and `idle(succeeded)`. Session `idle_outcome/time_idle` populated on both.
- **Timing (ms from parent creation):** child session row created at +5510, parent tool `time.ran` +5503, child idle +9108, parent tool completed +9112, parent idle +12738. 1 Hz `session.active` poll: parent only from +870, parent+child from +5870, parent only from +9870 (whole-second resolution). Child drains were visible in the active set, as §2 states.
- **Not covered:** ephemeral `session.tool.progress` and SSE (not subscribed; running-state `metadata.sessionID` therefore unobserved), retained events (event table 0 rows, as before), background completion notice, continuation, interruption, restart. No `session_inbox`/`kv job.background` rows were created for this run.
- **Fixture:** `testdata/subagents/opencode-2.x/` (`manifest.json`, `parent.json`, `child.json`, `lifecycle.json`); ids substituted, times offset, all text/prompt/title/model/path fields `[REDACTED]`; manifest conforms to `fixture.schema.json` from `feat/subagent-view-s0s1` (not copied here).
- **Cleanup (UTC):** `session.active` empty 19:03:35Z; parent+child deleted via `opencode session delete` (cascade) 19:03:35Z; read-only recheck: 96 sessions/10 children, 0 matching rows/messages, 0 `job.background` kv, 0 events (identical to the 18:43:33Z baseline); scratch dir removed 19:03:35Z. The §5 SQL therefore returns 0 again on the live DB; the table row above records the pre-deletion state.

## 6. Mapping to S0 and estimate — proposal only

Use the design's optional child-reader boundary and namespace `opencode-2`. Retain the existing root exclusion and 2.x control path. Additive reader work is required because current `ocMessage` drops tool IDs/metadata and session outcome/time.idle (`harness_opencode.go:136–171`).

| S0 entity/capability | 2.x mapping |
|---|---|
| Child | `(host,profile,root native segment,child session ID)`, verified parent chain; session title/agent as bounded labels; availability separate from state. |
| Invocation edge | `(parent session ID,parent message ID,tool id)` → validated metadata/input child ID. Many edges per child; explicit unlinked state when ephemeral progress was missed. |
| Lifecycle evidence | Fresh active map + execution events; idle outcome/time for last completed execution; parent metadata is corroboration. Running/retry overrides old terminal outcome. Unknown/stale on lost service or insufficient evidence. |
| Time/duration | Tool `time.ran/completed` describes parent invocation; background launch completion is not child completion. Use correlated execution/idle timestamps for child activity, or report duration unknown. |
| Thread | Child message paging and targeted message refresh; preserve native message/tool IDs and mutable rows. Text blocks lack independent native IDs in the projected message schema: revise the containing message, do not invent stable provider block IDs. |
| Summary/live | Shared direct-child discovery + one active-set read per service; selected-child transcript only. One shared `/api/event` connection, invalidation/coalescing, snapshot recovery. Experimental retained-log support optional. |

The mappings are design inferences from §§1–4, not implemented behavior. S0 should explicitly model missed launch anchors, late background notices, same-child continuation, migrated records, separate forks, moved children, shutdown recovery, and event retention being off.

Estimate for one implementer, including focused fixtures/tests: **S0 contract/version/schema work 0.5–1 engineering day**; **bounded 2.x reader (summary, invocation joins, selected-child messages) another 1.5–2.5 days**; **shared SSE ingestion/recovery another 0.75–1.5 days** if not already covered by design S4. UI slices remain the existing design's S1/S2 scope, not included again here. One authorized fixture capture is the runtime validation prerequisite; lack of a fixture does not prevent drafting source-backed decoders. No implementation, deployment, or held-session interaction is authorized by this estimate.

Verification: parsed both complete OpenAPI route inventories; independently checked SQL schema, parent joins, tool entries and notices; reviewed upstream behavior tests; validated all 46 pinned source-link targets/line bounds and Markdown reference definitions; confirmed the four byte-identical files in §4; `git diff --check` passed. No app tests were run because this change contains only this evidence note. Temporary source downloads/checkers are outside the repository; only the note is committed, without push.

[tool]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/tool/plugin/subagent.ts#L153-L265
[create]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session.ts#L262-L441
[tooltests]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/test/tool-subagent.test.ts#L404
[sql]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/sql.ts#L22-L98
[info]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/info.ts#L16-L62
[session]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/schema/src/session.ts#L30-L59
[fork]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/projector.ts#L120-L173
[history]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/history.ts#L62-L72
[message]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/schema/src/session-message.ts#L126-L295
[events]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/schema/src/session-event.ts#L239-L580
[completion]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/subagent-completion.ts#L10-L45
[jobrunner]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/subagent-job.ts#L18-L76
[job]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/job.ts#L9-L292
[kv]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/kv/sql.ts#L5-L9
[publisher]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/runner/publish-llm-event.ts#L604-L633
[updater]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/message-updater.ts#L324-L411
[store]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/store.ts#L99-L177
[tool-events-test]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/test/session-runner-tool-events.test.ts#L222-L248
[serverinfo]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/protocol/src/groups/server.ts#L5-L51
[routes]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/protocol/src/groups/session.ts#L48
[handlers]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/server/src/handlers/session.ts#L60
[messages-route]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/protocol/src/groups/message.ts#L8-L85
[messages-handler]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/server/src/handlers/message.ts#L9-L61
[execution]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/execution.ts#L48-L171
[event-route]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/protocol/src/groups/event.ts#L38-L52
[feed]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/server/src/event-feed.ts#L8-L79
[eventlog]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/schema/src/event-log.ts#L7-L20
[openapi22]: https://github.com/anomalyco/opencode/blob/527f0b931d1f9b3ebd34e106c51b31ce5db5b075/packages/protocol/openapi.json
[openapi30]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/protocol/openapi.json
[lifetime]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/test/subagent-lifetime.test.ts#L55-L143
[idle-projector]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/projector.ts#L412-L438
[restart]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/execution/restart.ts#L77-L205
[coordinator]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/run-coordinator.ts#L124-L143
[event-schema]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/schema/src/event.ts#L67-L149
[status-schema]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/schema/src/session-status-event.ts#L9-L53
[event-handler]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/server/src/handlers/event.ts#L11-L29
[bus]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/bus.ts#L174-L203
[server-routes]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/server/src/routes.ts#L119
[jobrunner22]: https://github.com/anomalyco/opencode/blob/527f0b931d1f9b3ebd34e106c51b31ce5db5b075/packages/core/src/session/subagent-job.ts#L16-L60
[message22]: https://github.com/anomalyco/opencode/blob/527f0b931d1f9b3ebd34e106c51b31ce5db5b075/packages/schema/src/session-message.ts#L284-L294
[projector22]: https://github.com/anomalyco/opencode/blob/527f0b931d1f9b3ebd34e106c51b31ce5db5b075/packages/core/src/session/projector.ts#L681-L700
[completion22]: https://github.com/anomalyco/opencode/blob/527f0b931d1f9b3ebd34e106c51b31ce5db5b075/packages/core/src/session/subagent-completion.ts#L10-L17
[job22]: https://github.com/anomalyco/opencode/blob/527f0b931d1f9b3ebd34e106c51b31ce5db5b075/packages/core/src/job.ts#L253-L294
[synthetic]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/session.ts#L273-L311
[inbox]: https://github.com/anomalyco/opencode/blob/f16ef9ad8878279ea2be3cfab7baeed97aef61aa/packages/core/src/session/inbox.ts#L169-L181
[publisher22]: https://github.com/anomalyco/opencode/blob/527f0b931d1f9b3ebd34e106c51b31ce5db5b075/packages/core/src/session/runner/publish-llm-event.ts#L328
