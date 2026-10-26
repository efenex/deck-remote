# Phone composer photos

The composer offers **Camera** and **Photos**, removable thumbnails and an optional
caption. Up to four photos stay in the open composer until the server accepts the
send. Selection never sends. Only Claude and Gemini advertise this capability;
running Codex sessions do not support `session send --image`.

The browser decodes camera/library files and normalizes them to JPEG (long edge
at most 2560 px), stripping metadata and bounding uploads. Unsupported formats
produce an actionable error. Inputs are limited to 16 MiB each, normalized files
to 8 MiB each. The existing authenticated `POST /api/rc/ask` accepts multipart
fields `sessionId`, `profile`, `text`, and repeated `photos`. Its existing act-token
and operator-identity gate remains mandatory. JSON text/slash requests keep their
current contract. The server caps the whole request, validates count, byte size,
format and dimensions, and generates private temporary filenames; no client path
is accepted.

The handler resolves the session/profile, rejects unsupported harnesses and uses
`session send --message-file - --queue --image <temp> ... --json`. It waits for the
durable queue receipt before returning 202 and uses the existing send tracker.
The CLI copies attachments into the session's ignored `.agentdeck-images/` before
queueing (verified in `session_send_queue_cmd.go` and `session_cmd.go` in the
local full21c checkout; installed CLI help also advertises `--image`). Thus request
temporary files can be removed on success or failure without breaking delivery.
Those CLI-owned copies follow agent-deck's existing retention policy.

Upload errors retain the draft and stop the spinner. A lost acknowledgement is
labelled unconfirmed; there is no automatic resend. Accepted bubbles carry the
image count and follow queued/delivered/failed status. No photo is stored in
localStorage or served by a new public endpoint. Tests use fake CLI processes and
synthetic images; they never send into live sessions.
