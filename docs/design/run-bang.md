# Running an ask's `! <cmd>` lines from deck-remote and the TUI

Design only (2026-10-19, task `deck-remote-run-bang-design`, G15). No code. Scope addendum
(operator 18:0xZ): one runner and one audit path, two front ends: deck-remote on the phone and
`tower_tui.py` on the laptop. Rulings on the six forks (operator 18:4xZ) are recorded in §6 and
applied throughout; §9 is the build plan.

Today both front ends can only copy an ask's `! ` lines (`y` in the TUI, "copy" on the phone).
Claude Code Remote Control cannot run them either. This doc proposes a way to run them that
keeps every operator step an operator step.

## 0. Summary

- **Only ask items run.** The command text comes from tower's journal (`kind=ask`,
  `data.items[i]`), is shown verbatim, and is bound by hash. No free-form shell, no editing.
- **One runner**, `tower/scripts/run_bang.py`, on the laptop as the operator's user. It
  classifies, journals a `run` record, executes detached with a minimal env, redacts, and
  journals the exit code. Both front ends call it; neither touches the shell.
- **Classes gate where a line may run.** Read-only first; mutating and secret-reading later,
  each with its own device-token capability; destructive and interactive lines are
  never phone-runnable and stay copy-only there.
- **Output is untrusted.** Only the exit code and a short redacted tail reach the journal;
  the full log stays on disk; secret-reading lines return the exit code only.
- **Tower signs runnable items** from phase 0 (F6b). An unsigned, expired or altered item is
  copy-only everywhere.
- **Passkey re-auth on the phone** for every mutating or secret-reading line from the start
  (F2b): one WebAuthn assertion with Face ID per line. Read-only lines need only the tap.
- **Audit is the journal** (`kind=run`) plus deck-remote's existing HTTP action log.
- **Hard rules hold.** The phone changes where the operator presses Enter, not who decides.

## 1. What is in today's asks

40 `! ` lines in `tower/journal.jsonl` (kind=ask, data.items, through 2026-10-19), hand-classified
by the rules in §3. Counts are approximate; phase 0 (§6) produces the real ones.

| class | count | examples | phone | TUI |
|---|---|---|---|---|
| read-only | 2 | `vault kv metadata get` (with `$VAULT_TOKEN` test, so copy-only by §3.2 until rewritten), `launchctl print` segments | phase 1 | phase 1 |
| mutating | 17 | `flux reconcile` (a converge), `git pull && ansible-playbook … --tags homelab_health`, `launchctl bootstrap`, `launchctl kickstart`, `ln -sfn`, `cp -p … backup`, `printf > settings.local.json`, `tick_lease.py takeover --force`, `sketchybar --reload`, `gh pr merge`, every `bash`/`python3 <script>` (`fetch_fields.py`, `env-ligna-ga-compare.sh`, `open-prs.sh`, `fix-deckremote-plist.sh`, `mint-kubeconfig.sh`) | phase 2, re-auth | phase 1, y/n |
| secret-reading | 5 | `vault kv get -field=password … \| pbcopy`, `openssl rand \| vault kv put`, `export VAULT_TOKEN=$(vault login -method=oidc …) && sealkeep sync`, `kubectl get secret … -o jsonpath \| wc -c`, `vault kv metadata delete` (also destructive) | phase 3, rc only | phase 2 |
| destructive | 3 | `ssh starling3 'rm -rf -- …'`, `pkill -f …`, `vault kv metadata delete` | never | y/n with the line retyped |
| interactive / local-only | 9 | `open https://…`, `ssh -t … tmux attach`, `ssh -fN -D 1080`, `vault login -method=oidc` (browser), `pbcopy`, `agent-deck session send … (paste …)` | never (copy) | TTY hand-over |
| copy-only | 4 | relative paths without `cd`, lines that assume `$VAULT_TOKEN` from an earlier line, the `tailscale serve --set-path` lines (unknown verb until allowlisted) | copy | copy |

The honest reading: **almost nothing in today's asks is read-only.** Phase 1 on the phone will
run very little until `/ask` writes self-contained lines and the allowlist grows with real
evidence. That is acceptable; the pilot exists to prove the runner and the audit path, not to
be useful on day one.

Two findings shape the design:

- **Most of the value is not on the phone.** Roughly a quarter of the lines need a browser, a
  TTY, the clipboard, a smartcard PIN or Touch ID. They only work where the operator sits.
  The TUI front end gets those by handing over the terminal; the phone never will.
- **Several lines depend on each other** (`export VAULT_TOKEN=…` then a second line;
  `. scripts/env.sh`; `bash scripts/x.sh` with no `cd`). A per-line runner breaks them unless
  the ask is written so each line stands alone (§4.3).

## 2. Threat model

Assets: the operator's laptop (full user privileges, ssh agent, kubeconfigs, Vault token),
every host those credentials reach, the secrets a command prints, and tower's journal, which
an LLM reads.

