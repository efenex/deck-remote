// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { banner } = require('./version.js');

const NOW = Date.parse('2026-10-19T20:00:00Z');

test('matching shas and a clean web dir: no banner', () => {
  assert.strictEqual(banner({ server_sha: 'abc1234', web_sha: 'abc1234', web_dirty: false, built_at: '2026-10-19T19:00:00Z' }, NOW), null);
});

test('mismatch warns with both shas and the time since the server build', () => {
  const b = banner({ server_sha: 'abc1234', web_sha: 'def5678', built_at: '2026-10-18T14:09:00Z' }, NOW);
  assert.strictEqual(b.level, 'warn');
  assert.match(b.text, /server abc1234 ≠ web def5678: rebuild \+ restart needed/);
  assert.match(b.text, /server built 29\.9 h ago/);
});

test('dirty web alone is an info note; with a mismatch both are shown', () => {
  const only = banner({ server_sha: 'abc1234', web_sha: 'abc1234', web_dirty: true }, NOW);
  assert.strictEqual(only.level, 'info');
  assert.strictEqual(only.text, 'web has uncommitted changes');
  const both = banner({ server_sha: 'a111111', web_sha: 'b222222', web_dirty: true, built_at: 'garbage' }, NOW);
  assert.match(both.text, /rebuild \+ restart needed · web has uncommitted changes/);
  assert.doesNotMatch(both.text, /server built/);
});

test('unknown shas never alarm; the dismiss key changes with the situation', () => {
  assert.strictEqual(banner({ server_sha: 'unknown', web_sha: 'abc1234' }, NOW), null);
  assert.strictEqual(banner({ server_sha: 'abc1234', web_sha: '' }, NOW), null);
  assert.strictEqual(banner(null, NOW), null);
  const k1 = banner({ server_sha: 'a111111', web_sha: 'b222222' }, NOW).key;
  const k2 = banner({ server_sha: 'a111111', web_sha: 'c333333' }, NOW).key;
  assert.notStrictEqual(k1, k2);
});
