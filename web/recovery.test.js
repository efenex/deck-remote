const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { listFeedback, listResponse, listIsStale, updateWorker } = require('./recovery.js');

test('list age and failure are independent of healthy generic contact', () => {
  const now = Date.now();
  const state = { at: now - 7 * 86400000, cached: true, error: 'HTTP 503' };
  assert.match(listFeedback(state, now), /List from 7 d ago · cached · refresh failed: HTTP 503/);
  assert.match(listFeedback({ at: 0, loading: true }), /age unknown.*refreshing/);
  assert.match(listFeedback({ at: now - 120000 }, now), /2 min ago/);
});

test('server-cache age survives HTTP 200, including a failed expired-cache rebuild', () => {
  const now = 1000000;
  for (const kind of ['stale', 'miss']) {
    const value = listResponse(new Headers({ 'X-Sessions-Cache': kind, 'X-Sessions-Age-Ms': '600000' }), now);
    assert.equal(value.at, 400000);
    assert.equal(value.serverStale, true);
    assert.match(listFeedback(value, now), /10 min ago.*server refresh pending or failed/);
  }
  assert.equal(listResponse(new Headers({ 'X-Sessions-Cache': 'stale' }), now).at, 0);
  // Healthy stale-while-revalidate (slow build) answers with the previous poll's rebuild.
  const swr = listResponse(new Headers({ 'X-Sessions-Cache': 'stale', 'X-Sessions-Age-Ms': '20000' }), now);
  assert.equal(swr.cached, false); assert.equal(swr.serverStale, false); assert.equal(swr.at, now - 20000);
  assert.equal(listResponse(new Headers({ 'X-Sessions-Cache': 'stale', 'X-Sessions-Age-Ms': '60000' }), now).serverStale, true);
  const fresh = listResponse(new Headers({ 'X-Sessions-Cache': 'hit', 'X-Sessions-Age-Ms': '2000' }), now);
  assert.equal(fresh.at, now - 2000);
  assert.equal(fresh.cached, false);
  assert.equal(listResponse(undefined, now).at, now, 'older servers without cache headers still work');
});

function registration() {
  const worker = new EventTarget();
  worker.state = 'installing';
  const reg = { active: {}, installing: worker, update: async () => {} };
  return { reg, worker, activate() {
    reg.active = worker; reg.installing = null; worker.state = 'activated';
    worker.dispatchEvent(new Event('statechange'));
  } };
}

test('update waits for activation, not just update() completion', async () => {
  const fixture = registration();
  let done = false;
  const updating = updateWorker(fixture.reg).then(value => { done = true; return value; });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(done, false);
  fixture.activate();
  assert.equal(await updating, true);
  assert.equal(await updateWorker(fixture.reg), false);
  assert.equal(await updateWorker(null), false);
});

test('redundant or stalled worker reports failure rather than hanging Resync', async () => {
  const { reg, worker } = registration();
  const updating = updateWorker(reg);
  await new Promise(resolve => setImmediate(resolve));
  worker.state = 'redundant'; worker.dispatchEvent(new Event('statechange'));
  await assert.rejects(updating, /installation failed/);
  await assert.rejects(updateWorker({ update: () => new Promise(() => {}) }, 10), /timed out/);
  await assert.rejects(updateWorker(registration().reg, 10), /timed out/);
});

test('service worker never intercepts session-list API and caches the matching shell', () => {
  const handlers = {};
  vm.runInNewContext(fs.readFileSync(require.resolve('./sw.js'), 'utf8'), {
    self: { location: { origin: 'https://fixture.invalid' }, addEventListener: (name, fn) => { handlers[name] = fn; } }, URL,
  });
  for (const path of ['/api/rc/sessions', '/api/rc/sessions?all=1&profile=default', '/api/rc/events', '/ws/session/a']) {
    handlers.fetch({ request: { method: 'GET', url: 'https://fixture.invalid' + path }, respondWith: () => assert.fail('API intercepted') });
  }
  const html = fs.readFileSync(require.resolve('./index.html'), 'utf8');
  const sw = fs.readFileSync(require.resolve('./sw.js'), 'utf8');
  for (const [, script] of html.matchAll(/<script src="([^"]+)"/g)) assert.ok(sw.includes("'" + script + "'"), script + ' precached');
});

test('Live is not claimed over a list older than 90 s while visible', () => {
  const now = 1000000;
  assert.equal(listIsStale({ at: now - 30000 }, true, now), false);
  assert.equal(listIsStale({ at: now - 91000 }, true, now), true, 'frozen timers / hung poll');
  assert.equal(listIsStale({ at: now - 91000 }, false, now), false, 'hidden: polling is paused by design');
  assert.equal(listIsStale({ at: 0 }, true, now), true);
  assert.equal(listIsStale({ at: now, cached: true }, true, now), true);
  assert.equal(listIsStale({ at: now, error: 'x' }, true, now), true);
});
