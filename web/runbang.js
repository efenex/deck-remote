// Run-bang on the Tower tab (docs/design/run-bang.md §3, §9 D4; phase 2: run-bang-phase2-review
// §5 #7). Pure helpers: a `! ` item's run metadata (tower_model.py "rb", which is
// tower_tui.run_choices) in, the badge / offer out; the SSE frame parser for the run stream; the
// confirm sheet's model from `run_bang.py verify` JSON; invisible characters made visible; and the
// WebAuthn JSON <-> ArrayBuffer codecs for the passkey ceremonies. Loaded before app.js; the same
// file is required by node --test (runbang.test.js).
(function (root) {
  'use strict';

  // tower_tui.SIG_BAD, so the phone says what the laptop says.
  const SIG_BAD = {
    unsigned: '✗unsigned', expired: 'expired', tampered: '✗tampered', 'wrong-kid': '✗wrong kid', 'missing-key': '✗no key',
    'sig-v1': '✗re-sign v2', 'sig-stale-params': '✗stale cwd/timeout',
  };
  // tower scripts/runbang/sign.py HINTS ("<id>" = the ask id).
  // tower runner.py HINTS too (phase 2): the fallback when a refusal carries none.
  const HINTS = {
    'sig-v1': 're-sign as v2: run_bang.py resign --ask <id>',
    'sig-stale-params': 'cwd or timeout changed after signing: re-sign (journal.py add --kind ask ... --sign, or run_bang.py resign)',
    'laptop-only': 'laptop only (phase 2, F8a/F10a): copy the line and run it on the laptop',
    'params-changed': 'the signed cwd/timeout changed since the sheet loaded (re-signed?): reload and confirm again',
    'still-running': "the previous attempt's process group is still alive: wait for it, or stop it on the laptop",
    'arm-missing': 'a prog step runs from the phone only under a Vault-signed arm of this revision: Arm… (power), or run_bang.py seq arm --ask <id> --via tui --payload on the laptop',
    'arm-expired': 'the arm expired (4 h): arm this revision again',
    'arm-revoked': 'the arm was revoked on the laptop: arm again if that was not meant',
    'arm-tampered': 'the arm record does not verify against this revision: arm again',
    'arm-key-unknown': 'the arm was signed by a key version the laptop no longer pins: arm again',
    'arm-exp-too-far': 'the arm expires further out than 4 h: refused, arm again',
    'arm-wrong-profile': 'the arm is not a power arm: arm again',
  };
  const PHASE2 = new Set(['mut', 'secret']); // mut: phase 2 (passkey per line); secret: phase 3
  const RUN_CLASSES = new Set(['ro', 'mut', 'prog']);
  const PASSKEY_CLASSES = new Set(['mut', 'prog']); // one Face ID per line (prog: under the revision's arm, V-2)

  function hint(reason, ask) { return HINTS[reason] ? HINTS[reason].replace('<id>', ask || '<id>') : ''; }

  // badge(rb, ask, nowSec) -> { cls, mark, tone: ok|warn|urgent, hint, offer: run|phase2|laptop|never|blocked, why, passkey }
  // offer: run = a Run button (mut: behind Face ID); phase2 = copy-only, "needs phase 2" (secret,
  // or mut while tower's feed still gates it); laptop = a mut line outside the phone list (copy);
  // never = never phone-runnable (destructive, interactive, copy); blocked = the runner would refuse now (why).
  function badge(rb, ask, nowSec) {
    if (!rb || typeof rb !== 'object') return null;
    const cls = rb.class || 'copy';
    let sig = rb.sig || 'unsigned';
    if (sig === 'ok' && typeof rb.sig_exp === 'number' && rb.sig_exp <= (nowSec || Date.now() / 1000)) sig = 'expired';
    const ok = sig === 'ok';
    const out = {
      cls, sig, mark: ok ? '✓' : (SIG_BAD[sig] || '✗' + sig),
      tone: ok ? 'ok' : (sig === 'tampered' || sig === 'sig-stale-params' ? 'urgent' : 'warn'),
      hint: hint(sig, ask), offer: 'run', why: '', passkey: PASSKEY_CLASSES.has(cls),
    };
    const gated = (cls === 'mut' || cls === 'prog') && !rb.ok && /class|copy-only|phase/.test(String(rb.why || ''));
    // The feed's class is the classifier's (a `pwd` tagged prog reads ro); the runner's arm-* reason
    // is the authority that the step is a prog step waiting for an arm.
    const armWhy = !rb.ok && (String(rb.why || '').match(/\barm-[a-z-]+/) || [])[0];
    if (armWhy && ok) {
      out.offer = 'arm';
      out.why = 'needs an arm (' + armWhy + ')';
      out.hint = hint(armWhy, ask);
      return out;
    }
    if (!RUN_CLASSES.has(cls) || gated) {
      out.offer = PHASE2.has(cls) ? 'phase2' : 'never';
      out.why = PHASE2.has(cls) ? 'class ' + cls + ': copy-only here, needs phase 2' : 'class ' + cls + ': copy-only, never run from the phone';
    } else if (cls === 'mut' && rb.phone_ok === false) {
      out.offer = 'laptop';
      out.why = 'laptop only';
      out.hint = HINTS['laptop-only'];
    } else if (!ok || !rb.ok) {
      out.offer = 'blocked';
      out.why = rb.why || ('signature: ' + sig);
      if (!out.hint) out.hint = hint(String(rb.why || '').replace(/^signature: /, ''), ask);
    }
    return out;
  }

  // runState(run) -> null | { glyph, tone, text } for the item's latest run (feed / done event).
  function runState(run) {
    if (!run || typeof run !== 'object') return null;
    if (run.state === 'running') return { glyph: '⏳', tone: 'warn', text: 'running (attempt ' + (run.attempt || 1) + ')' };
    if (run.state === 'refused') return { glyph: '⊘', tone: 'dim', text: 'refused: ' + (run.reason || '') };
    if (run.state !== 'done') return null;
    if (run.rc === 0) return { glyph: '✓', tone: 'ok', text: 'ran, rc=0' };
    return { glyph: '✗', tone: 'urgent', text: 'rc=' + run.rc + (run.stale ? ' (runner lost: stale)' : '') };
  }

  // parseSSE(buf) -> { events: [{event, data}], rest }: complete frames out, the partial tail kept.
  function parseSSE(buf) {
    const events = [];
    let rest = buf;
    for (;;) {
      const i = rest.indexOf('\n\n');
      if (i < 0) break;
      const frame = rest.slice(0, i);
      rest = rest.slice(i + 2);
      let event = 'message';
      const data = [];
      frame.split('\n').forEach((ln) => {
        if (ln.startsWith('event: ')) event = ln.slice(7);
        else if (ln.startsWith('data: ')) data.push(ln.slice(6));
      });
      let d = data.join('\n');
      try { d = JSON.parse(d); } catch (_) { /* keep the raw string */ }
      events.push({ event, data: d });
    }
    return { events, rest };
  }

  // Invisible or direction-changing code points a signed line may hide (the classifier sends
  // them to copy, review §1.4; the sheet must still show them): C0 but tab, DEL, C1, soft
  // hyphen, the Hangul/Mongolian fillers, zero-width and word-joiner, LRM/RLM/ALM, bidi
  // embeddings/overrides/isolates, BOM, and the interlinear annotation marks.
  const INVISIBLE = /[\u0000-\u0008\u000A-\u001F\u007F-\u009F\u00AD\u034F\u061C\u115F\u1160\u17B4\u17B5\u180E\u200B-\u200F\u202A-\u202E\u2060-\u2064\u2066-\u206F\u3164\uFE00-\uFE0F\uFEFF\uFFA0\uFFF9-\uFFFB]/u;

  // visibleParts(text) -> [{ esc: bool, s }]: text runs and one ⟨U+XXXX⟩ token per invisible
  // code point, for a sheet that builds DOM with textContent (no HTML).
  function visibleParts(text) {
    const out = [];
    let buf = '';
    for (const ch of String(text == null ? '' : text)) {
      if (INVISIBLE.test(ch)) {
        if (buf) { out.push({ esc: false, s: buf }); buf = ''; }
        out.push({ esc: true, s: '⟨U+' + ch.codePointAt(0).toString(16).toUpperCase().padStart(4, '0') + '⟩' });
      } else buf += ch;
    }
    if (buf) out.push({ esc: false, s: buf });
    return out;
  }
  function hasInvisible(text) { return visibleParts(text).some((p) => p.esc); }

  // age(ts, nowMs) -> "5 min ago" | "3 h ago" | "2 d ago" | "" for an ISO string or unix seconds.
  function age(ts, nowMs) {
    let ms = typeof ts === 'number' ? ts * 1000 : Date.parse(ts || '');
    if (!isFinite(ms)) return '';
    const s = Math.max(0, Math.round(((nowMs == null ? Date.now() : nowMs) - ms) / 1000));
    if (s < 90) return s + ' s ago';
    if (s < 3600) return Math.round(s / 60) + ' min ago';
    if (s < 172800) return Math.round(s / 3600) + ' h ago';
    return Math.round(s / 86400) + ' d ago';
  }

  // sheet(v, top, nowMs) -> the confirm sheet's model, from one `run/verify` item v and its
  // envelope top ({ask_ts, sig_ts}) only (review §2: the sheet's DOM comes from verify JSON):
  // { facts: [[label, value]], state: run|laptop|blocked|never|phase2, passkey, why, hint, prev }.
  function sheet(v, top, nowMs) {
    top = top || {};
    const facts = [];
    const sub = v.class === 'mut' ? (v.phone_ok ? ' · phone list' : ' · laptop only') : '';
    facts.push(['class', v.class + (v.class === 'ro' ? ' (read-only)' : v.class === 'mut' ? ' (changes things)' : v.class === 'prog' ? ' (a program; runs under the arm)' : '') + sub]);
    facts.push(['cwd', v.cwd || 'not reported by the runner']);
    facts.push(['timeout', v.timeout ? v.timeout + ' s' : 'not reported by the runner']);
    const sigAge = age(top.sig_ts, nowMs);
    facts.push(['signature', (v.sig === 'ok' ? '✓ signed' : (SIG_BAD[v.sig] || v.sig)) + (sigAge ? ' ' + sigAge : '') + (v.kid ? ' · key ' + v.kid : '')]);
    const askAge = age(top.ask_ts, nowMs);
    if (askAge) facts.push(['proposed', askAge]);
    if (typeof v.exp === 'number') facts.push(['expires', new Date(v.exp * 1000).toISOString().replace('.000Z', 'Z')]);
    const run = v.run && typeof v.run === 'object' ? v.run : null;
    let prev = null;
    if (run && (run.state === 'done' || run.state === 'refused' || run.state === 'running')) {
      prev = { attempt: (run.attempt || 1) + (run.state === 'running' ? 0 : 1), rc: run.rc, state: run.state, tail: Array.isArray(run.tail) ? run.tail.slice(-5) : [] };
      facts.push(['attempt', String(prev.attempt) + (run.state === 'done' ? ' (previous rc=' + run.rc + ')' : run.state === 'refused' ? ' (previous refused: ' + (run.reason || '') + ')' : ' (one is running)')]);
    } else facts.push(['attempt', '1']);
    const out = { facts, passkey: PASSKEY_CLASSES.has(v.class), state: 'run', why: '', hint: v.hint || '', prev };
    if (!RUN_CLASSES.has(v.class)) {
      out.state = PHASE2.has(v.class) ? 'phase2' : 'never';
      out.why = 'Class ' + v.class + (PHASE2.has(v.class) ? ' is copy-only on the phone (needs phase 3).' : ' never runs from the phone.');
    } else if (v.class === 'mut' && v.phone_ok === false) {
      out.state = 'laptop';
      out.why = 'Laptop only: this line\'s effect is not all in the line (a file, a remote hop, an overwrite). Copy it and run it on the laptop.';
      out.hint = out.hint || HINTS['laptop-only'];
    } else if (v.phone_cap === false) {
      out.state = 'phase2';
      out.why = 'Class ' + v.class + ' is copy-only on the phone in this phase.';
    } else if (v.class === 'prog' && /^arm-/.test(String(v.why || ''))) {
      out.state = 'arm';
      out.why = 'This step needs a Vault-signed arm of its revision first (' + v.why + ').';
      out.hint = out.hint || hint(v.why, '');
    } else if (!v.runnable) {
      out.state = 'blocked';
      out.why = 'The runner would refuse: ' + v.why;
      out.hint = out.hint || hint(v.why, '');
    }
    return out;
  }

  // --- V-2: the Vault-signed power arm (docs/design/run-bang-vault-sign.md §2.1, §2.4, §3.1) ---
  // The runner canonicalises once (verify's seq.arm_payload); the sheet recomputes the same string
  // from the fields it shows and refuses to sign when the two differ.

  const ARM_TAG = 'run-bang-arm/v2';
  const ARM_TTL = 4 * 3600; // operator Q-V3; the runner enforces it, the sheet only sanity-checks
  const ARM_SKEW = 3600; // phone clock slack for that sanity check (the runner's clock decides)

  // pyStr(s): Python json.dumps(s) (ensure_ascii): everything outside printable ASCII is \uXXXX
  // (lower-case hex, UTF-16 units), with the five short escapes json uses.
  const PY_SHORT = { '"': '\\"', '\\': '\\\\', '\n': '\\n', '\r': '\\r', '\t': '\\t', '\b': '\\b', '\f': '\\f' };
  function pyStr(s) {
    let out = '"';
    for (let i = 0; i < s.length; i++) {
      const ch = s[i];
      const c = s.charCodeAt(i);
      if (PY_SHORT[ch]) out += PY_SHORT[ch];
      else if (c >= 0x20 && c <= 0x7e) out += ch;
      else out += '\\u' + c.toString(16).padStart(4, '0');
    }
    return out + '"';
  }
  const cpCmp = (a, b) => {
    const x = Array.from(a);
    const y = Array.from(b);
    for (let i = 0; i < Math.min(x.length, y.length); i++) {
      const d = x[i].codePointAt(0) - y[i].codePointAt(0);
      if (d) return d;
    }
    return x.length - y.length;
  };
  // canonJSON(v): tower sign.canon = json.dumps(v, sort_keys=True, separators=(",", ":")). Throws
  // on a non-integer number (Python's float repr is not JavaScript's; a sequence never holds one).
  function canonJSON(v) {
    if (v === null) return 'null';
    if (v === true) return 'true';
    if (v === false) return 'false';
    if (typeof v === 'number') {
      if (!Number.isSafeInteger(v)) throw new Error('canonJSON: non-integer number');
      return String(v);
    }
    if (typeof v === 'string') return pyStr(v);
    if (Array.isArray(v)) return '[' + v.map(canonJSON).join(',') + ']';
    if (typeof v === 'object') return '{' + Object.keys(v).sort(cpCmp).map((k) => pyStr(k) + ':' + canonJSON(v[k])).join(',') + '}';
    throw new Error('canonJSON: ' + typeof v);
  }

  const cleanId = (s) => typeof s === 'string' && s.length > 0 && s.length <= 200 && /^[\x20-\x7e]+$/.test(s);

  // armString(f): the canonical run-bang-arm/v2 string of fields f (varm.canonical).
  function armString(f) {
    return [ARM_TAG, pyStr(f.ask), f.seq_rev, String(f.n), f.seq_sha256, String(f.exp), pyStr(f.device), f.profile].join('\n') + '\n';
  }

  function utf8(s) { return new TextEncoder().encode(s); }
  function b64(bytes) {
    let bin = '';
    for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return typeof btoa === 'function' ? btoa(bin) : Buffer.from(bin, 'binary').toString('base64');
  }
  async function sha256Hex(s) {
    const c = typeof crypto !== 'undefined' ? crypto : globalThis.crypto;
    if (!(c && c.subtle)) throw new Error('no WebCrypto here (needs the https tailnet URL)');
    const d = await c.subtle.digest('SHA-256', utf8(s));
    return Array.from(new Uint8Array(d)).map((x) => x.toString(16).padStart(2, '0')).join('');
  }
  async function canonSha256(seq) { return sha256Hex(canonJSON(seq)); }

  // armCheck(top, askId, nowSec) -> {ok, why, fields, text, b64, sha256, prog: [{index, title, line, sha256}]}
  // from one run/verify response: every field of the string is recomputed from what the sheet
  // shows (ask id from the card, rev / n from the seq block and items, device from arm_device,
  // profile power, each prog line's sha256 from its own bytes, seq_sha256 from the raw data.seq
  // when verify carries it as seq.data) and the string must equal verify's arm_payload and its sha.
  async function armCheck(top, askId, nowSec) {
    const bad = (why) => ({ ok: false, why });
    const seq = top && top.seq;
    if (!seq || typeof seq !== 'object') return bad('not a sequence ask');
    if (seq.sig !== 'ok') return bad('the sequence signature is ' + seq.sig + ': re-sign it on the laptop');
    const f = seq.arm_fields;
    if (!f || typeof seq.arm_payload !== 'string') return bad('the runner sent no arm payload for this device (an older tower?)');
    const items = Array.isArray(top.items) ? top.items : [];
    const now = nowSec == null ? Date.now() / 1000 : nowSec;
    if (f.ask !== askId || top.ask !== askId) return bad('verify names another ask');
    if (!cleanId(f.ask) || !cleanId(f.device)) return bad('the ask or device id is not printable ASCII');
    if (f.device !== top.arm_device) return bad('the payload names another device');
    if (f.seq_rev !== seq.rev || !/^[0-9a-f]{16}$/.test(f.seq_rev)) return bad('the payload names another revision');
    if (f.n !== items.length) return bad('the payload counts ' + f.n + ' steps, verify lists ' + items.length);
    if (f.profile !== 'power') return bad('not a power arm');
    if (!/^[0-9a-f]{64}$/.test(String(f.seq_sha256))) return bad('bad seq_sha256');
    if (!Number.isSafeInteger(f.exp) || f.exp <= now - ARM_SKEW || f.exp > now + ARM_TTL + ARM_SKEW) return bad('the arm expiry is not within 4 h of now: check the phone clock, reload');
    if (seq.data && typeof seq.data === 'object' && (await canonSha256(seq.data)) !== f.seq_sha256) return bad('data.seq does not hash to the payload');
    const steps = new Map((Array.isArray(seq.steps) ? seq.steps : []).map((s) => [s.index, s]));
    const prog = [];
    for (const it of items) {
      if (it.class !== 'prog') continue;
      if (typeof it.line !== 'string' || (await sha256Hex(it.line)) !== it.sha256) return bad('step ' + it.index + ': the line does not match its hash');
      prog.push({ index: it.index, title: (steps.get(it.index) || {}).title || '', line: it.line, sha256: it.sha256 });
    }
    if (!prog.length) return bad('no prog step in this revision: nothing to arm');
    const text = armString(f);
    const sha = await sha256Hex(text);
    if (text !== seq.arm_payload || sha !== seq.arm_payload_sha256) return bad('verify and card disagree: reload');
    return { ok: true, why: '', fields: f, text, b64: b64(utf8(text)), sha256: sha, prog };
  }

  // armDiff(prog, last) -> {rows: [{index, title, line, state: new|changed|unchanged}], removed: [title]}
  // against the last revision this phone armed for the ask (last = {rev, at, steps: [{index,
  // title, sha256}]} or null): unchanged steps (same bytes) are collapsed and named (review P2-6).
  function armDiff(prog, last) {
    const was = new Map(((last && last.steps) || []).map((s) => [s.sha256, s]));
    const byIndex = new Map(((last && last.steps) || []).map((s) => [s.index, s]));
    const now = new Set(prog.map((p) => p.sha256));
    const rows = prog.map((p) => Object.assign({}, p, { state: !last ? 'new' : was.has(p.sha256) ? 'unchanged' : byIndex.has(p.index) ? 'changed' : 'new' }));
    const removed = ((last && last.steps) || []).filter((s) => !now.has(s.sha256) && !prog.some((p) => p.index === s.index)).map((s) => s.title || 'step ' + s.index);
    return { rows, removed };
  }

  // --- Vault, from the PWA (design §2.4 step 2-3). The token is a return value, never stored:
  // the caller keeps it in a local, spends it on one sign and drops it. No cookies, no referrer.
  function vaultInit(method, headers, body) {
    return { method, headers: Object.assign({ 'X-Vault-Request': 'true' }, body ? { 'Content-Type': 'application/json' } : {}, headers || {}),
      body: body ? JSON.stringify(body) : undefined, credentials: 'omit', cache: 'no-store', referrerPolicy: 'no-referrer', redirect: 'error', mode: 'cors' };
  }
  async function vaultErr(res, what) {
    let errs = '';
    try { errs = ((await res.json()).errors || []).map(String).join('; ').slice(0, 200); } catch (_) { /* no body */ }
    return new Error(what + ': HTTP ' + res.status + (errs ? ' (' + errs + ')' : ''));
  }
  // vaultAuthURL(cfg, nonce, fetchFn) -> {url, state}: the Forgejo URL to open and the state Vault put in it.
  async function vaultAuthURL(cfg, nonce, fetchFn) {
    const res = await fetchFn(cfg.vault + '/v1/auth/' + cfg.mount + '/oidc/auth_url', vaultInit('POST', null, { role: cfg.role, redirect_uri: cfg.redirect, client_nonce: nonce }));
    if (!res.ok) throw await vaultErr(res, 'Vault auth_url');
    const url = ((await res.json()).data || {}).auth_url;
    if (typeof url !== 'string' || !/^https:\/\//.test(url)) throw new Error('Vault auth_url: no https URL (is the redirect allowed on role ' + cfg.role + '?)');
    return { url, state: new URL(url).searchParams.get('state') || '' };
  }
  // vaultLogin(cfg, cb, nonce, fetchFn) -> the client token (the caller must not store it).
  async function vaultLogin(cfg, cb, nonce, fetchFn) {
    const q = '?state=' + encodeURIComponent(cb.state) + '&code=' + encodeURIComponent(cb.code) + '&client_nonce=' + encodeURIComponent(nonce);
    const res = await fetchFn(cfg.vault + '/v1/auth/' + cfg.mount + '/oidc/callback' + q, vaultInit('GET'));
    if (!res.ok) throw await vaultErr(res, 'Vault login');
    const tok = ((await res.json()).auth || {}).client_token;
    if (typeof tok !== 'string' || !tok) throw new Error('Vault login: no token in the answer');
    return tok;
  }
  // vaultSign(cfg, token, input64, fetchFn) -> "vault:v<N>:<base64>".
  async function vaultSign(cfg, token, input64, fetchFn) {
    const res = await fetchFn(cfg.vault + '/v1/' + cfg.transit + '/sign/' + cfg.key, vaultInit('POST', { 'X-Vault-Token': token }, { input: input64 }));
    if (!res.ok) throw await vaultErr(res, 'Vault sign');
    const sig = ((await res.json()).data || {}).signature;
    if (typeof sig !== 'string' || !/^vault:v[1-9][0-9]{0,5}:[A-Za-z0-9+/]{86}==$/.test(sig)) throw new Error('Vault sign: not an ed25519 signature');
    return sig;
  }
  function nonce() {
    const u = new Uint8Array(24);
    (typeof crypto !== 'undefined' ? crypto : globalThis.crypto).getRandomValues(u);
    return bufToB64u(u);
  }

  // --- WebAuthn JSON codecs (the server speaks go-webauthn's base64url JSON) ---
  function b64uToBuf(s) {
    const b = String(s).replace(/-/g, '+').replace(/_/g, '/');
    const bin = (typeof atob === 'function' ? atob : (x) => Buffer.from(x, 'base64').toString('binary'))(b + '==='.slice((b.length + 3) % 4));
    const u = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i);
    return u.buffer;
  }
  function bufToB64u(buf) {
    const u = new Uint8Array(buf instanceof ArrayBuffer ? buf : buf.buffer.slice(buf.byteOffset, buf.byteOffset + buf.byteLength));
    let bin = '';
    for (let i = 0; i < u.length; i++) bin += String.fromCharCode(u[i]);
    const b = typeof btoa === 'function' ? btoa(bin) : Buffer.from(bin, 'binary').toString('base64');
    return b.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }
  const descs = (list) => (list || []).map((d) => Object.assign({}, d, { id: b64uToBuf(d.id) }));
  // creationOptions({publicKey}) / requestOptions({publicKey}) -> the publicKey dictionary
  // navigator.credentials.create/get takes.
  function creationOptions(o) {
    const pk = Object.assign({}, (o && o.publicKey) || o);
    pk.challenge = b64uToBuf(pk.challenge);
    pk.user = Object.assign({}, pk.user, { id: b64uToBuf(pk.user.id) });
    if (pk.excludeCredentials) pk.excludeCredentials = descs(pk.excludeCredentials);
    return pk;
  }
  function requestOptions(o) {
    const pk = Object.assign({}, (o && o.publicKey) || o);
    pk.challenge = b64uToBuf(pk.challenge);
    if (pk.allowCredentials) pk.allowCredentials = descs(pk.allowCredentials);
    return pk;
  }
  // credentialJSON(cred) -> the JSON go-webauthn parses (create and get responses alike).
  function credentialJSON(cred) {
    const r = cred.response;
    const resp = { clientDataJSON: bufToB64u(r.clientDataJSON) };
    if (r.attestationObject) {
      resp.attestationObject = bufToB64u(r.attestationObject);
      if (typeof r.getTransports === 'function') resp.transports = r.getTransports();
    }
    if (r.authenticatorData) resp.authenticatorData = bufToB64u(r.authenticatorData);
    if (r.signature) resp.signature = bufToB64u(r.signature);
    if (r.userHandle) resp.userHandle = bufToB64u(r.userHandle);
    return {
      id: cred.id, rawId: bufToB64u(cred.rawId), type: cred.type,
      authenticatorAttachment: cred.authenticatorAttachment || undefined,
      response: resp,
      clientExtensionResults: typeof cred.getClientExtensionResults === 'function' ? cred.getClientExtensionResults() : {},
    };
  }

  const api = {
    badge, runState, parseSSE, hint, SIG_BAD, HINTS, PASSKEY_CLASSES,
    visibleParts, hasInvisible, age, sheet, b64uToBuf, bufToB64u, creationOptions, requestOptions, credentialJSON,
    ARM_TTL, pyStr, canonJSON, armString, sha256Hex, canonSha256, armCheck, armDiff, b64, utf8,
    vaultAuthURL, vaultLogin, vaultSign, nonce,
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckRunBang = api;
})(typeof window !== 'undefined' ? window : globalThis);
