// Isolated loopback fixtures only: no installed browser profile or live service.
import test from 'node:test';
import assert from 'node:assert/strict';
import { startMockApi, launchChrome, withDefaults } from './harness.mjs';

const rows = Array.from({ length: 17 }, (_, i) => ({
  id: 'session-' + i, title: 'session-' + i, group: 'fixture', tool: 'claude',
  working: true, state: 'working', lastActive: Math.floor(Date.now() / 1000),
}));

async function fixture(t, { stallProfiles = false } = {}) {
  const base = await startMockApi(t);
  const browser = await launchChrome();
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, serviceWorkers: 'block' }));
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  t.after(() => assert.deepEqual(errors, []));
  await page.route('**/*', route => new URL(route.request().url()).origin === base ? route.continue() : route.abort());
  await page.addInitScript(() => {
    if (!localStorage.getItem('fixture-seeded')) {
    localStorage.setItem('fixture-seeded', 'true');
    localStorage.setItem('deck_remote_token', 'fixture-not-a-real-token');
    localStorage.setItem('deck_remote_profile', 'default');
    // Saved, non-default (and hidden) quiet-hour settings are not unsaved drafts.
    localStorage.setItem('deck_remote_prefs', JSON.stringify({ quietOn: false, quietStart: '21:00' }));
    localStorage.setItem('dr-tower-drafts', JSON.stringify({ 'ask-fixture': { text: 'unsent fixture reply', at: Date.now() } }));
    localStorage.setItem('dr-deck-cache', JSON.stringify({ at: Date.now() - 7 * 86400000, profile: 'default', sessions: [
      { id: 'v281', title: 'v281', group: 'fixture', working: true, state: 'working' },
    ] }));
    }
    window.streams = [];
    window.EventSource = class {
      constructor() {
        this.readyState = 1; window.streams.push(this);
        setTimeout(() => this.onopen?.(), 0);
      }
      close() { this.readyState = 2; }
    };
  });
  let status = 503, count = 0;
  await page.route('**/api/rc/sessions*', route => {
    count++;
    return route.fulfill({ status, json: status === 200 ? { sessions: rows, view: { hidden: 0 } } : { error: 'fixture list unavailable' } });
  });
  if (stallProfiles) await page.route('**/api/rc/profiles', () => {});
  await page.goto(base);
  await page.waitForFunction(() => document.querySelector('#connText').textContent.includes('refresh failed'));
  return { page, base, count: () => count, status: value => { status = value; } };
}

test('stale singleton stays visibly stale despite live SSE, then each resume signal refreshes and reconnects', { timeout: 60000 }, async t => {
  const f = await fixture(t), { page } = f;
  assert.match(await page.locator('#connText').innerText(), /Live.*List from 7 d ago.*refresh failed: fixture list unavailable/);
  assert.equal(await page.locator('.card').count(), 1);
  f.status(200);
  for (const event of ['visibilitychange', 'pageshow', 'focus', 'online']) {
    const before = f.count();
    const streams = await page.evaluate(() => window.streams.length);
    await page.evaluate(event => {
      // Clear burst coalescing just as a real background transition does.
      window.dispatchEvent(new Event('pagehide'));
      (event === 'visibilitychange' ? document : window).dispatchEvent(new Event(event));
    }, event);
    await page.waitForFunction(n => window.streams.length > n, streams);
    await page.waitForFunction(() => document.querySelectorAll('.card').length === 17);
    assert.ok(f.count() > before, event + ' fetches without waiting for the 20 s poll');
    assert.equal(await page.evaluate(() => window.streams.filter(s => s.readyState === 1).length), 1);
  }
  await page.waitForFunction(() => document.querySelector('#connStrip').hidden);
  assert.equal(await page.evaluate(() => JSON.parse(localStorage.getItem('dr-deck-cache')).sessions.length), 17);
});

test('Resync clears derived cache on failure, keeps pairing/drafts, and recovers without restart', { timeout: 60000 }, async t => {
  const f = await fixture(t), { page } = f;
  const drafts = await page.evaluate(() => localStorage.getItem('dr-tower-drafts'));
  await page.click('.tab[data-tab="settings"]');
  await page.click('#btnResync');
  await page.waitForFunction(() => !document.querySelector('#btnResync').disabled);
  assert.match(await page.locator('#resyncStatus').innerText(), /List from 7 d ago.*refresh failed/);
  assert.equal(await page.evaluate(() => localStorage.getItem('dr-deck-cache')), null);
  assert.equal(await page.evaluate(() => localStorage.getItem('deck_remote_token')), 'fixture-not-a-real-token');
  assert.equal(await page.evaluate(() => localStorage.getItem('dr-tower-drafts')), drafts);
  f.status(401);
  await page.click('#btnResync');
  await page.waitForFunction(() => !document.querySelector('#btnResync').disabled);
  assert.match(await page.locator('#resyncStatus').innerText(), /Unauthorized/);
  assert.equal(await page.evaluate(() => localStorage.getItem('deck_remote_token')), 'fixture-not-a-real-token');
  f.status(200);
  await page.click('#btnResync');
  await page.waitForFunction(() => !document.querySelector('#btnResync').disabled);
  await page.waitForFunction(() => JSON.parse(localStorage.getItem('dr-deck-cache') || '{}').sessions?.length === 17);
  assert.equal(await page.evaluate(() => localStorage.getItem('dr-tower-drafts')), drafts);
});

