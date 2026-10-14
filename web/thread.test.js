// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { mergeFreshHistory, trimOverlap } = require('./thread.js');

const h = (role, content, ts) => ({ role, content, ts, hist: true });
const live = (role, content, ts) => ({ role, content, ts });

test('reopening a thread does not duplicate history', () => {
  let conv = [];
  const page = [h('me', 'a'), h('reply', 'b'), h('me', 'c')];
  for (let i = 0; i < 3; i++) conv = mergeFreshHistory(page.map((t) => ({ ...t })), conv);
  assert.deepStrictEqual(conv.map((t) => t.content), ['a', 'b', 'c']);
});

test('live rows survive a reload; ones now in the transcript are not doubled', () => {
  const conv = [h('me', 'a'), live('me', 'sent'), live('reply', 'answer'), live('me', 'pending')];
  const fresh = [h('me', 'a'), h('me', 'sent'), h('reply', 'answer')];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.content), ['a', 'sent', 'answer', 'pending']);
});

test('notes and errors are kept', () => {
  const conv = [live('note', 'x'), live('error', 'boom')];
  assert.strictEqual(mergeFreshHistory([h('me', 'a')], conv).length, 3);
});

test('a genuine repeat is kept', () => {
  const conv = [h('me', 'yes', 1000), live('me', 'yes', 5000.5)];
  const fresh = [h('me', 'yes', 1000)];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.content), ['yes', 'yes']);
});

test('older page overlapping the head is trimmed at the boundary only', () => {
  const head = [h('me', 'c'), h('reply', 'd'), h('me', 'e')];
  const older = [h('me', 'ok'), h('reply', 'b'), h('me', 'c'), h('reply', 'd')];
  assert.deepStrictEqual(trimOverlap(older, head).map((t) => t.content), ['ok', 'b']);
  const repeat = [h('me', 'c'), h('reply', 'x')];
  assert.strictEqual(trimOverlap(repeat, head).length, 2);
});

test('a live row is matched to its transcript copy by time', () => {
  const conv = [h('me', 'yes', 1000), live('me', 'yes', 5000.5)];
  const fresh = [h('me', 'yes', 1000), h('reply', 'ok', 4990), h('me', 'yes', 5001)];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.ts), [1000, 4990, 5001]);
});
