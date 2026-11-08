// Fixture-only: replies go to a route stub, never to the live Tower journal.
// G15: a typed Tower reply survives overview refreshes, and a closed ask keeps its draft visible.
import test from 'node:test';
import assert from 'node:assert/strict';
import { startMockApi, openApp, launchChrome, withDefaults } from './harness.mjs';

test('Tower reply drafts survive refreshes, ask closes and reloads', { timeout: 90000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchChrome(); t.after(() => browser.close());
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, serviceWorkers: 'block' });
  withDefaults(page.context());
  let asks = ['ask-a', 'ask-b'];
  const replies = [];
  await page.route('**/api/rc/tower**', async r => {
    const p = new URL(r.request().url()).pathname;
    if (p.endsWith('/reply')) { replies.push(r.request().postDataJSON()); return r.fulfill({ status: 202, json: { requestId: 'fixture' } }); }
    if (p.endsWith('/whoami')) return r.fulfill({ json: { actions_enabled: true, act: true } });
    if (p.endsWith('/model')) return r.fulfill({ json: { model: {
      header: 'Fixture tower', sections: [{ id: 'waiting', title: 'WAITING ON YOU', count: asks.length, items: asks.map(id =>
        ({ kind: 'ask', id, key: 'ask:' + id, status: 'waiting', blocks: [{ k: 'p', t: 'Question ' + id }] })) }],
    } } });
    return r.fulfill({ json: { enabled: true, overview: { counts: { waiting: asks.length } } } });
  });
  await openApp(page, base + '/?token=fake');
  await page.locator('#tabTower').click();
  const boxA = page.locator('.treply textarea[data-ask="ask-a"]');
  await boxA.waitFor();
  const refresh = () => page.evaluate(() => document.getElementById('towerRefresh').click());
  const settled = () => page.waitForTimeout(700);

  // 1. focused + non-empty: the refresh must not rebuild the list (same node, focus, caret kept)
  await boxA.click();
  await boxA.fill('half-typed reply');
  await boxA.evaluate(el => el.setSelectionRange(5, 9));
  const handle = await boxA.elementHandle();
  await refresh(); await settled();
  assert.equal(await handle.evaluate(el => el.isConnected), true, 'focused draft box was torn down');
  assert.equal(await boxA.inputValue(), 'half-typed reply');
  assert.equal(await boxA.evaluate(el => document.activeElement === el && el.selectionStart + ',' + el.selectionEnd), '5,9');

  // 2. blur: the deferred rebuild runs, the draft comes back in a fresh box
  await page.locator('.thint').first().click();
  await page.waitForFunction(el => !el.isConnected, handle);
  assert.equal(await boxA.inputValue(), 'half-typed reply');

  // 3. the ask closes while a draft exists: its box is gone, the draft shows with Copy
  asks = ['ask-b'];
  await refresh();
  const closed = page.locator('.tdraft');
  await closed.waitFor();
  assert.match(await closed.innerText(), /ask-a[\s\S]*half-typed reply/);
  assert.equal(await closed.locator('button', { hasText: 'Copy' }).count(), 1);

  // 4. a reload keeps it (localStorage), the box returns when the ask is back and sending clears it
  await page.reload();
  await page.locator('#tabTower').click();
  await page.locator('.tdraft').waitFor();
  asks = ['ask-a', 'ask-b'];
  await refresh();
  await boxA.waitFor();
  assert.equal(await boxA.inputValue(), 'half-typed reply');
  assert.equal(await page.locator('.tdraft').count(), 0);
  page.once('dialog', d => d.accept());
  await Promise.all([page.waitForResponse('**/api/rc/tower/reply*'), page.locator('.treply').first().locator('button').click()]);
  await page.waitForFunction(() => document.querySelector('.treply textarea').value === '');
  assert.deepEqual(replies, [{ ask: 'ask-a', text: 'half-typed reply' }]);
  await page.reload();
  await page.locator('#tabTower').click();
  await boxA.waitFor();
  assert.equal(await boxA.inputValue(), '', 'a sent draft must not come back');
});
