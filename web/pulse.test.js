// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const P = require('./pulse.js');

test('level buckets follow claude-pulse (quota 50/80, ctx 70/90)', () => {
  assert.deepStrictEqual(['quota'].map((k) => [0, 49.9, 50, 79.9, 80, 100].map((p) => P.level(p, k))), [['ok', 'ok', 'warn', 'warn', 'hot', 'hot']]);
  assert.deepStrictEqual([0, 69.9, 70, 89.9, 90, 100].map((p) => P.level(p, 'ctx')), ['ok', 'ok', 'warn', 'warn', 'hot', 'hot']);
});

test('fmtTokens', () => {
  assert.strictEqual(P.fmtTokens(950), '950');
  assert.strictEqual(P.fmtTokens(44033), '44k');
  assert.strictEqual(P.fmtTokens(1000000), '1M');
  assert.strictEqual(P.fmtTokens(1250000), '1.3M');
  assert.strictEqual(P.fmtTokens(200000), '200k');
});

test('fmtReset', () => {
  const now = 1792578000 * 1000;
  assert.strictEqual(P.fmtReset(1792578000 + 42 * 60, now), '42m');
  assert.strictEqual(P.fmtReset(1792578000 + 3720, now), '1h02m');
  assert.strictEqual(P.fmtReset(1792578000 + 3 * 86400 + 4 * 3600 + 60, now), '3d4h');
  assert.strictEqual(P.fmtReset(1792578000 - 5, now), '');
  assert.strictEqual(P.fmtReset(0, now), '');
  assert.strictEqual(P.fmtReset(1792578000 + 20, now), '1m');
});

test('fmtAge shows only stale records', () => {
  const now = 1792578000 * 1000;
  assert.strictEqual(P.fmtAge(1792578000 - 120, now), '');
  assert.strictEqual(P.fmtAge(1792578000 - 25 * 60, now), '25m old');
  assert.strictEqual(P.fmtAge(1792578000 - 3 * 3600, now), '3h old');
  assert.strictEqual(P.fmtAge(0, now), '');
});

test('ctxMini / ctxFull', () => {
  assert.strictEqual(P.ctxMini(null), '');
  assert.strictEqual(P.ctxMini({}), '');
  const m = P.ctxMini({ pct: 91, tokens: 910000, window: 1000000, model: 'Opus 5.5' });
  assert.match(m, /lv-hot/);
  assert.match(m, /width:91%/);
  assert.match(m, /<b>91%<\/b>/);
  assert.match(m, /910k \/ 1M tokens · Opus 5.5/);
  const f = P.ctxFull({ pct: 4, tokens: 44033, window: 1000000, model: 'Sonnet 5.5' });
  assert.match(f, /lv-ok/);
  assert.match(f, /44k \/ 1M/);
  assert.match(f, /Sonnet 5\.5/);
  // pct is clamped for the bar width but the number is honest up to 100.
  assert.match(P.ctxMini({ pct: 130 }), /width:100%/);
});

test('model names are escaped', () => {
  assert.doesNotMatch(P.ctxFull({ pct: 1, model: '<img onerror=x>' }), /<img/);
  assert.match(P.quotaStrip([{ groups: ['<b>'], fiveHour: { pct: 1 } }], 0), /&lt;b&gt;/);
});

test('quotaStrip renders one block per account and a missing window as a dash', () => {
  const now = 1792578000 * 1000;
  const html = P.quotaStrip([
    { groups: ['ai-tooling', 'upstream'], fiveHour: { pct: 10, resetsAt: 1792578000 + 3720 }, sevenDay: { pct: 85, resetsAt: 1792578000 + 2 * 86400 }, asOf: 1792578000 - 60 },
    { groups: ['luna'], sevenDay: { pct: 55 }, asOf: 1792578000 - 3600 },
  ], now);
  assert.strictEqual((html.match(/class="q-acct/g) || []).length, 2);
  assert.strictEqual((html.match(/class="q-acct stale"/g) || []).length, 1); // only the hour-old one is dimmed
  assert.match(html, /ai-tooling · upstream/);
  assert.match(html, /↻ 1h02m/);
  assert.match(html, /q-win lv-hot/); // 7d 85 %
  assert.match(html, /q-win none/); // luna has no 5h window
  assert.match(html, /60m old/);
  assert.strictEqual(P.quotaStrip([], now), '');
  assert.strictEqual(P.quotaStrip(undefined, now), '');
});

test('quotaFor / quotaInline', () => {
  const q = [{ groups: ['a', 'b'], fiveHour: { pct: 12, resetsAt: 0 }, sevenDay: { pct: 20 } }, { groups: ['c'] }];
  assert.strictEqual(P.quotaFor(q, 'b'), q[0]);
  assert.strictEqual(P.quotaFor(q, 'zzz'), null);
  assert.strictEqual(P.quotaFor(undefined, 'a'), null);
  assert.match(P.quotaInline(q[0], 0), /5h <b>12%<\/b>.*7d <b>20%<\/b>/);
  assert.strictEqual(P.quotaInline(q[1], 0), '');
  assert.strictEqual(P.quotaInline(null, 0), '');
});