test('late pre-resume response cannot overwrite the recovered list', { timeout: 60000 }, async t => {
  const f = await fixture(t), { page } = f;
  const delayed = [];
  let received;
  const pending = new Promise(resolve => { received = resolve; });
  await page.route('**/api/rc/sessions*', route => { delayed.push(route); received(); });
  await page.click('#goRefresh');
  await pending;
  assert.equal(await page.locator('#goRefresh').isDisabled(), true);
  f.status(200);
  await page.unroute('**/api/rc/sessions*');
  await page.route('**/api/rc/sessions*', route => route.fulfill({ json: { sessions: rows } }));
  await page.evaluate(() => { window.dispatchEvent(new Event('pagehide')); window.dispatchEvent(new Event('online')); });
  await page.waitForFunction(() => document.querySelectorAll('.card').length === 17);
  for (const route of delayed) await route.fulfill({ json: { sessions: [{ id: 'v281', title: 'v281', working: true }] } });
  await page.waitForFunction(() => !document.querySelector('#goRefresh').disabled);
  assert.equal(await page.locator('.card').count(), 17);
});

test('resume is wired even while the boot profile fetch is stalled', { timeout: 60000 }, async t => {
  const f = await fixture(t, { stallProfiles: true }), { page } = f;
  f.status(200);
  await page.evaluate(() => { window.dispatchEvent(new Event('pagehide')); window.dispatchEvent(new Event('online')); });
  await page.waitForFunction(() => document.querySelectorAll('.card').length === 17);
});

test('Resync reloads for a new shell, preserving synthetic pairing and persisted drafts', { timeout: 60000 }, async t => {
  const f = await fixture(t), { page } = f;
  f.status(200);
  const drafts = await page.evaluate(() => localStorage.getItem('dr-tower-drafts'));
  await page.route('**/index.html', async route => {
    const response = await route.fetch();
    await route.fulfill({ response, body: (await response.text()).replace('/app.js?v=56', '/app.js?v=next-fixture') });
  });
  await page.click('.tab[data-tab="settings"]');
  await Promise.all([page.waitForNavigation(), page.click('#btnResync')]);
  await page.waitForFunction(() => document.querySelectorAll('.card').length === 17);
  assert.equal(await page.evaluate(() => localStorage.getItem('deck_remote_token')), 'fixture-not-a-real-token');
  assert.equal(await page.evaluate(() => localStorage.getItem('dr-tower-drafts')), drafts);
});

test('an activated worker update defers reload while an unsaved form is present', { timeout: 60000 }, async t => {
  const f = await fixture(t), { page } = f;
  f.status(200);
  await page.evaluate(() => {
    let checks = 0;
    const reg = { active: {}, update: async () => { if (++checks === 1) reg.active = {}; } };
    Object.defineProperty(navigator, 'serviceWorker', { value: { getRegistration: async () => reg } });
    const ta = document.createElement('textarea'); ta.id = 'fixture-draft'; ta.value = 'keep this unsent reply';
    document.body.appendChild(ta);
  });
  await page.click('.tab[data-tab="settings"]');
  await page.click('#btnResync');
  await page.waitForFunction(() => !document.querySelector('#btnResync').disabled);
  assert.match(await page.locator('#resyncStatus').innerText(), /Update ready.*draft/);
  assert.equal(await page.locator('#fixture-draft').inputValue(), 'keep this unsent reply');
  await page.locator('#fixture-draft').fill('');
  await Promise.all([page.waitForNavigation(), page.click('#btnResync')]);
});

test('HTTP 200 with stale server-cache headers keeps the real age visible and retries until rebuilt', { timeout: 60000 }, async t => {
  const f = await fixture(t), { page } = f;
  let rebuilt = false;
  await page.route('**/api/rc/sessions*', route => route.fulfill({
    headers: { 'X-Sessions-Cache': rebuilt ? 'refreshed' : 'miss', 'X-Sessions-Age-Ms': rebuilt ? '0' : '600000' },
    json: { sessions: rebuilt ? rows : [{ id: 'v281', title: 'v281', group: 'fixture', working: true, state: 'working' }] },
  }));
  await page.click('.tab[data-tab="settings"]');
  await page.click('#btnResync');
  await page.waitForFunction(() => !document.querySelector('#btnResync').disabled);
  assert.match(await page.locator('#resyncStatus').innerText(), /List from 10 min ago.*server refresh pending or failed/);
  assert.equal(await page.locator('#connStrip').evaluate(el => el.hidden), false);
  assert.ok(await page.evaluate(() => Date.now() - JSON.parse(localStorage.getItem('dr-deck-cache')).at >= 600000));
  rebuilt = true;
  // The 3 s retry (not a new manual action or 20 s poll) must fetch the rebuilt list.
  await page.waitForFunction(() => JSON.parse(localStorage.getItem('dr-deck-cache')).sessions.length === 17, null, { timeout: 8000 });
  assert.equal(await page.locator('#connStrip').evaluate(el => el.hidden), true);
});
