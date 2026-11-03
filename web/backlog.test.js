// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const B = require('./backlog.js');

test('slug matches tower_actions.go towerSlug (TestTowerSlugMatchesTab)', () => {
  assert.strictEqual(B.slug('T-42.fix_Thing'), 't-42-fix-thing');
  assert.strictEqual(B.slug('sg_Next.Thing'), 'sg-next-thing');
  assert.strictEqual(B.slug('--'), 'x');
  assert.strictEqual(B.slug('a'.repeat(70)), 'a'.repeat(60));
});

test('request records: tower ids and data for start / defer / drop', () => {
  assert.deepStrictEqual(B.request('start', 'T-42.fix_Thing'), { id: 'pick-backlog-t-42-fix-thing', data: { text: 'operator picked backlog task T-42.fix_Thing: dispatch now', task: 'T-42.fix_Thing', from: 'tui' } });
  assert.deepStrictEqual(B.request('defer', 'T-42.fix_Thing', '2026-11-04'), { id: 'pick-defer-t-42-fix-thing', data: { text: 'operator deferred backlog task T-42.fix_Thing until 2026-11-04', task: 'T-42.fix_Thing', not_before: '2026-11-04T08:00:00Z', from: 'tui' } });
  assert.deepStrictEqual(B.request('drop', 'T-42.fix_Thing'), { id: 'pick-drop-t-42-fix-thing', data: { text: 'operator dropped backlog task T-42.fix_Thing', task: 'T-42.fix_Thing', from: 'tui' } });
});

test('act bodies carry the exact task id; defer needs a real date', () => {
  assert.deepStrictEqual(B.actBody('start', 'T-42.fix_Thing'), { op: 'backlog_start', id: 'T-42.fix_Thing' });
  assert.deepStrictEqual(B.actBody('defer', 'T-42.fix_Thing', '2026-11-04'), { op: 'backlog_defer', id: 'T-42.fix_Thing', not_before: '2026-11-04T08:00:00Z' });
  assert.deepStrictEqual(B.actBody('drop', 'T-42.fix_Thing'), { op: 'backlog_drop', id: 'T-42.fix_Thing' });
  assert.throws(() => B.actBody('defer', 'x', ''));
  assert.throws(() => B.actBody('defer', 'x', '04/11/2026'));
  assert.throws(() => B.actBody('defer', 'x', '2026-13-01'));
});

test('tomorrow is the next local calendar day, also across a month end', () => {
  assert.strictEqual(B.tomorrow(new Date(2026, 10, 3, 23, 30).getTime()), '2026-11-04');
  assert.strictEqual(B.tomorrow(new Date(2026, 11, 31, 9).getTime()), '2027-01-01');
});

test('rowView: age, deps, not_before, truncated title', () => {
  const now = Date.parse('2026-11-03T12:00:00Z');
  const v = B.rowView({ id: 'T-1', title: 'x'.repeat(200), age_days: 12.7, goal: 'g-home', deps_open: ['T-0', 'T-00'], not_before: '2026-11-10T08:00:00Z' }, now);
  assert.strictEqual(v.age, '12 d');
  assert.strictEqual(v.title.length, 90);
  assert.ok(v.title.endsWith('…'));
  assert.strictEqual(v.waits, 'waits on T-0, T-00');
  assert.strictEqual(v.notBefore, 'not before 2026-11-10');
  const w = B.rowView({ id: 'T-2', title: 't', created: '2026-10-31T12:00:00Z', deps_open: [] }, now);
  assert.strictEqual(w.age, '3 d');
  assert.strictEqual(w.waits, '');
  assert.strictEqual(w.notBefore, '');
  assert.strictEqual(B.rowView({ id: 'T-3' }, now).age, '? d');
});

test('section: header from counts.backlog; null without the key (older tower)', () => {
  assert.strictEqual(B.section({ sections: [] }), null);
  assert.strictEqual(B.section(null), null);
  assert.strictEqual(B.section({ backlog: [{ id: 'a' }], counts: { backlog: 7 } }).header, 'BACKLOG (7)');
  assert.strictEqual(B.section({ backlog: [{ id: 'a' }, {}] }).header, 'BACKLOG (2)');
  assert.strictEqual(B.section({ backlog: [{ id: 'a' }, {}] }).rows.length, 1);
});

test('queued when any of the three request ids is open in the overview or was posted from here', () => {
  const model = { open_requests: ['pick-drop-t-1'], sections: [{ items: [
    { kind: 'request', id: 'pick-backlog-t-2', status: 'waiting' }, { kind: 'request', id: 'pick-defer-t-3', status: 'done' }] }] };
  const open = B.openRequestIds(model);
  assert.ok(B.isQueued('T-1', open, new Set()));
  assert.ok(B.isQueued('T-2', open, new Set()));
  assert.ok(!B.isQueued('T-3', open, new Set()), 'a done request does not hold the row');
  assert.ok(B.isQueued('T-3', open, new Set(['pick-defer-t-3'])));
});