| threat | where | control |
|---|---|---|
| **Stolen or unlocked phone** | phone | act-cap device token is per phone and revocable; new `run:*` caps are minted separately and only on phones the operator names; every mutating and secret-reading line needs a fresh WebAuthn assertion with user verification (Face ID), which an unlocked phone in someone else's hand cannot produce; destructive never runs from the phone. Blast radius of an unlocked phone is therefore "signed read-only commands tower already proposed", plus reading redacted output of past runs (§4.4). |
| **Session hijack** (token theft from the PWA, XSS) | phone | tokens stay in the PWA's storage; the run endpoint accepts the bearer in the `Authorization` header only (`bearerHeaderOnly`), never `?token=`; same-origin PWA, no CORS. A stolen bearer alone can run only read-only lines: the passkey assertion is bound to the run's challenge and to deck-remote's origin, so it cannot be minted elsewhere or replayed. XSS inside the PWA could drive a `navigator.credentials.get` call, but Face ID still has to happen on the device and the prompt names the origin; the residual is "an XSS'd PWA runs a *signed, tower-proposed* mutating line the operator approves with Face ID", which is the same line they were about to run. |
| **CSRF** | phone | POST with a JSON body and a bearer header; no cookie auth, so a cross-site form cannot carry the credential. Tailscale identity is stamped by `tailscale serve`, not by the client. |
| **Replay / double tap** | both | the request names `(ask, item, sha256(cmd))`; the runner journals `run … --to running` before exec and refuses a second request while that record is `running` or already `done rc=0`. deck-remote's 10 s `towerDup` window stays as the first line. |
| **Injected ask** (a compromised worker appends a plausible `kind=ask` with `--by tower`; prompt injection becomes a command) | both | the journal is a plain file, so `by=tower` is a claim, not proof. From phase 0 every runnable item carries an HMAC that only `journal.py` running with the signing key can produce (§3.5); the runner verifies against the current item text and refuses otherwise. Plus: the operator sees the exact text and confirms per line, the classifier drops destructive and unknown first words to copy-only, and `learnings_check.py` findings are on the confirm screen. **Limit, stated plainly:** workers run as the same uid as tower on the same laptop, and `journal.py` is tower's documented API, so a worker that runs `journal.py add --kind ask --sign` gets signed items without ever touching the key. The signature therefore separates items that went through `journal.py --sign` on this laptop from everything else: raw `>>` appends, rsync or hand edits, writes from another host, a prompt-injected tool call that writes the file. It does **not** separate tower from a same-uid worker; a worker calling tower's `journal.py` is already a hard-rule-3 breach and leaves a transcript. Signing is behind an explicit `--sign` flag that only the `/ask` skill passes, so a worker signing is a deliberate, visible act and not a side effect of using the API. Hardening beyond that is §8. A worker that can write the journal can also call the runner directly, so the runner is not the trust anchor for "the operator did this"; the front ends' logs are. |
| **Secrets in output** | both | output never goes to the phone or the journal unredacted; secret-reading lines keep no stdout at all (§4.4), only redacted stderr; the on-disk log is `0600` under `tower/state/runs/`, local only, 30 days; "unfold" on the phone returns the redacted full log, never a raw one, and each unfold is in the action log. |
| **Output as prompt injection into tower** | both | tower reads the journal; a command's output can contain text aimed at the LLM. Only `rc` and a 20-line redacted tail enter the journal, under a `data.tail` key that `/ask` treats as evidence, not instructions. |
| **Runner environment leaks** | both | the runner does not inherit deck-remote's environment (no `DECK_REMOTE_*`, no shared token), only a minimal env (§4.2). |

Local TUI versus remote phone: on the laptop the operator is already authenticated to the OS,
has the TTY, and can type a PIN. Re-auth adds nothing there; confirmation does. On the phone the
device is the weak point and the TTY is absent, so classes are narrower and re-auth is per class.

## 3. Authority

### 3.1 Verbatim binding

A run request is `{ask, item, sha256}`. The runner re-reads the *current* journal, finds the
open ask, takes `items[item]`, strips the leading `! `, and refuses unless the hash matches.
This is the pattern `tower_actions.go` (branch `feat/tower-tab-parity`) already uses for
fold/close/pick (validate against the current model), and it kills stale tabs, hand-made requests
and edited commands in one check. The front end shows the same text it hashed. Neither front
end accepts typed commands.

**Extraction boundary.** Runnable means: an entry of `data.items[]` whose text starts with
exactly `! `; the command is that string with the two-character prefix stripped, nothing else.
A `!` found in `data.detail`, in `data.text`, or inline in prose (what the TUI's `bangs()`
fallback and `learnings_check.INLINE_RE` find, ending at a double space) is copy-only on both
front ends. The regexes stay for display and for `learnings_check`; they are not in the trust
path.

### 3.2 Classes

Classification is by the first two command words, reusing `learnings_check.command_words`
(strips `sudo`/`env`/`VAR=` prefixes). A line is split on `&&`, `;`, `||` and `|`; its class is
the **maximum** over segments. Unknown first word → `copy-only`.

