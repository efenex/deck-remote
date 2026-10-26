# Codex 0.164 rollout compatibility

Read-only shape audit on 2026-10-26: 19 local rollout files under the requested
2026/10/26 directory, 17 with `cli_version: 0.164.0` and two with 0.158.0.
Inspection output contained record types, field names/types and counts. A later
independent read-only check loaded 25 real 0.164 rollouts and matched all 17
available final replies in memory; no content was printed or copied. The
fixture `testdata/codex/normal-0164.jsonl` is handwritten synthetic content, not
an excerpt or transformed copy of an operator conversation.

| Surface | Existing 0.143/0.144 parser | Observed 0.164 shape / handling |
| --- | --- | --- |
| Identity | `session_meta.id/session_id`, `cli_version` | Same fields; additional provenance/context metadata is ignored. |
| User input | `event_msg.user_message.message` | `response_item.message` with role `user`, `input_text` content; `item_completed.UserMessage` mirror has a different ID. Content-kind metadata excludes AGENTS/environment instructions. |
| Replies | `event_msg.agent_message`, phase | `response_item.message` role `assistant`, `output_text`, `commentary`/`final_answer`; `item_completed.AgentMessage` usually shares its ID. Native IDs and per-source occurrence counts avoid duplicate rows. |
| Peer messages | Not parsed | `response_item.agent_message` has author/recipient and `input_text`; display as peer, never as the session's final reply. |
| Reasoning | Ignored | Encrypted `response_item.reasoning` and completed Reasoning items stay out of the conversation. |
| Tools | Function/custom calls and matching outputs | Same call IDs; `arguments`/`input` strings, optional namespace, output can be a content array. Pending tools clear on their matching output. |
| Questions | Synchronous `request_user_input` | The sample contained `request_user_input_async` with title/options strings and an immediately completed output. Its displayed question is commentary, not a keyboard-driven form. Synchronous question parsing is retained and tested with a synthetic call using the existing schema. |
| Turns/tokens | Task start/complete/abort events | Task boundaries remain authoritative; completion's `last_agent_message` is a deduplicated fallback. Token counts and token-usage records are telemetry, never replies or turn boundaries. |

Policy: accept stable patch versions in the explicitly validated 0.143, 0.144
and 0.164 minor families. Do not infer compatibility for the intervening or newer
minor versions; prereleases and malformed versions are unverified. Record parsing
uses shape rather than header version because resumed sessions retain their old
metadata. Unknown versions preserve recognized messages and append a clear
read-only/update notice; attention, delivery and other structured actions remain
gated. An empty unknown rollout still produces that notice instead of a blank
transcript. No private reasoning or developer/system instructions are displayed.
