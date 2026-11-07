// Fixture-only: never send replies to the live Tower journal.
import test from 'node:test';
import assert from 'node:assert/strict';
import { startMockApi, openApp, launchChrome, withDefaults } from './harness.mjs';

test('Tower reply leaves tab navigation reachable with stale keyboard metrics', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchChrome(); t.after(() => browser.close());
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' });
  withDefaults(page.context());
  await page.addInitScript(() => {
    const vv = new EventTarget(); Object.assign(vv, { height: innerHeight, offsetTop: 0, scale: 1 });
    Object.defineProperty(window, 'visualViewport', { value: vv });
  });
  let replyStatus = 202;
  const replies = [];
  await page.route('**/api/rc/tower**', async r => {
    const p = new URL(r.request().url()).pathname;
    if (p.endsWith('/reply')) {
      replies.push(r.request().postDataJSON());
      return r.fulfill({ status: replyStatus, json: replyStatus === 202 ? { requestId: 'fixture' } : { error: 'fixture refusal' } });
    }
    if (p.endsWith('/whoami')) return r.fulfill({ json: { actions_enabled: true, act: true } });
    if (p.endsWith('/model')) return r.fulfill({ json: { model: {
      header: 'Fixture tower', sections: [{ id: 'waiting', title: 'WAITING ON YOU', count: 1, items: [
        { kind: 'ask', id: 'fixture-ask', key: 'ask:fixture-ask', status: 'waiting', blocks: [{ k: 'p', t: 'Fixture question' }] },
      ] }],
    } } });
    return r.fulfill({ json: { enabled: true, overview: { counts: { waiting: 1 } } } });
  });
  await openApp(page, base + '/?token=fake');
  const input = page.locator('.treply textarea');
  // initTower loads a model in the background; entering the tab loads it again.
  // Wait for both renders before typing so a refresh cannot replace the draft.
  await input.waitFor({ state: 'attached' });
  const initialInput = await input.elementHandle();
  await page.locator('#tabTower').tap();
  await page.waitForFunction(el => !el.isConnected, initialInput);
  await input.waitFor();
  for (const outcome of ['cancel', 'success', 'failure']) {
    await input.fill('fixture answer');
    await page.evaluate(() => {
      const vv = window.visualViewport; vv.height = 449; vv.offsetTop = 50;
      vv.dispatchEvent(new Event('resize'));
    });
    replyStatus = outcome === 'failure' ? 403 : 202;
    await Promise.all([
      page.waitForEvent('dialog').then(d => outcome === 'cancel' ? d.dismiss() : d.accept()),
      outcome === 'cancel' ? Promise.resolve() : page.waitForResponse('**/api/rc/tower/reply*'),
      page.locator('.treply button').tap(),
    ]);
    await page.evaluate(() => document.activeElement.blur());
    // Simulate iOS omitting the final viewport expansion after keyboard dismissal.
    await page.waitForTimeout(700);
    assert.equal(await page.locator('html').evaluate(el => el.classList.contains('kb')), false);
    const nav = await page.locator('.tabbar').boundingBox();
    assert.ok(nav && nav.y >= 0 && nav.y + nav.height <= 844, 'tab bar must remain on screen');
  }
  await page.locator('[data-tab="settings"]').tap();
  assert.equal(await page.locator('#screenSettings').isVisible(), true);
  await page.locator('#tabTower').tap();
  assert.equal(await page.locator('#screenTower').isVisible(), true);
  assert.equal(replies.length, 2, 'cancel must not send');
});