| class | rule | examples |
|---|---|---|
| **read-only** | allowlisted verbs that do not write: `git status/log/diff/fetch`, `kubectl get/describe/logs` (not `get secret`), `flux get`, `vault kv metadata get`, `vault kv list`, `launchctl print/list`, `ls`, `cat`, `grep`, `wc`, `df`, `curl -s` without `-X`/`-d`. **No scripts**: `bash x`, `python3 x`, `sh x` are at least mutating whatever the file is called, because a worker can name a file `check.sh` and `briefs/` is writable by tower | ~2 of 40 |
| **mutating** | `git pull/switch/checkout`, `ansible-playbook`, `kubectl apply/rollout`, `flux reconcile/suspend/resume` (a converge is an operator step, AGENTS.md:135), `launchctl bootstrap/kickstart/enable`, `ln`, `cp`, `mkdir`, `printf >`, `perl -pi`, `gh pr merge`, `sketchybar --reload`, `tick_lease.py takeover`, `sealkeep sync`, any `bash`/`python3`/`sh` script; a script may be demoted to read-only only when the ask pins `data.script_sha256` and the runner verifies it | ~17 |
| **secret-reading** | `vault kv get/put`, `vault read`, `vault login`, anything that reads `.env` or `$VAULT_TOKEN`, `openssl rand`, any `kubectl get secret` (no sink detection; `… \| wc -c` goes rc-only and that is fine) | ~5 |
| **destructive** | `rm -rf`, `rm -r`, `pkill`, `kill`, `launchctl bootout`, `vault kv metadata delete`, `kubectl delete`, `git push --force`, `--force`, `git reset --hard`, `flux delete`, `ssh … 'rm …'` (remote segment is classified too) | 3 |
| **interactive / local-only** | `open`, `pbcopy`, `ssh -t`, `ssh -fN`, `tmux attach`, `vault login -method=oidc`, `sudo`, anything in `LEARNINGS.md`'s smartcard list, `agent-deck session send` | 9 |
| **copy-only** | unknown first word, a relative path with no leading `cd <abs>`, a line that reads `$VAULT_TOKEN` or `.env` without setting it in the same line, lines over 2 kB | the rest |

**Never phone-runnable**, regardless of phase: destructive, interactive, copy-only, smartcard
SSH, `sudo`, and anything touching a trust prompt. Hard-rule items (merges, applies, Vault/`.env`
reads, user/key creation) are mutating or secret-reading and arrive in phases 2 and 3 with
re-auth. The classifier is deny-by-default: a verb not in a table is copy-only.

Runnable from the TUI: everything but copy-only. Destructive asks a y/n plus retyping the last
word of the line (the usual "type `delete` to confirm" shape). Interactive lines run with the
TTY handed over (§4.5).

### 3.3 Capabilities

`tokens.go` has a closed `knownCaps` set with only `act`. Add:

| cap | lets a phone run | granted to |
|---|---|---|
| `run:ro` | read-only | the operator's phone, phase 1 |
| `run:mut` | mutating (and read-only) | after phase 1 review |
| `run:secret` | secret-reading, rc only | after phase 2 review, or never |

A token's caps are the ceiling; the class table is the floor. The shared token has no `run:*`
cap and never will. Each run endpoint call must also pass today's `authAct` (act cap, loopback
peer, `Tailscale-User-Login` equal to `--tower-operator`).

### 3.4 Per-command confirm and re-auth

The phone shows one confirm screen per line: the verbatim command, its class, the working
dir, the timeout, and any `learnings_check.py` matches. The operator taps **Run**. For
mutating and secret classes, Run requires a **passkey assertion with Face ID** (F2 = b, from
the start; there is no laptop-armed grant phase).

**Enrolment** (once per phone). The phone must already hold an act-cap device token and come
through `tailscale serve` as the operator. Enrolment additionally needs a **one-time code shown
on the laptop** (TUI `P` key, which asks deck-remote over loopback; 6 digits, 5 min), so a stolen
bearer cannot enrol its own passkey. deck-remote then runs the WebAuthn registration ceremony
(`navigator.credentials.create`, `authenticatorAttachment: platform`, `residentKey: preferred`,
`userVerification: required`) and stores `{credential id, public key, sign count, device-token
id, created}` in `<state>/deck-remote-passkeys.json` (`0600`). `rpId` is the fixed tailnet
hostname (`DECK_REMOTE_PUBLIC_URL`); the LAN fallback hostname is a different origin and cannot
run mutating lines, by design. Revoking the device token revokes its passkeys.

**Per-run assertion.** The phone posts `{ask, item, sha256}`; deck-remote checks class and caps,
then returns a 32-byte challenge stored against that `(ask, item, sha256, device)` for 60 s. The
phone calls `navigator.credentials.get` with `userVerification: required`; Face ID happens; the
phone posts the assertion. deck-remote verifies origin, `rpIdHash`, the UV flag, the signature
against the stored public key, and that the sign count moved, then marks the challenge spent and
execs the runner. One assertion covers one line; a second line is a second Face ID. There is no
session-level "re-authed for 10 min" state. Go side: `github.com/go-webauthn/webauthn`.

