// Uses the existing UI fixture server; no real sessions or sends.
import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { freePort, waitHttp, launchChrome, withDefaults } from './harness.mjs';

test('phone: offline refresh feedback, transcript failure and resume layout recovery', { timeout: 60000 }, async t => {
  const port = await freePort();
  const server = spawn('python3', ['-I', '../scripts/ui/mock-api.py', '../web', String(port)], { stdio: 'ignore' });
  t.after(() => server.kill());
  const base = 'http://127.0.0.1:' + port;
  await waitHttp(base);
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
  await page.goto(base + '/?token=fake');
  await page.locator('.card[data-id="s1"]').waitFor();
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
