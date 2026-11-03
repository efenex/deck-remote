// Uses the existing UI fixture server; no real sessions or sends.
import test from 'node:test';
import assert from 'node:assert/strict';
import { startMockApi, openApp, launchChrome, withDefaults } from './harness.mjs';

test('phone: offline refresh feedback, transcript failure and resume layout recovery', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchChrome();
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' }));
  const page = await ctx.newPage(); const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.route('**/api/rc/tower**', route => {
    const path = new URL(route.request().url()).pathname;
    const data = path.endsWith('/model') ? { model: { header: 'TOWER 15:02Z', generated_at: '2026-10-25T15:02:00Z', sections: [] } } :
      path.endsWith('/whoami') ? {} : { enabled: true, overview: { generated_at: '2026-10-25T15:02:00Z', counts: {} } };
    return route.fulfill({ json: data });
  });
  await openApp(page, base + '/?token=fake');
  // A black-holed response must have a finite spinner, including on the session shield.
  await page.route('**/api/rc/**', route => new Promise(resolve => {
    setTimeout(() => route.abort().catch(() => {}).finally(resolve), 13500);
  }));
  await page.click('#goRefresh');
  assert.equal(await page.locator('#goRefresh').isDisabled(), true);
  await page.waitForFunction(() => !document.querySelector('#goRefresh').disabled, { }, { timeout: 15000 });
  assert.match(await page.locator('.toast').last().innerText(), /Deck refresh failed.*Host did not respond/s);
  await page.unroute('**/api/rc/**');
  await page.route('**/api/rc/**', route => route.abort('internetdisconnected'));
  for (const [tab, id, label] of [['inbox', 'inboxRefresh', 'Inbox'], ['tower', 'towerRefresh', 'Tower']]) {
    await page.click('.tab[data-tab="' + tab + '"]'); await page.click('#' + id);
    await page.waitForFunction(id => !document.getElementById(id).disabled, id);
    assert.match(await page.locator('.toast').last().innerText(), new RegExp(label + ' refresh failed'));
  }
  assert.match(await page.locator('#towerAsOf').innerText(), /2026-10-25T15:02:00Z.*STALE/);
  await page.click('.tab[data-tab="deck"]'); await page.click('.card[data-id="s1"]');
  await page.waitForFunction(() => document.querySelector('#convo').textContent.includes('Transcript unavailable'));
  assert.match(await page.locator('#sheetConnection').innerText(), /Host unreachable.*last contact/s);
  await page.click('#sheetCheckPerm');
  await page.waitForFunction(() => !document.querySelector('#sheetCheckPerm').disabled);
  assert.match(await page.locator('.toast').last().innerText(), /Attention refresh failed/);
  await page.evaluate(() => {
    document.activeElement.blur(); document.documentElement.classList.add('kb');
    const sheet = document.querySelector('#sheet'); sheet.style.height = '400px'; sheet.style.bottom = 'auto';
    window.dispatchEvent(new Event('pageshow'));
  });
  assert.equal(await page.evaluate(() => document.documentElement.classList.contains('kb')), false);
  assert.equal(await page.locator('#sheet').evaluate(el => el.style.height), '');
  assert.deepEqual(errors, []);
});

test('phone: Tower BACKLOG section, defer picker and the act payload', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchChrome();
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' }));
  const page = await ctx.newPage(); const errors = []; const posts = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('dialog', d => d.accept());
  const gen = new Date().toISOString();
  const backlog = [
    { id: 'T-42.fix_Thing', title: 'Fix the thing', status: 'backlog', age_days: 12, goal: 'g-home', deps_open: ['T-7'], not_before: null },
    { id: 'T-43', title: 'Later', status: 'backlog', age_days: 2, goal: 'g-x', deps_open: [], not_before: '2026-11-20T08:00:00Z' },
  ];
  await page.route('**/api/rc/tower**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/act')) { posts.push(route.request().postDataJSON()); return route.fulfill({ json: { ok: true, msg: 'queued → tower' } }); }
    const data = path.endsWith('/model') ? { enabled: true, actions: true, model: { header: 'TOWER', generated_at: gen, sections: [], palette: {}, legend: [], backlog, counts: { backlog: 2 } } } :
      path.endsWith('/whoami') ? { actions_enabled: true, act: true } : { enabled: true, overview: { generated_at: gen, counts: {} } };
    return route.fulfill({ json: data });
  });
  await openApp(page, base + '/?token=fake');
  await page.click('.tab[data-tab="tower"]');
  const head = page.locator('.tsechead.tbacklog');
  await head.waitFor();
  const txt = async l => (await l.textContent()).replace(/\s+/g, ' ');
  assert.match(await txt(head), /▸ BACKLOG \(2\)/);
  assert.equal(await page.locator('.tbrow').count(), 0, 'collapsed by default');
  await head.click();
  assert.equal(await page.locator('.tbrow').count(), 2);
  assert.match(await txt(page.locator('.tbrow').first()), /12 d T-42\.fix_Thing · g-home.*Fix the thing.*waits on T-7/);
  await page.locator('.tbrow').first().getByRole('button', { name: 'Defer…' }).click();
  const date = page.locator('.tbrow input.tdate');
  const tomorrow = await page.evaluate(() => window.DeckBacklog.tomorrow());
  assert.equal(await date.inputValue(), tomorrow);
  await date.fill('2026-12-01');
  await page.locator('.tbrow').first().getByRole('button', { name: 'Defer', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('.tbrow').textContent.includes('queued with tower'));
  assert.deepEqual(posts, [{ op: 'backlog_defer', id: 'T-42.fix_Thing', not_before: '2026-12-01T08:00:00Z' }]);
  const box = await page.locator('.tbrow').first().boundingBox();
  assert.ok(box.x >= 0 && box.x + box.width <= 390, 'row fits the phone width');
  await page.screenshot({ path: process.env.E2E_SHOT || '/dev/null' }).catch(() => {});
  assert.deepEqual(errors, []);
});
