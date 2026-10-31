// Real hit testing and pointer activation against the UI fixture; no live sends.
import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import net from 'node:net';
import { chromium } from 'playwright-core';

test('composer: keyboard geometry survives focus transfer and sends once per tap', { timeout: 30000 }, async t => {
  const listener = net.createServer(); listener.listen(0, '127.0.0.1'); await once(listener, 'listening');
  const port = listener.address().port; await new Promise(r => listener.close(r));
  const server = spawn('python3', ['-I', '../scripts/ui/mock-api.py', '../web', String(port)], { stdio: 'ignore' });
  t.after(() => server.kill());
  const base = 'http://127.0.0.1:' + port;
  for (let i = 0; i < 50; i++) { try { if ((await fetch(base)).ok) break; } catch {} await new Promise(r => setTimeout(r, 100)); }
  const browser = await chromium.launch({ channel: 'chrome', headless: true }); t.after(() => browser.close());
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' });
  const errors = []; page.on('pageerror', e => errors.push(e.message));
  await page.addInitScript(() => {
    const vv = new EventTarget(); Object.assign(vv, { height: innerHeight, offsetTop: 0, scale: 1 });
    Object.defineProperty(window, 'visualViewport', { value: vv });
  });
  await page.route('**/api/rc/attention?*', r => r.fulfill({ json: { pending: false } }));
  const sends = [];
  await page.route('**/api/rc/ask*', async r => {
    sends.push(r.request().postDataJSON());
    await r.fulfill({ status: 202, json: { requestId: 'r' + sends.length, status: 'queued' } });
  });
  await page.goto(base + '/?token=fake'); await page.click('.card[data-id="s1"]');
  await page.locator('#sheet').evaluate(el => Promise.all(el.getAnimations().map(a => a.finished)));
  await page.fill('#composerInput', 'go offload lane');
  await page.evaluate(() => {
    const vv = window.visualViewport; vv.height = 449; vv.offsetTop = 50; vv.dispatchEvent(new Event('resize'));
  });
  const before = await page.locator('.send-btn').boundingBox();
  const x = before.x + before.width / 2, y = before.y + before.height / 2;
  assert.equal(await page.evaluate(({ x, y }) => document.elementFromPoint(x, y)?.className, { x, y }), 'send-btn');
  // Pointer down can transfer focus BEFORE pointer up / click. The keyboard
  // has not resized yet, so moving the sheet here loses the original tap.
  const firstResponse = page.waitForResponse('**/api/rc/ask*');
  await page.mouse.move(x, y); await page.mouse.down();
  const during = await page.locator('.send-btn').boundingBox();
  await page.mouse.up();
  assert.equal(during.y, before.y, 'send target moved between pointer down and up');
  await firstResponse;
  await page.waitForFunction(() => document.querySelector('#composerInput').value === '');
  assert.equal(sends.length, 1);
  assert.equal(await page.locator('#composerInput').evaluate(el => el === document.activeElement), true, 'send keeps typing focus');

  // A native touch tap uses the same click path, without a duplicate send.
  await page.fill('#composerInput', 'second');
  const secondResponse = page.waitForResponse('**/api/rc/ask*');
  await page.locator('.send-btn').tap();
  await secondResponse;
  await page.waitForFunction(() => document.querySelector('#composerInput').value === '');
  assert.equal(sends.length, 2);
  await page.fill('#composerInput', 'cancelled gesture');
  const target = await page.locator('.send-btn').boundingBox();
  await page.mouse.move(target.x + target.width / 2, target.y + target.height / 2);
  await page.mouse.down(); await page.mouse.move(200, 200); await page.mouse.up();
  assert.equal(sends.length, 2, 'pointerdown alone must never send');
  assert.equal(await page.locator('#composerInput').inputValue(), 'cancelled gesture');
  // Keyboard / accessibility activation still goes through the click handler.
  const thirdResponse = page.waitForResponse('**/api/rc/ask*');
  await page.locator('.send-btn').focus(); await page.keyboard.press('Enter');
  await thirdResponse;
  assert.equal(sends.length, 3);
  // Blur without a viewport resize must not reopen full-height layout over
  // the keyboard (e.g. the accessory bar's Done button / a photo control).
  await page.evaluate(() => document.activeElement.blur());
  await page.waitForTimeout(200);
  assert.equal(await page.locator('#sheet').evaluate(el => el.style.height), '449px');
  assert.equal(await page.locator('#app').evaluate(el => el.inert), true);
  await page.evaluate(() => {
    const vv = window.visualViewport; vv.offsetTop = 0; vv.height = innerHeight; vv.dispatchEvent(new Event('resize'));
  });
  assert.equal(await page.locator('#sheet').evaluate(el => el.style.height), '');
  await page.click('#sheetBack');
  assert.equal(await page.locator('#app').evaluate(el => el.inert), false);
  assert.deepEqual(errors, []);
});
