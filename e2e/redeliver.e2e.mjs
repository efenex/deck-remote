import test from 'node:test';
import assert from 'node:assert/strict';
import { startMockApi, openApp, launchBrowser, withDefaults } from './harness.mjs';

// Real app.js rendering and click path, but ONLY a disposable mock server and
// fake SSE/requests. Never open an act-token instance or the real phone journal.
test('delivery diagnostics and same-ID re-delivery preserve bubble and composer', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchBrowser(); t.after(() => browser.close());
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
  await page.route('**/api/rc/ask/status?*', r => { checks.push(r.request().url()); return r.fulfill({ json: status }); });
  await page.route('**/api/rc/ask/redeliver*', r => {
    const body = r.request().postDataJSON();
    retries.push(body);
    return r.fulfill({ json: { ...event, state: 'refused', operationId: body.operationId, redeliveryOutcome: 'refused', reason: 'composer occupied; nothing typed', redeliveryReason: 'Re-delivery: refused' } });
  });
  await openApp(page, base + '/?token=fake');
  await page.click('.card[data-id="s1"]');
  await page.waitForFunction(() => !document.getElementById('composerInput').disabled);
  await page.fill('#composerInput', '[REDACTED]');
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
  assert.deepEqual(retries, [{ sessionId: 's1', requestId: 'r1', sendId: 'send1', profile: asks[0].profile, operationId: retries[0].operationId }]);
  assert.match(retries[0].operationId, /^[a-f0-9-]{36}$/);
  assert.equal(await page.locator('#composerInput').inputValue(), 'keep this draft');
  assert.equal(await page.locator('#convo .msg.me').count(), 1, 'original bubble retained');
  // The final state changed while the phone slept, with no SSE received.
  status = { ...event, state: 'lost', statusTs: ts + 120, reason: 'no landed trace after 2 minutes — delivery is unconfirmed' };
  await page.evaluate(() => window.dispatchEvent(new Event('pageshow')));
  await page.getByRole('button', { name: 'Check again', exact: true }).waitFor();
  assert.equal(checks.length, 1);
  assert.equal(retries.length, 1, 'resume only checks status; it never releases');
  assert.equal(asks.length, 1);
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
  // R0 outcomes must render over SSE too, without waiting for the POST.
  for (const [state, outcome, label] of [
    ['unsupported', 'unsupported', 'unverifiable'],
    ['blocked_unknown', 'blocked_unknown', 'may have landed; not re-sent · check again'],
    ['pending', 'pending', 'in progress'],
    ['refused', 'conflict', 're-delivery unavailable'],
  ]) {
    await page.evaluate(ev => window.deliveryEvents.onmessage({ data: JSON.stringify(ev) }), {
      ...event, state, redeliveryOutcome: outcome, operationId: 'sse-fixture', statusTs: ts + 121,
    });
    assert.ok((await page.locator('#convo').innerText()).includes(label));
  }
  // A late transcript receipt may predate the timeout. Evidence still wins.
  await page.evaluate(ev => window.deliveryEvents.onmessage({ data: JSON.stringify(ev) }), { ...event, state: 'delivered', statusTs: ts + 2, landedRowId: 'row1', reason: 'Seen in the target transcript' });
  assert.equal(await page.locator('#convo .retry').count(), 0);
  assert.match(await page.locator('#convo').innerText(), /landed/);
  assert.doesNotMatch(await page.locator('#convo').innerText(), /Re-delivery: refused/);
  assert.deepEqual(errors, []);
});

