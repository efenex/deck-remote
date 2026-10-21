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
  assert.doesNotMatch(P.quotaStrip([{ fiveHour: { pct: 1, resetsAt: '<img>' } }], 0), /<img/);
});

const NOW = 1792578000; // 2026-10-21T10:20:00Z (a Wednesday)

test('fmtResetAt: time within a day, weekday + time beyond; UTC here, device zone in the app', () => {
  assert.strictEqual(P.fmtResetAt(NOW + 3 * 3600, NOW * 1000, 'UTC'), '13:20');
  assert.strictEqual(P.fmtResetAt(1793077200, NOW * 1000, 'UTC'), 'Tue 05:00');
  assert.strictEqual(P.fmtResetAt(0, NOW * 1000, 'UTC'), '');
  assert.strictEqual(P.fmtResetAt(NOW + 3 * 3600, NOW * 1000, 'Europe/Brussels'), '15:20');
});

test('quotaStrip: one account line, Week first, "Week 28% resets Tue 05:00" and "5h 0%"', () => {
  const html = P.quotaStrip([{ sevenDay: { pct: 28, resetsAt: 1793077200 }, fiveHour: { pct: 0, resetsAt: NOW + 3 * 3600 }, asOf: NOW - 30 }], NOW * 1000, 'UTC');
  assert.strictEqual((html.match(/class="q-acct/g) || []).length, 1);
  assert.ok(html.indexOf('Week') < html.indexOf('5h'));
  assert.match(html, /<b>28%<\/b>/);
  assert.match(html, /resets Tue 05:00/);
  assert.match(html, /<b>0%<\/b>/);
  assert.match(html, /resets 13:20/);
  assert.doesNotMatch(html, /as of/); // fresh record: no age tag
  assert.doesNotMatch(html, /stale/);
});

test('quotaStrip: green/yellow/red bars, as-of tag after 10 min, dim after an hour, other-account note', () => {
  const at = (pct, asOf, extra) => P.quotaStrip([Object.assign({ sevenDay: { pct, resetsAt: NOW + 86400 * 5 }, asOf }, extra || {})], NOW * 1000, 'UTC');
  assert.match(at(28, NOW), /q-line lv-ok/);
  assert.match(at(60, NOW), /q-line lv-warn/);
  assert.match(at(85, NOW), /q-line lv-hot/);
  assert.match(at(28, NOW - 14 * 60), /as of 14m/);
  assert.doesNotMatch(at(28, NOW - 14 * 60), /q-acct stale/);
  assert.match(at(28, NOW - 3 * 3600), /q-acct stale/);
  assert.match(at(28, NOW - 3 * 3600), /as of 3h/);
  assert.match(at(28, NOW, { others: 1 }), /\+1 other account</);
  assert.match(at(28, NOW, { others: 2 }), /\+2 other accounts</);
});

test('quotaStrip: an expired window reads 0% (never the stale number); a missing one is a dash', () => {
  const html = P.quotaStrip([{ sevenDay: { pct: 28, resetsAt: NOW + 86400 }, fiveHour: { pct: 0, expired: true }, asOf: NOW }], NOW * 1000, 'UTC');
  assert.match(html, /5h<\/span>.*<b>0%<\/b>.*just reset/);
  const none = P.quotaStrip([{ sevenDay: { pct: 28, resetsAt: NOW + 86400 }, asOf: NOW }], NOW * 1000, 'UTC');
  assert.match(none, /q-line none/);
  assert.match(none, /—/);
  assert.strictEqual(P.quotaStrip([], NOW), '');
  assert.strictEqual(P.quotaStrip(undefined, NOW), '');
});

test('quotaFor ignores the group (limits are per account); quotaInline "Week 28% · 5h 0%"', () => {
  const q = [{ sevenDay: { pct: 28 }, fiveHour: { pct: 0 } }];
  assert.strictEqual(P.quotaFor(q, 'any-group'), q[0]);
  assert.strictEqual(P.quotaFor([]), null);
  assert.strictEqual(P.quotaFor(undefined), null);
  assert.match(P.quotaInline(q[0]), /Week <b>28%<\/b>.*5h <b>0%<\/b>/);
  assert.match(P.quotaInline({ sevenDay: { pct: 28 }, fiveHour: { pct: 55, expired: true } }), /5h <b>0%<\/b>/);
  assert.strictEqual(P.quotaInline({}), '');
  assert.strictEqual(P.quotaInline(null), '');
});
