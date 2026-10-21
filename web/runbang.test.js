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
