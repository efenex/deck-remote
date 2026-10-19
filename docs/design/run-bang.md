# Running an ask's `! <cmd>` lines from deck-remote and the TUI

Design only (2026-10-19, task `deck-remote-run-bang-design`, G15). No code. Scope addendum
(operator 18:0xZ): one runner and one audit path, two front ends: deck-remote on the phone and
`tower_tui.py` on the laptop.

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
| **Stolen or unlocked phone** | phone | act-cap device token is per phone and revocable; new `run:*` caps are minted separately and only on phones the operator names; mutating and secret classes need re-auth (fork F2); destructive never runs from the phone. Blast radius with no re-auth is "read-only commands tower already proposed". |
| **Session hijack** (token theft from the PWA, XSS) | phone | tokens stay in the PWA's storage; the run endpoint accepts the bearer in the `Authorization` header only (`bearerHeaderOnly`), never `?token=`; same-origin PWA, no CORS. Re-auth for mutating classes means a stolen bearer alone cannot apply. |
| **CSRF** | phone | POST with a JSON body and a bearer header; no cookie auth, so a cross-site form cannot carry the credential. Tailscale identity is stamped by `tailscale serve`, not by the client. |
| **Replay / double tap** | both | the request names `(ask, item, sha256(cmd))`; the runner journals `run … --to running` before exec and refuses a second request while that record is `running` or already `done rc=0`. deck-remote's 10 s `towerDup` window stays as the first line. |
| **Injected ask** (a compromised worker appends a plausible `kind=ask` with `--by tower`; prompt injection becomes a command) | both | the journal is a plain file, so `by=tower` is a claim, not proof. Controls: the operator sees the exact text and confirms per line; the classifier drops destructive and unknown first words to copy-only; `learnings_check.py` findings are shown on the confirm screen. Later option: tower signs runnable items (F6). A worker that can write the journal can also call the runner directly, so the runner is not the trust anchor for "the operator did this"; the front ends' logs are. |
| **Secrets in output** | both | output never goes to the phone or the journal unredacted; secret-reading lines return the exit code only; the on-disk log is `0600` under `tower/state/runs/`; a byte cap and token-shape redaction apply to everything else. |
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
TTY handed over (§4.4).

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
mutating and secret classes, Run requires re-auth (F2). The lean is a **laptop-armed nonce**:
the TUI (or a hotkey) arms "phone may run mutating for 10 min", journaled as `grant`; the phone
can only ask. This reuses tower's existing `grant` kind and needs no WebAuthn plumbing, and it
means a stolen phone alone cannot apply anything. Its cost is real: the reason to apply from the
phone is being away from the laptop, and a short laptop-armed window covers only "I am leaving
in a minute, let the rollout land from the train". A **passkey with Face ID** (WebAuthn,
`navigator.credentials.get` with `userVerification: required`, the credential bound to
deck-remote's origin) is the fit for the stolen-unlocked-phone case and works anywhere; it is
more code, a server-side credential store, and a second secret to enrol and back up. The fork
is F2.

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

Interactive runs on the TUI (§4.4) hand over the TTY, so nothing is spooled or redacted; they
journal `running` and `done` with rc only and no tail. That keeps one audit path with one
documented gap rather than a second log.

**Stale `running` records.** The `running` record carries `pid` and `pgid`. If the runner
dies (laptop sleep, crash), a `running` record older than its timeout plus 60 s is treated as
`done rc=-1` by `derive()` and by the refuse check, so it can neither block a rerun nor block
auto-close forever. A rerun after rc≠0 or rc=-1 is a new record with an attempt suffix
(`<ask>:<item>:<sha8>:2`); only `done rc=0` for the same `(ask, item, sha)` refuses a rerun.

Redaction on every line, before it leaves disk: known token shapes (`hvs.`, `s.` Vault tokens,
`ghp_`/`gho_`, `AKIA`, `xox[bp]-`, `eyJ` JWTs, `password=`, `token=`, `Bearer `, base64 runs over
40 chars), plus a 64 kB cap on what the phone receives and a 20-line cap on the journal tail.
Secret-reading lines stream nothing; the phone sees "running… rc=0".