Read-only lines need the tap only. The TUI never needs a passkey: the operator is at the
keyboard, and a y/n (retyped last word for destructive) is the confirmation.

### 3.5 Signed runnable items (F6 = b, from phase 0)

**Key.** A 32-byte random HMAC-SHA256 key at `~/.config/tower/run-bang.key` (`0600`, directory
`0700`), created by `tower/scripts/run_bang.py keygen` and never copied into any repo, brief,
env var or launchd plist. It is outside every checkout so no worker's `cwd`, `git grep` or
`find .` reaches it, and the path is named only in this doc and in `run_bang.py`. `keygen
--rotate` writes a new key with a new `kid` and keeps the previous one for 7 days so open asks
still verify. The limit (same uid) is in §2; hardening options are in §8.

**What is signed.** Per runnable item, the canonical string

```
run-bang/v1\n<ask id>\n<item index>\n<sha256 of the item text with the "! " prefix stripped>\n<expiry, unix seconds>\n
```

MAC'd with the key. Stored in the ask's data, parallel to `items`:

```json
"sig": {"kid": "k1", "exp": 1760990400, "items": {"2": "base64(mac)", "3": "base64(mac)"}}
```

Default expiry is 24 h from signing; `/ask` can shorten it. Items that are not `! ` lines have
no entry. Binding the index and the ask id means an item cannot be transplanted into another
ask or reordered; binding the hash means one changed byte breaks it; expiry bounds how long an
old ask stays executable.

**Who signs.** `journal.py add --kind ask … --sign` walks `data.items`, computes the MACs and
writes `data.sig` before appending the record. The flag is explicit: only the `/ask` skill
passes it, and an `add --kind ask` without it appends unsigned (a warning on stderr), which
makes the ask copy-only on both front ends, visibly, and is the safe failure. Any later
`add --kind ask --sign` for the same id that changes `items` re-signs; one that only changes
other data (answer, detail) leaves `sig` alone. The `/ask` skill otherwise changes nothing
except emitting self-contained lines (§4.2). Same-uid limit: §2.

**Verification in the runner.** `run_bang.py run --ask A --item i --sha S` re-reads the
journal, derives A the way `derive()` does (`a.update(data)` per record, so `items` and `sig`
are whichever record last set each), requires status `open`, takes `items[i]`, strips `! `,
recomputes the sha and requires it to equal S, then recomputes the MAC with the key named by
`kid` and compares it to `sig.items[i]` in constant time, and requires `now < exp`. Any failure
is a refusal with a reason (`unsigned`, `expired`, `tampered`, `wrong kid`) that is journaled as
`run … --to refused` and shown on the front end. A later record that changed `items` without
`--sign` leaves the old `sig` in the merged ask, so its hash no longer matches: `tampered`,
refused, which is the right answer for an edit that bypassed the skill. Only then is the class computed and the line
executed. `run_bang.py verify --ask A` is the dry-run both front ends call to draw the badge.

## 4. Execution

### 4.1 Host and user

The laptop (`blackbird`), as the operator's own user, the same user that runs deck-remote's
launchd job and the TUI. Remote hops (`ssh`, `kubectl --context`, `ansible-playbook -l`)
happen *inside* the command through the operator's existing credentials. The runner never
executes on another host and never uses a service account. This is deliberate: every runnable
class executes with full operator privilege, and the classifier plus confirm are the only
barriers. Say that on the confirm screen for mutating lines.

### 4.2 Working dir and environment

- **cwd**: the ask's `data.cwd` if tower set one, else the leading `cd <abs>` in the line, else
  `$HOME`. A relative path with neither is copy-only (§3.2), and `/ask` should emit
  `cd <abs> && …` lines so that rule rarely bites.
- **env**: a fresh minimal env, not deck-remote's: `PATH` (the launchd PATH), `HOME`, `USER`,
  `LANG`, `TERM=dumb`, `VAULT_ADDR` when the line names it, `KUBECONFIG` from the operator's
  default. No `DECK_REMOTE_*`, no shared token, no `VAULT_TOKEN` carried from a previous line.
- **`.env` and Vault tokens**: a line that needs a secret must obtain it *inside the line*
  (`export VAULT_TOKEN=$(vault login …) && …`, `set -a; . .env; set +a; …`). Those lines are
  secret-reading: the runner returns the exit code only, keeps the on-disk log but redacts it
  before the phone or journal see anything, and neither deck-remote nor tower ever hold the
  value. Lines that *assume* `$VAULT_TOKEN` from an earlier line are copy-only. On the laptop
  the OIDC login opens a browser, so `vault login -method=oidc` is interactive and TUI-only.
- **shell**: `/bin/zsh -c <line>` with `set -o pipefail`, one line per run, no state across
  lines (F3).

### 4.3 Timeouts and streaming

