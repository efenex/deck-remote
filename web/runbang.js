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
  };
  const PHASE2 = new Set(['mut', 'secret']); // mut: phase 2 (passkey per line); secret: phase 3
  const RUN_CLASSES = new Set(['ro', 'mut']);
  const PASSKEY_CLASSES = new Set(['mut']); // one Face ID per line

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
    const gated = cls === 'mut' && !rb.ok && /class|copy-only|phase/.test(String(rb.why || ''));
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
    facts.push(['class', v.class + (v.class === 'ro' ? ' (read-only)' : v.class === 'mut' ? ' (changes things)' : '') + sub]);
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
    } else if (!v.runnable) {
      out.state = 'blocked';
      out.why = 'The runner would refuse: ' + v.why;
      out.hint = out.hint || hint(v.why, '');
    }
    return out;
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
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckRunBang = api;
})(typeof window !== 'undefined' ? window : globalThis);
