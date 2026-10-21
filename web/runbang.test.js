// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const R = require('./runbang.js');

const rb = (o) => Object.assign({ index: 0, sha256: 'a'.repeat(64), class: 'ro', sig: 'ok', ok: true, why: null, run: null }, o);

test('a signed runnable ro line offers Run', () => {
  const b = R.badge(rb({}), 'ask1');
  assert.strictEqual(b.offer, 'run');
  assert.strictEqual(b.mark, '✓');
  assert.strictEqual(b.tone, 'ok');
});

test('mut and secret are copy-only with "needs phase 2"; destructive/interactive/copy never run', () => {
  for (const c of ['mut', 'secret']) {
    const b = R.badge(rb({ class: c, ok: false, why: 'class ' + c + ' is copy-only in phase 1' }), 'a');
    assert.strictEqual(b.offer, 'phase2');
    assert.match(b.why, /needs phase 2/);
  }
  for (const c of ['destructive', 'interactive', 'copy']) assert.strictEqual(R.badge(rb({ class: c, ok: false }), 'a').offer, 'never');
});

test('sig-v1 and sig-stale-params block with the re-sign hint', () => {
  const v1 = R.badge(rb({ sig: 'sig-v1', ok: false, why: 'signature: sig-v1' }), 'ask1');
  assert.strictEqual(v1.offer, 'blocked');
  assert.strictEqual(v1.mark, '✗re-sign v2');
  assert.strictEqual(v1.hint, 're-sign as v2: run_bang.py resign --ask ask1');
  const st = R.badge(rb({ sig: 'sig-stale-params', ok: false, why: 'signature: sig-stale-params' }), 'ask1');
  assert.strictEqual(st.tone, 'urgent');
  assert.match(st.hint, /cwd or timeout changed after signing/);
  const un = R.badge(rb({ sig: 'unsigned', ok: false, why: 'signature: unsigned' }), 'a');
  assert.strictEqual(un.offer, 'blocked');
  assert.strictEqual(un.hint, '');
});

test('expiry is re-checked at draw time; already ran rc=0 blocks', () => {
  assert.strictEqual(R.badge(rb({ sig_exp: 100 }), 'a', 200).offer, 'blocked');
  assert.strictEqual(R.badge(rb({ ok: false, why: 'already ran, rc=0' }), 'a').why, 'already ran, rc=0');
  assert.strictEqual(R.badge(null), null);
});

test('runState glyphs', () => {
  assert.strictEqual(R.runState({ state: 'done', rc: 0 }).glyph, '✓');
  assert.strictEqual(R.runState({ state: 'done', rc: 2 }).text, 'rc=2');
  assert.strictEqual(R.runState({ state: 'running', attempt: 2 }).glyph, '⏳');
  assert.strictEqual(R.runState({ state: 'refused', reason: 'expired' }).text, 'refused: expired');
  assert.strictEqual(R.runState(null), null);
});

test('parseSSE keeps a partial frame for the next chunk', () => {
  const a = R.parseSSE('event: line\ndata: "x"\n\nevent: done\ndata: {"rc":0');
  assert.deepStrictEqual(a.events, [{ event: 'line', data: 'x' }]);
  const b = R.parseSSE(a.rest + '}\n\n');
  assert.deepStrictEqual(b.events, [{ event: 'done', data: { rc: 0 } }]);
  assert.strictEqual(b.rest, '');
});

// --- phase 2 (#7): mut behind Face ID, the confirm sheet's model, invisible characters, codecs ---

test('mut: Run behind a passkey when the feed offers it; laptop-only lines are copy', () => {
  const run = R.badge(rb({ class: 'mut', ok: true, phone_ok: true }), 'a');
  assert.strictEqual(run.offer, 'run');
  assert.strictEqual(run.passkey, true);
  const lap = R.badge(rb({ class: 'mut', ok: true, phone_ok: false }), 'a');
  assert.strictEqual(lap.offer, 'laptop');
  assert.match(lap.hint, /laptop only/);
  assert.strictEqual(R.badge(rb({ class: 'secret', ok: true }), 'a').offer, 'phase2');
  assert.strictEqual(R.badge(rb({}), 'a').passkey, false);
});