`towerExecTimeout` (`tower_actions.go`, branch `feat/tower-tab-parity`) is 60 s and bound to the
HTTP request, which fits `journal.py` and nothing else. The runner is detached: `subprocess.Popen(start_new_session=True)` (macOS has no `setsid`
binary; this is the portable form), stdout+stderr spooled to `tower/state/runs/<run-id>.log`
(`0600`), kills sent to the process group. Per-class defaults: read-only 2 min, mutating 15 min,
secret 5 min; `/ask` may set `data.timeout` up to 30 min. On timeout the process group gets
`SIGTERM`, then `SIGKILL` after 10 s, rc=124.

deck-remote tails the log file and publishes `tower-run-output` events on the existing SSE hub
(same as `tower-reply-result`), redacted line by line; the TUI tails the same file. A phone that
disconnects does not stop the run; reopening the tab resumes from the file. A deck-remote
restart does not kill the run either, because the runner is a child of `run_bang.py`, not of
deck-remote (F1).

Interactive runs on the TUI (§4.5) hand over the TTY, so nothing is spooled or redacted; they
journal `running` and `done` with rc only and no tail. That keeps one audit path with one
documented gap rather than a second log.

**Stale `running` records.** The `running` record carries `pid` and `pgid`. If the runner
dies (laptop sleep, crash), a `running` record older than its timeout plus 60 s is treated as
`done rc=-1` by `derive()` and by the refuse check, so it can neither block a rerun nor block
auto-close forever. A rerun after rc≠0 or rc=-1 is a new record with an attempt suffix
(`<ask>:<item>:<sha8>:2`); only `done rc=0` for the same `(ask, item, sha)` refuses a rerun.

Redaction on every line, before it leaves disk: known token shapes (`hvs.`, `s.` Vault tokens,
`ghp_`/`gho_`, `AKIA`, `xox[bp]-`, `eyJ` JWTs, `password=`, `token=`, `Bearer `, base64 runs over
40 chars), plus a 64 kB cap on the live stream and a 20-line cap on the journal tail.
Secret-reading lines stream nothing; the phone sees "running… rc=0".

### 4.4 Unfolding the full output (F4 = a, unfoldable to c)

- **Where it is kept.** `tower/state/runs/<run-id>.log`, `0600`, on the laptop only. It is
  never copied to the phone's storage, never attached to the journal, never pushed anywhere.
  Retention 30 days; `run_bang.py gc` (called from the overview launchd job) deletes older logs
  and the journal keeps the `done` record with its 20-line tail as the durable trace.
- **What the stream and the tail are.** The live stream (§4.3) is the first 64 kB, redacted.
  The journal tail is the last 20 lines, redacted. Both are derived from the log file, never a
  second capture.
- **Unfold on the phone.** `GET /api/rc/tower/run/<id>/log` behind `authAct` plus the run's
  class cap. It streams the whole file through the same redactor, uncapped by the 64 kB stream
  limit but capped at 1 MB (larger logs are truncated with a marker; the TUI can show the rest).
  It is never the raw file: redaction is applied at read time, so a redactor fix also applies to
  old logs. Each unfold is one action-log line (`action: run-log`, device, login, run id), so
  "who looked at the output" is auditable like "who ran it".
- **Unfold on the TUI.** Reads the same file through the same redactor (`run_bang.py log
  <id>`). The TUI may pass `--raw` for read-only and mutating runs because the operator is at
  the laptop where the raw file already is; `--raw` is refused for secret-reading runs.
- **Secret-reading lines.** The runner connects stdout to `/dev/null`: a secret-reading line's
  primary output is never written to disk, so there is nothing to unfold and nothing a redactor
  can miss. stderr is spooled through the redactor and capped at 8 kB, so a failed `vault` call
  still explains itself. Unfold on a secret-reading run therefore shows redacted stderr only, on
  both front ends. If the operator needs the value, they run the line on the laptop; that is the
  point of the class.
- **Interactive runs** (TUI TTY hand-over) have no log; unfold shows "ran on the TTY, rc=N".

### 4.5 The TUI front end

`tower_tui.py` gains `!` on a waiting item: pick a line (or all runnable lines in order), confirm,
then `Actions.run(ask, i, sha)` calls the same `run_bang.py`. For read-only and mutating lines the
TUI shows the tailed log in its detail pane. For interactive lines it suspends curses
(`endwin`), runs the line in the foreground with the real TTY, and resumes; that is the mode the
phone never gets, and it is why the TUI is a front end worth having at all for the lines in §1
that need a browser, a PIN or the clipboard.

### 4.6 Exit code back to tower

Each run journals two records through `journal.py add`, the only writer:

```
journal.py add --kind run --id <ask>:<item>:<sha8> --to running --by operator \
  --data '{"ask":…,"item":i,"sha256":…,"class":"ro","via":"phone|tui","device":"<token name>","login":"<ts login>","cwd":…}'
journal.py add --kind run --id <same> --to done --by operator \
  --data '{"rc":0,"secs":12.4,"log":"state/runs/<id>.log","tail":[…20 redacted lines…]}'
```

