// Run: node --test web/
// A tapped push must land on the session that rang, also when the PWA already has another
// session's sheet open (the tower's), and the countdown helper must read right.
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// --- service worker: notificationclick -------------------------------------------------
function swEnv(clients) {
  const handlers = {};
  const opened = [];
  const self = {
    addEventListener: (t, fn) => { handlers[t] = fn; },
    registration: { showNotification: async () => {} },
    clients: { matchAll: async () => clients, claim: async () => {}, openWindow: async (u) => { opened.push(u); } },
    location: { origin: 'http://x' },
    skipWaiting() {},
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'sw.js'), 'utf8'), { self, caches: {}, URL, fetch() {}, setTimeout, clearTimeout, Promise });
  return { handlers, opened };
}
async function tap(env, data) {
  let p;
  env.handlers.notificationclick({ notification: { close() {}, data }, waitUntil: (x) => { p = x; } });
  await p;
}

test('notificationclick: an open window is told session, entry and kind, then focused', async () => {
  const log = [];
  const win = { postMessage: (m) => log.push(['msg', m]), focus: async () => { log.push(['focus']); } };
  const env = swEnv([win]);
  await tap(env, { id: 'n1', sessionId: 'luna-p678', kind: 'approval' });
  assert.deepEqual(JSON.parse(JSON.stringify(log[0])), ['msg', { type: 'open-notification', id: 'n1', sessionId: 'luna-p678', kind: 'approval' }]);
  assert.deepEqual(log[1], ['focus']);
  assert.deepEqual(env.opened, []);
});

test('notificationclick: a cold start URL carries the session and kind, not only the entry', async () => {
  const env = swEnv([]);
  await tap(env, { id: 'n 1', sessionId: 'luna-p678', kind: 'approval' });
  assert.deepEqual(env.opened, ['/?notif=n%201&session=luna-p678&kind=approval']);
  env.opened.length = 0;
  await tap(env, { sessionId: 's9' });
  assert.deepEqual(env.opened, ['/?session=s9']);
});

// --- app: where the message lands ------------------------------------------------------
const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const code = source.slice(source.indexOf('  async function openNotification('), source.indexOf("  $('#inboxRefresh')"));

function appEnv({ sheetOpenFor, sessions }) {
  const calls = [];
  const state = { sessions: new Map(sessions.map((id) => [id, { id }])), allSessions: new Map(), openSheetId: sheetOpenFor || null };
  let sheet = !!sheetOpenFor;
  const ctx = {
    state, inbox: { focusId: null }, NEEDS_KINDS: new Set(['approval', 'question', 'stall']),
    $: (sel) => (sel === '#sheet' && sheet ? {} : null),
    closeSheet: () => { calls.push('closeSheet'); sheet = false; state.openSheetId = null; },
    showTab: (t) => calls.push('showTab:' + t),
    openSheet: (id) => { if (sheet) ctx.closeSheet(); sheet = true; state.openSheetId = id; calls.push('openSheet:' + id); },
    loadSessions: async () => {}, api: async () => ({ sessions: [] }), toast: (t) => calls.push('toast:' + t),
  };
  vm.runInNewContext(code + '\nthis.openNotification = openNotification;', ctx);
  return { ctx, calls, state };
}

test('tap on an approval while the tower sheet is open lands on the approving session', async () => {
  const e = appEnv({ sheetOpenFor: 'tower', sessions: ['tower', 'luna-p678'] });
  await e.ctx.openNotification('n1', 'luna-p678', 'approval');
  assert.equal(e.state.openSheetId, 'luna-p678');
  assert.ok(e.calls.includes('openSheet:luna-p678'));
  assert.ok(e.calls.includes('showTab:deck'));
});

test('tap on a reply shows the inbox entry and closes the sheet that covered it', async () => {
  const e = appEnv({ sheetOpenFor: 'tower', sessions: ['tower', 'a'] });
  await e.ctx.openNotification('n2', 'a', 'reply');
  assert.deepEqual(e.calls, ['closeSheet', 'showTab:inbox']);
  assert.equal(e.ctx.inbox.focusId, 'n2');
});

test('tap on an approval for a session that is gone falls back to the inbox entry', async () => {
  const e = appEnv({ sheetOpenFor: null, sessions: ['tower'] });
  await e.ctx.openNotification('n3', 'gone', 'approval');
  assert.ok(e.calls.some((c) => c.startsWith('toast:')));
  assert.equal(e.calls[e.calls.length - 1], 'showTab:inbox');
  assert.equal(e.ctx.inbox.focusId, 'n3');
});

// --- approval card countdown -----------------------------------------------------------
test('approveDeadline counts down from the server deadline and ends expired', () => {
  const fn = source.slice(source.indexOf('  function approveDeadline('), source.indexOf('  // Render the Approve dock'));
  const ctx = {};
  vm.runInNewContext(fn + '\nthis.approveDeadline = approveDeadline;', ctx);
  const t0 = 1_000_000_000_000;
  assert.match(ctx.approveDeadline(t0 / 1000 + 16, t0).text, /in 0:16\./);
  assert.match(ctx.approveDeadline(t0 / 1000 + 16, t0 + 2000).text, /in 0:14\./);
  assert.match(ctx.approveDeadline(t0 / 1000 + 75, t0).text, /in 1:15\./);
  const gone = ctx.approveDeadline(t0 / 1000 + 16, t0 + 17000);
  assert.equal(gone.expired, true);
  assert.match(gone.text, /blocked/);
});