test('reload recovery, corrupt storage, and explicit new checks preserve the original send', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchBrowser(); t.after(() => browser.close());
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, serviceWorkers: 'block' });
  withDefaults(page.context());
  await page.addInitScript(() => {
    window.EventSource = class {
      constructor() { window.deliveryEvents = this; setTimeout(() => this.onopen?.({}), 0); }
      close() {}
    };
  });
  let messages = [];
  await page.route('**/api/rc/history?*', r => r.fulfill({ json: { messages, hasMore: false } }));
  await page.route('**/api/rc/activity?*', r => r.fulfill({ json: { working: false } }));
  await page.route('**/api/rc/attention?*', r => r.fulfill({ json: { pending: false } }));
  let asks = 0;
  await page.route('**/api/rc/ask*', r => {
    asks++;
    return r.fulfill({ status: 202, json: { requestId: 'r1', sessionId: 's1', status: 'sent' } });
  });
  const event = { type: 'ask-state', sessionId: 's1', requestId: 'r1', sendId: 'send1', state: 'failed', statusTs: Math.floor(Date.now()/1000), reason: 'target not running' };
  await page.route('**/api/rc/ask/status?*', r => r.fulfill({ json: event }));
  const taps = [];
  await page.route('**/api/rc/ask/redeliver*', r => {
    const body = r.request().postDataJSON(); taps.push(body);
    if (taps.length === 1 || taps.length === 3) return r.abort('failed'); // owner may have committed; response lost
    if (taps.length === 5 || taps.length === 6) return r.fulfill({ status: 502, json: { error: 're-delivery returned no landed snapshot; retry this check' } });
    return r.fulfill({ json: { ...event, operationId: body.operationId, redeliveryOutcome: 'refused', state: 'refused', redeliveryReason: 'refused: target not running' } });
  });
  await openApp(page, base + '/?token=fake');
  await page.click('.card[data-id="s1"]');
  await page.fill('#composerInput', '[REDACTED]');
  await Promise.all([page.waitForResponse(r => new URL(r.url()).pathname === '/api/rc/ask'), page.click('.send-btn')]);
  await page.waitForFunction(() => document.getElementById('composerInput').value === '');
  await page.evaluate(ev => window.deliveryEvents.onmessage({ data: JSON.stringify(ev) }), event);
  await page.getByRole('button', { name: 'Re-deliver', exact: true }).click();
  await page.getByText(/Re-delivery not confirmed/).waitFor();
  const saved = await page.evaluate(() => Object.keys(localStorage).filter(k => k.startsWith('deck.redelivery.')).map(k => JSON.parse(localStorage[k])));
  assert.equal(saved.length, 1);
  assert.equal(saved[0].operationId, taps[0].operationId);
  assert.equal(JSON.stringify(saved).includes('[REDACTED]'), false, 'persist only receipt IDs');
  // One corrupt slot must not suppress another send's valid recovery receipt.
  await page.evaluate(() => localStorage.setItem('deck.redelivery.corrupt', '{'));
  messages = [{ role: 'user', id: 'original-row', content: '[REDACTED]', ts: event.statusTs }];
  await openApp(page, base + '/?token=fake');
  await page.click('.card[data-id="s1"]');
  await page.locator('#convo .msg.me').waitFor();
  assert.equal(await page.locator('#convo .msg.me').count(), 1, 'history is the only message bubble after reload');
  assert.equal(await page.locator('#convo .delivery-check').count(), 1, 'receipt remains actionable as a notice');
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth));
  await page.evaluate(() => localStorage.removeItem('deck.redelivery.corrupt'));
  await page.getByRole('button', { name: 'Retry check', exact: true }).click();
  await page.getByText('refused: target not running', { exact: false }).waitFor();
  assert.equal(taps[1].operationId, taps[0].operationId, 'reload retries the SAME operation');
  await page.getByRole('button', { name: 'Re-deliver', exact: true }).click();
  await page.getByText(/Re-delivery not confirmed/).waitFor();
  assert.equal(taps.length, 3);
  assert.notEqual(taps[2].operationId, taps[1].operationId, 'new deliberate tap can retry a sticky refusal');
  // A matching SSE receipt also acknowledges a tap whose HTTP response was lost.
  await page.evaluate(ev => window.deliveryEvents.onmessage({ data: JSON.stringify(ev) }), {
    ...event, state: 'refused', operationId: taps[2].operationId, profile: taps[2].profile,
    redeliveryOutcome: 'refused', redeliveryReason: 'refused: target not running',
  });
  await page.waitForFunction(() => !Object.keys(localStorage).some(k => k.startsWith('deck.redelivery.')));
  await page.getByRole('button', { name: 'Re-deliver', exact: true }).click();
  await page.waitForFunction(() => !Object.keys(localStorage).some(k => k.startsWith('deck.redelivery.')));
  assert.equal(taps.length, 4);
  assert.notEqual(taps[3].operationId, taps[2].operationId);
  await page.getByRole('button', { name: 'Re-deliver', exact: true }).click();
  await page.getByText(/no landed snapshot/).waitFor();
  await page.getByRole('button', { name: 'Retry check', exact: true }).click();
  await page.getByRole('button', { name: 'Start a new check', exact: true }).waitFor();
  assert.equal(taps[5].operationId, taps[4].operationId, 'ordinary retry preserves a stuck key');
  await page.getByRole('button', { name: 'Start a new check', exact: true }).click();
  await page.getByRole('button', { name: 'Re-deliver', exact: true }).waitFor();
  assert.notEqual(taps[6].operationId, taps[5].operationId, 'explicit new check escapes a deterministic replay error');
  assert.ok(taps.every(tap => tap.sendId === 'send1' && tap.requestId === 'r1'));
  // A corrupt slot for this send is replaced durably on the next deliberate tap.
  await page.evaluate(profile => localStorage.setItem('deck.redelivery.' + JSON.stringify([profile, 's1', 'send1']), '{'), taps[0].profile);
  await page.getByRole('button', { name: 'Re-deliver', exact: true }).click();
  await page.waitForFunction(() => !Object.keys(localStorage).some(k => k.startsWith('deck.redelivery.')));
  assert.equal(taps.length, 8);
  assert.equal(asks, 1, 'no new text send');
});