`derive()` gets a `run` state machine (`running -> done | refused`; `refused` is terminal and
never satisfies auto-close). The overview and `/ask` read it:
when every runnable item of an ask has `done rc=0` and no copy-only items remain, tower closes
the ask (`--to answered`, `answer: "ran from phone/tui, rc=0 ×N"`) and fans out. A non-zero rc
leaves the ask open, marks the item ✗, and tower's next tick sees the tail. Who performs the
close is F5; the lean is tower, not the runner, so the runner never writes `ask` records.

## 5. Audit

One append-only path: the journal's `run` records (who: `by`, `device`, `login`, `via`; what:
ask, item, hash, class, cwd; when: `ts`; result: rc, secs, log path, tail). The full output is
in `state/runs/`, retained 30 days. deck-remote's `deck-remote-tower-actions.log` keeps the HTTP
side: every allowed or refused attempt with device and Tailscale login, including refusals the
runner never saw (wrong cap, no identity, bad hash). The TUI writes no second log; its runs are
distinguishable by `via: tui`.

Tower reads runs through `derive()` like any other kind; `/recap` lists the last 12 h of runs
under the ask they belonged to; `/retro` can count refusals and non-zero exits.

## 6. Rollout

1. **Phase 0, classify and sign.** `journal.py` signs every `! ` item of every new ask (§3.5);
   both front ends show each line's class, its signature badge (✓ signed / ✗ unsigned /
   expired) and `learnings_check` matches next to the copy button. No runner, nothing executes.
   One week against real asks; every misclass or unsigned ask is a retro item. Signing from day 0
   means phase 1 never runs an item that predates signatures.