### 4.4 The TUI front end

`tower_tui.py` gains `!` on a waiting item: pick a line (or all runnable lines in order), confirm,
then `Actions.run(ask, i, sha)` calls the same `run_bang.py`. For read-only and mutating lines the
TUI shows the tailed log in its detail pane. For interactive lines it suspends curses
(`endwin`), runs the line in the foreground with the real TTY, and resumes; that is the mode the
phone never gets, and it is why the TUI is a front end worth having at all for the lines in §1
that need a browser, a PIN or the clipboard.

### 4.5 Exit code back to tower

Each run journals two records through `journal.py add`, the only writer:

```
journal.py add --kind run --id <ask>:<item>:<sha8> --to running --by operator \
  --data '{"ask":…,"item":i,"sha256":…,"class":"ro","via":"phone|tui","device":"<token name>","login":"<ts login>","cwd":…}'
journal.py add --kind run --id <same> --to done --by operator \
  --data '{"rc":0,"secs":12.4,"log":"state/runs/<id>.log","tail":[…20 redacted lines…]}'
```

`derive()` gets a `run` state machine (`running -> done`). The overview and `/ask` read it:
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

1. **Phase 0, classify only.** Both front ends show each line's class and `learnings_check`
   matches next to the copy button. No runner. Validates the classifier against real asks for a
   week; every misclass is a retro item.
2. **Phase 1, read-only.** `run_bang.py`, `kind=run`, `run:ro` cap, TUI `!`. Phone runs read-only;
   TUI runs read-only, mutating (y/n) and interactive (TTY hand-over).
3. **Phase 2, mutating from the phone** behind `run:mut` and re-auth. Still no destructive.
4. **Phase 3, secret-reading from the phone**, rc only, behind `run:secret`. Opt-in; may stay off.

Destructive from the phone is not a phase.

### Forks for the operator

| # | decision | options | lean |
|---|---|---|---|
| F1 | **Where the runner lives** | (a) `tower/scripts/run_bang.py`, deck-remote and the TUI both exec it; (b) Go code in deck-remote, the TUI calls deck-remote over loopback | **(a)**: one implementation, the TUI needs no daemon, detached runs survive a deck-remote restart, and it matches "journal.py is the only writer" |
| F2 | **Re-auth for mutating/secret on the phone** | (a) laptop-armed 10 min grant, journaled (cheap, but only covers "leaving the laptop shortly"); (b) WebAuthn passkey with Face ID (works away from the laptop, the real fit for a stolen unlocked phone, more code); (c) none, rely on the cap | **(a)** for phase 2's first weeks because it is a day of work; **(b)** before phase 2 is called done; never (c) |
| F3 | **Shell semantics** | (a) one line, one fresh shell, env-carrying lines copy-only, `/ask` emits self-contained `cd … && …` lines; (b) run all of an ask's lines in one shell in order | **(a)**: per-line confirm and per-line rc need per-line runs; (b) makes one confirm cover a chain |
| F4 | **Output policy per class** | (a) ro/mut: redacted stream + 20-line journal tail, secret: rc only; (b) everything rc only, log on disk; (c) full output to the phone | **(a)** |
| F5 | **Who closes the ask** | (a) tower, on its next tick, from `run done rc=0`; (b) the runner journals `ask --to answered` itself | **(a)**: the runner never writes ask records, and tower's fan-out stays in one place |
| F6 | **Ask integrity against injection** | (a) verbatim confirm + deny-by-default classifier, now; (b) tower signs runnable items (HMAC with a key only tower's user can read) and the runner verifies | **(a)** for phases 0–2; revisit (b) before phase 3 or if workers ever get journal write access by design |

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
- Retention of `state/runs/` logs: 30 days proposed; secret-reading logs could be dropped on
  `done` instead of kept redacted.
- Does `/ask` grow a `cwd`/`timeout`/`class_override` schema, or does the runner infer
  everything from the line? Lean: infer, override allowed only to make a line stricter.