test('visibleParts shows zero-width, bidi and control characters, keeps tabs and plain text', () => {
  const p = R.visibleParts('git pu​ll‮ && x\t­y');
  assert.deepStrictEqual(p.map((x) => x.s), ['git pu', '⟨U+200B⟩', 'll', '⟨U+202E⟩', ' && x\t', '⟨U+00AD⟩', 'y']);
  assert.ok(R.hasInvisible('a⁦b'));
  assert.ok(R.hasInvisible('a\u0007b'));
  assert.ok(R.hasInvisible('a﻿b'));
  assert.ok(!R.hasInvisible('git pull --ff-only\torigin main'));
  assert.ok(!R.hasInvisible('écho ünïcode ✓ 日本'));
});

test('age reads ISO strings and unix seconds', () => {
  const now = Date.parse('2026-10-21T23:00:00Z');
  assert.strictEqual(R.age('2026-10-21T20:00:00Z', now), '3 h ago');
  assert.strictEqual(R.age(now / 1000 - 600, now), '10 min ago');
  assert.strictEqual(R.age(null, now), '');
});

const item = (o) => Object.assign({ index: 1, sha256: 'b'.repeat(64), class: 'mut', phone_ok: true, phone_cap: true, passkey: true, sig: 'ok', runnable: true, why: null, run: null, cwd: '/Users/op/src', timeout: 900, exp: 1900000000, kid: 'k1' }, o);
const top = { ask_ts: '2026-10-21T20:00:00Z', sig_ts: '2026-10-21T22:00:00Z' };

test('sheet: every review §2 field comes from verify JSON', () => {
  const m = R.sheet(item({}), top, Date.parse('2026-10-21T23:00:00Z'));
  const f = Object.fromEntries(m.facts);
  assert.strictEqual(m.state, 'run');
  assert.strictEqual(m.passkey, true);
  assert.match(f.class, /mut .*phone list/);
  assert.strictEqual(f.cwd, '/Users/op/src');
  assert.strictEqual(f.timeout, '900 s');
  assert.match(f.signature, /✓ signed 1 h ago · key k1/);
  assert.strictEqual(f.proposed, '3 h ago');
  assert.strictEqual(f.attempt, '1');
});

test('sheet: previous attempt, laptop-only, runner refusal, never', () => {
  const again = R.sheet(item({ run: { state: 'done', rc: 2, attempt: 1, tail: ['a', 'b'] } }), top);
  assert.match(Object.fromEntries(again.facts).attempt, /^2 \(previous rc=2\)/);
  assert.deepStrictEqual(again.prev.tail, ['a', 'b']);
  const lap = R.sheet(item({ phone_ok: false }), top);
  assert.strictEqual(lap.state, 'laptop');
  assert.match(lap.why, /Laptop only/);
  const pre = R.sheet(item({ runnable: false, why: 'class:mut' }), top);
  assert.strictEqual(pre.state, 'blocked');
  assert.match(pre.why, /class:mut/);
  assert.strictEqual(R.sheet(item({ class: 'destructive', passkey: false, phone_cap: false }), top).state, 'never');
  assert.strictEqual(R.sheet(item({ class: 'ro', passkey: false }), top).passkey, false);
});

test('WebAuthn codecs: base64url round trip, options to buffers, credential to JSON', () => {
  assert.strictEqual(R.bufToB64u(R.b64uToBuf('AQIDBA-_')), 'AQIDBA-_');
  const c = R.creationOptions({ publicKey: { challenge: 'AQID', user: { id: 'BAU', name: 'p' }, rp: { id: 'x' }, excludeCredentials: [{ type: 'public-key', id: 'Bg' }] } });
  assert.ok(c.challenge instanceof ArrayBuffer && c.user.id instanceof ArrayBuffer && c.excludeCredentials[0].id instanceof ArrayBuffer);
  assert.strictEqual(c.user.name, 'p');
  const g = R.requestOptions({ publicKey: { challenge: 'AQID', allowCredentials: [{ type: 'public-key', id: 'Bg' }], userVerification: 'required' } });
  assert.deepStrictEqual(Array.from(new Uint8Array(g.allowCredentials[0].id)), [6]);
  assert.strictEqual(g.userVerification, 'required');
  const buf = (a) => new Uint8Array(a).buffer;
  const j = R.credentialJSON({
    id: 'Bg', rawId: buf([6]), type: 'public-key', authenticatorAttachment: 'platform',
    response: { clientDataJSON: buf([1]), authenticatorData: buf([2]), signature: buf([3]), userHandle: null },
    getClientExtensionResults: () => ({}),
  });
  assert.deepStrictEqual(j, { id: 'Bg', rawId: 'Bg', type: 'public-key', authenticatorAttachment: 'platform', response: { clientDataJSON: 'AQ', authenticatorData: 'Ag', signature: 'Aw' }, clientExtensionResults: {} });
});