2. **Phase 1, read-only, plus passkey enrolment.** `run_bang.py`, `kind=run`, `run:ro` cap, TUI
   `!`. Phone runs signed read-only lines on a tap; TUI runs read-only, mutating (y/n) and
   interactive (TTY hand-over). The passkey enrolment and assertion path (§3.4) is built in
   this phase. Read-only lines still need only the tap; a Settings toggle ("ask Face ID for
   read-only runs too", off by default) lets the operator exercise enrolment and assertion on
   real runs without turning the pilot into a Face ID tax, so phase 2 is a cap flip and not a new
   auth surface.
3. **Phase 2, mutating from the phone** behind `run:mut` and a Face ID assertion per line.
   Still no destructive.
4. **Phase 3, secret-reading from the phone**, rc and redacted stderr only, behind `run:secret`
   and Face ID. Opt-in; may stay off.

Destructive from the phone is not a phase.

### Rulings (operator, 2026-10-19 18:4xZ)

| # | ruling | consequence in this doc |
|---|---|---|
| F1 | **a** | runner is `tower/scripts/run_bang.py`, exec'd by deck-remote and the TUI (§4) |
| F2 | **b** | WebAuthn passkey + Face ID per mutating/secret line from the start; no laptop-armed grant (§3.4) |
| F3 | **a** | one line, one fresh shell; env-carrying lines stay copy-only (§4.2) |
| F4 | **a, unfoldable to c** | redacted stream + 20-line journal tail for ro/mut, secret lines rc + redacted stderr; full redacted log on demand, local only, 30 days, never in the journal (§4.4) |
| F5 | **a** | tower closes the ask on its next tick from `run done rc=0` (§4.6) |
| F6 | **b now** | tower-signed runnable items from phase 0; key, payload, verification in §3.5 |

### Forks as presented (kept for the record)

| # | decision | options | lean |
|---|---|---|---|
| F1 | **Where the runner lives** | (a) `tower/scripts/run_bang.py`, deck-remote and the TUI both exec it; (b) Go code in deck-remote, the TUI calls deck-remote over loopback | **(a)**: one implementation, the TUI needs no daemon, detached runs survive a deck-remote restart, and it matches "journal.py is the only writer" |
| F2 | **Re-auth for mutating/secret on the phone** | (a) laptop-armed 10 min grant, journaled (cheap, but only covers "leaving the laptop shortly"); (b) WebAuthn passkey with Face ID (works away from the laptop, the real fit for a stolen unlocked phone, more code); (c) none, rely on the cap | **(a)** for phase 2's first weeks because it is a day of work; **(b)** before phase 2 is called done; never (c). *Overruled: (b) from the start.* |
| F3 | **Shell semantics** | (a) one line, one fresh shell, env-carrying lines copy-only, `/ask` emits self-contained `cd … && …` lines; (b) run all of an ask's lines in one shell in order | **(a)**: per-line confirm and per-line rc need per-line runs; (b) makes one confirm cover a chain |
| F4 | **Output policy per class** | (a) ro/mut: redacted stream + 20-line journal tail, secret: rc only; (b) everything rc only, log on disk; (c) full output to the phone | **(a)** |
| F5 | **Who closes the ask** | (a) tower, on its next tick, from `run done rc=0`; (b) the runner journals `ask --to answered` itself | **(a)**: the runner never writes ask records, and tower's fan-out stays in one place |
| F6 | **Ask integrity against injection** | (a) verbatim confirm + deny-by-default classifier, now; (b) tower signs runnable items (HMAC with a key only tower's user can read) and the runner verifies | **(a)** for phases 0–2; revisit (b) before phase 3 or if workers ever get journal write access by design. *Overruled: (b) now.* |

## 7. Relation to tower's hard rules

Hard rule 2 says merges, force-pushes, Vault/`.env` reads, real `ansible-playbook` runs,
smartcard SSH, user/key creation and trust prompts are operator steps, surfaced as `! ` lines
through `/ask`. This design does not change who decides; it changes where the operator presses
Enter.

- The operator still sees the exact command and confirms it, one line at a time. Tower never
  calls the runner; neither do workers (a worker that does is a hard-rule breach the journal's
  `via`/`device` fields make visible). Nothing runs without a tap or a keypress from the operator.
- Merges and applies stay operator steps: they are mutating, phone-runnable only in phase 2
  with re-auth, and the confirm screen names the class.
- Secrets stay operator steps and stay out of tower: secret-reading lines return rc only, and
  the value never transits deck-remote, the journal or the phone.
- Smartcard SSH, `sudo` and trust prompts stay where the hardware and the TTY are: TUI-only,
  with the terminal handed over. The phone shows them as copy-only.
- Force pushes and deletes are destructive: never from the phone, retyped on the TUI.
- The permission classifier is not routed around: this runner runs the operator's own command
  under the operator's confirmation, not a worker's. The class table is a second classifier on
  top, not a replacement.

## 8. Open questions (not forks)

- `flux reconcile` is classified mutating (a converge, hard rule). If that makes the phone
  pilot useless in practice, the operator can allowlist it per cluster context in phase 2, not
  by loosening the class.
- Retention of `state/runs/` logs: 30 days (§4.4). Secret-reading runs keep redacted stderr
  only, so there is nothing further to drop.
- **Signing-key hardening** beyond a `0600` file (same-uid limit, §2): (a) a macOS Keychain
  item whose ACL names one code-signed helper binary that does the signing, so `python3` cannot
  read it; (b) a Secure Enclave key, which would make every `/ask` prompt for Touch ID and so
  does not fit an unattended tower; (c) a second uid (`_towersign`) owning the key behind a
  launchd socket, which would need a way to tell tower's processes from a worker's and there is
  none on the same uid. Lean: ship the file, revisit (a) if a retro ever shows a worker reading
  `~/.config/tower/`.
- Does `/ask` grow a `cwd`/`timeout`/`class_override` schema, or does the runner infer
  everything from the line? Lean: infer, override allowed only to make a line stricter.

## 9. Build plan

Slices are ordered so each is mergeable alone and tested before the next starts. "Tests"
means automated unless marked manual. Nothing in this plan is built yet; tower dispatches
after the operator sees it.

### Tower side (`ai-tooling/tower`)

| slice | builds | tests |
|---|---|---|
| **T1 classify** | `scripts/runbang/classify.py`: the §3.2 tables, `command_words` reuse, segment split, max-over-segments, copy-only rules (relative path, `$VAULT_TOKEN` assumption, unknown verb, >2 kB). `run_bang.py classify <line>` CLI. | `scripts/tests/test_classify.py`: a fixture of the 40 journal lines with expected classes (the §1 table, corrected as phase 0 finds misclasses); every verb in the tables; `sudo`/`env`/`VAR=` prefixes; `ssh host 'rm -rf'` classified by the remote segment; unknown verb → copy-only. |
| **T2 sign** | `scripts/runbang/sign.py`: keygen/rotate (`~/.config/tower/run-bang.key`, `0600`, `kid`), canonical string, `sign_items(ask_id, items, exp)`, `verify(ask_rec, i, sha)`. `journal.py add --kind ask` calls `sign_items` on `items` changes; warning and no `sig` when the key is missing. | `test_sign.py`: round trip; one changed byte, swapped index, transplanted ask id, past `exp`, unknown `kid`, missing key → each a distinct refusal; rotation keeps the old `kid` verifying within 7 days; `journal.py add` writes `sig` only for `! ` items and re-signs only when `items` change. |
| **T3 run kind** | `journal.py derive`: `run` state machine (`running -> done \| refused`), stale-`running` rule (timeout + 60 s → rc -1), attempt suffixes; `overview.py` and `asks.md` show per-item ✓/✗/⏳ and the 20-line tail; `/ask` auto-close rule (all runnable items `done rc=0`, no copy-only left) implemented in the `/tick` path as a `journal.py add --kind ask --to answered`. | `test_journal_run.py`: replay fixtures for running/done/refused/stale/rerun; auto-close fires only when every runnable item is rc=0 and never when a copy-only item remains; a non-zero rc leaves the ask open with the tail visible. |
| **T4 runner** | `scripts/run_bang.py run --ask --item --sha --via phone\|tui --device --login`: verify (T2) → classify (T1) → journal `running` with pid/pgid → `Popen(start_new_session=True)` with the §4.2 env and cwd → spool to `state/runs/<id>.log` `0600` (stdout to `/dev/null` for secret) → per-class timeout, TERM then KILL → redactor → journal `done` with rc, secs, tail. `log <id> [--raw]`, `verify --ask`, `gc`. | `test_run_bang.py`: fake commands (`true`, `false`, `sleep`, `printf` of token shapes); env contains no `DECK_REMOTE_*`; cwd rules; timeout gives rc 124 and no orphan in the pgid; redactor fixtures for every token shape in §4.3; secret class writes no stdout; `--raw` refused on secret; double run refused on `done rc=0`, allowed with suffix after rc≠0; `gc` deletes only files older than 30 days. |
| **T5 ask authoring** | `/ask` skill and `overview.py`: emit self-contained `cd <abs> && …` lines; `learnings_check` output carried into `data.learnings[i]` so the front ends can show it without re-running the check. | `test_learnings_data.py`; a lint in `learnings_check.py` that warns on a `! ` line with a relative path or an unset `$VAULT_TOKEN`. |

### deck-remote side (`ai-tooling/deck-remote`, branch from `feat/tower-tab-parity` once merged)

| slice | builds | tests |
|---|---|---|
| **D1 caps** | `tokens.go`: `run:ro`, `run:mut`, `run:secret` in `knownCaps`; mint UI shows them; `whoami` reports them. | extend `tokens_test.go`: mint/refuse unknown cap; `hasCap` per class; shared token never has `run:*`. |
| **D2 passkeys** | `passkeys.go` on `go-webauthn/webauthn`: enrol-code (6 digits, 5 min, minted by `POST /api/rc/passkey/enrol-code` behind `authAct` on loopback only, shown by the TUI), `POST /api/rc/passkey/register/{begin,finish}`, `POST /api/rc/tower/run/challenge`, assertion verify (origin, rpIdHash, UV, signature, sign count), store `<state>/deck-remote-passkeys.json` `0600`, revoke with the device token. | `passkeys_test.go` with a software authenticator (the library's test helpers): enrol needs act cap + identity + a valid code; wrong origin, UV=false, stale sign count, spent or expired challenge, challenge for a different `(ask,item,sha)` → each refused and action-logged; revoking the token removes its credentials. |
| **D3 run endpoints** | `tower_run.go`: `POST /api/rc/tower/run` `{ask,item,sha256[,assertion]}` behind `authAct` + class cap (+ assertion for mut/secret) → exec `run_bang.py run … --via phone --device --login`; `tower-run-output` SSE events tailing the log through the redactor; `GET /api/rc/tower/run/<id>/log` (§4.4); action-log lines for run, refuse and run-log. | `tower_run_test.go` with an injected `towerExec` (same seam as `tower_actions.go`): cap matrix × class matrix; stale-tab hash mismatch → 409; SSE carries redacted lines and ends with the `done` event; log endpoint caps at 1 MB and is refused for secret runs' stdout; every attempt is one action-log line. |
| **D4 PWA** | Tower tab: class and signature badges on each `! ` line; per-line confirm sheet (verbatim text, class, cwd, timeout, learnings matches); Run button → challenge → `navigator.credentials.get` → run; stream pane; unfold; passkey enrolment screen in Settings; copy stays for copy-only lines. | manual checklist on a real iPhone (Face ID prompt names the origin; LAN fallback origin shows "mutating needs the tailnet URL"); a Playwright run against the software-authenticator server for the sheet, badges and the unfold cap. |

### TUI side (`tower/scripts/tower_tui.py`)

| slice | builds | tests |
|---|---|---|
| **U1 badges** | class and signature badges next to each `! ` line in the detail pane; `learnings` from `data.learnings`. This is the phase-0 deliverable together with T1/T2. | `test_tower_tui.py`: model rendering of the badges from a fixture ask (the TUI already exposes its model to `tower_model.py`, so deck-remote's tab gets the same badges for free). |
| **U2 run** | `!` on a waiting item: pick a line or all runnable lines in order; y/n confirm, retyped last word for destructive; `Actions.run` → `run_bang.py run … --via tui`; log tail in the detail pane; `u` unfolds (`run_bang.py log`, `--raw` offered for ro/mut only); interactive class → `endwin()`, foreground run, resume, rc-only journal. | `test_tower_tui_actions.py` with a stubbed `subprocess.run`: argv shape per class; destructive requires the retyped word; interactive path journals rc only; non-curses pieces unit-tested, the curses hand-over checked manually. |
| **U3 enrol code** | `P` shows the passkey enrol code: `POST http://127.0.0.1:<port>/api/rc/passkey/enrol-code` over loopback with the operator's act-cap token (read from the TUI's config, never from argv), the same loopback-plus-act gate `tower.go` already trusts; no CLI path. | one test for the request shape and the 5-min expiry message, with a stubbed HTTP client. |

### Order

Phase 0 = T1, T2, T5, U1 (and D4's badges). Phase 1 = T3, T4, D1, D2, D3, D4, U2, U3.
Phase 2 and 3 are cap mints and a one-line gate flip each, no new slices.
