'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { fresh, badge, rowText } = require('./subagents.js');
const now = Date.parse('2026-11-11T09:00:10Z');
const summary = { availability: 'available', complete: true, running: 1, unknown: 0, freshUntil: new Date(now + 2000).toISOString() };
test('fresh available evidence announces a running count regardless of coverage; expiry is local', () => {
  assert.equal(badge(summary, now), '1 subagent running');
  assert.equal(badge(summary, now + 2000), '1 last seen active · status stale');
  assert.equal(fresh({ ...summary, complete: false }, now), true);
  assert.equal(badge({ ...summary, complete: false }, now), '1 subagent running · partial');
  assert.equal(badge({ ...summary, complete: false }, now + 2000), '1 last seen active · status stale');
  assert.doesNotMatch(badge({ ...summary, availability: 'unavailable' }, now), /running/);
  assert.equal(badge(null, now), '');
  assert.equal(badge({ ...summary, running: 0 }, now), '');
  assert.match(badge({ ...summary, running: 0, unknown: 1 }, now), /status unknown/);
});
test('terminal outcomes remain distinct; stale running becomes unknown', () => {
  const child = { label: 'Explore', status: 'running', freshness: 'fresh', evidenceAt: '2026-11-11T09:00:06Z', linked: true, availability: 'available', lastAction: { name: 'Read' } };
  assert.match(rowText(child, now), /Explore · running · Read · fresh evidence/);
  assert.match(rowText(child, now + 31000), /unknown · Read · status stale/);
  for (const status of ['finished', 'failed', 'succeeded', 'canceled', 'unknown']) assert.ok(rowText({ ...child, status }, now + 31000).includes(status));
  assert.match(rowText({ ...child, linked: false, availability: 'unavailable' }, now), /Unlinked child.*source unavailable/);
});
