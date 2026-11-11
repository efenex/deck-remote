import test from 'node:test';
import assert from 'node:assert/strict';
import { startMockApi, openApp, launchChrome, withDefaults } from './harness.mjs';

// Real app.js rendering and click path, but ONLY a disposable mock server and
// fake SSE/requests. Never open an act-token instance or the real phone journal.
test('delivery diagnostics and same-ID re-delivery preserve bubble and composer', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchChrome(); t.after(() => browser.close());
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' });
  withDefaults(page.context());
  const errors = []; page.on('pageerror', e => errors.push(e.message));
  await page.addInitScript(() => {
    window.EventSource = class {
      constructor() { window.deliveryEvents = this; setTimeout(() => this.onopen?.({}), 0); }
      close() {}
    };
  });
  await page.route('**/api/rc/history?*', r => r.fulfill({ json: { messages: [], hasMore: false } }));
  await page.route('**/api/rc/activity?*', r => r.fulfill({ json: { working: false } }));
  await page.route('**/api/rc/attention?*', r => r.fulfill({ json: { pending: false } }));
  const asks = [], retries = [];
  await page.route('**/api/rc/ask*', r => {
    asks.push(r.request().postDataJSON());
    return r.fulfill({ status: 202, json: { requestId: 'r1', sessionId: 's1', status: 'sent' } });
  });
  const ts = Math.floor(Date.now() / 1000);
  const event = { type: 'ask-state', sessionId: 's1', requestId: 'r1', sendId: 'send1', state: 'queued', reason: 'target busy / mid-turn', targetStatus: 'running', statusTs: ts, attempts: 1 };
  let status = event;
  const checks = [];
  await page.route('**/api/rc/ask/redeliver*', r => {
    const body = r.request().postDataJSON();
    if (body.checkOnly) { checks.push(body); return r.fulfill({ json: status }); }
    retries.push(body);
    return r.fulfill({ json: { ...event, reason: 'composer occupied; nothing typed', redeliveryReason: 'Re-delivery: refused' } });
  });
  await openApp(page, base + '/?token=fake');
  await page.click('.card[data-id="s1"]');
  await page.waitForFunction(() => !document.getElementById('composerInput').disabled);
  await page.fill('#composerInput', 'delivery fixture');
  await Promise.all([
    page.waitForResponse(r => new URL(r.url()).pathname === '/api/rc/ask'),
    page.click('.send-btn'),
  ]);
  await page.waitForFunction(() => document.getElementById('composerInput').value === '');
  await page.evaluate(ev => window.deliveryEvents.onmessage({ data: JSON.stringify(ev) }), event);
  await page.getByRole('button', { name: 'Re-deliver', exact: true }).waitFor();
  assert.match(await page.locator('#convo').innerText(), /target busy \/ mid-turn.*target: running.*attempts: 1/);
  assert.equal(await page.locator('#convo .ts[title]').last().getAttribute('title'), new Date(ts * 1000).toISOString());
  await page.fill('#composerInput', 'keep this draft');
  await page.getByRole('button', { name: 'Re-deliver', exact: true }).click();
  await page.getByText(/Re-delivery: refused/).waitFor({ timeout: 5000 }).catch(async err => {
    console.error({ asks, retries, errors, convo: await page.locator('#convo').innerText() });
    throw err;
  });
  assert.equal(asks.length, 1, 'never enqueue a second message');
  assert.deepEqual(retries, [{ sessionId: 's1', requestId: 'r1', sendId: 'send1', profile: asks[0].profile }]);
  assert.equal(await page.locator('#composerInput').inputValue(), 'keep this draft');
  assert.equal(await page.locator('#convo .msg.me').count(), 1, 'original bubble retained');
  // The final state changed while the phone slept, with no SSE received.
  status = { ...event, state: 'lost', statusTs: ts + 120, reason: 'no landed trace after 2 minutes — delivery is unconfirmed' };
  await page.evaluate(() => window.dispatchEvent(new Event('pageshow')));
  await page.getByText(/lost \/ unconfirmed/).waitFor();
  assert.equal(checks.length, 1);
  assert.equal(retries.length, 1, 'resume only checks status; it never releases');
  assert.equal(asks.length, 1);
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
  // A late transcript receipt may predate the timeout. Evidence still wins.
  await page.evaluate(ev => window.deliveryEvents.onmessage({ data: JSON.stringify(ev) }), { ...event, state: 'delivered', statusTs: ts + 2, landedRowId: 'row1', reason: 'Seen in the target transcript' });
  assert.equal(await page.locator('#convo .retry').count(), 0);
  assert.match(await page.locator('#convo').innerText(), /landed/);
  assert.doesNotMatch(await page.locator('#convo').innerText(), /Re-delivery: refused/);
  assert.deepEqual(errors, []);
});
