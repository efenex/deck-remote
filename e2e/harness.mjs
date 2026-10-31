// Shared start-up helpers for the e2e files (make e2e).
import { spawn } from 'node:child_process';
import net from 'node:net';
import { once } from 'node:events';
import { chromium } from 'playwright-core';

// One budget for every per-wait timeout that is not a deliberate UI assertion: server start-up,
// Chrome launch, and Playwright's default action/navigation/wait timeout (also 30 s upstream).
// On a slow machine, set E2E_TIMEOUT_MS instead of editing literals.
export const E2E_TIMEOUT_MS = Number(process.env.E2E_TIMEOUT_MS) || 30_000;

export async function freePort() {
  const s = net.createServer(); s.listen(0, '127.0.0.1'); await once(s, 'listening');
  const port = s.address().port; await new Promise((r) => s.close(r)); return port;
}

// Poll until url answers 2xx; throw after E2E_TIMEOUT_MS. Always drain the body: an unread body
// on a connection the server then closes (mock-api.py is HTTP/1.0) trips an uncaught undici
// assertion (`assert(!this.paused)`), which fails the test while its body keeps running and
// leaves a Chrome that keeps the file's process alive.
// ready(body) optionally checks the body text, so a caller can wait on the API the page needs.
export async function waitHttp(url, ready = () => true) {
  const deadline = Date.now() + E2E_TIMEOUT_MS;
  let last = 'no response';
  while (Date.now() < deadline) {
    try {
      const r = await fetch(url); const body = await r.text();
      if (r.ok && ready(body)) return;
      last = r.ok ? 'not ready: ' + body.slice(0, 80) : 'HTTP ' + r.status;
    } catch (e) { last = e.message; }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`${url} did not come up within ${E2E_TIMEOUT_MS} ms (${last})`);
}

export function launchChrome(opts = {}) {
  return chromium.launch({ channel: 'chrome', headless: true, timeout: E2E_TIMEOUT_MS, ...opts });
}

// Apply the budget to every Playwright wait in a context that has no explicit timeout.
export function withDefaults(ctx) { ctx.setDefaultTimeout(E2E_TIMEOUT_MS); return ctx; }

// The UI fixture (scripts/ui/mock-api.py) on a free port, returned once its session API serves
// the deck the tests open (card s1), not just once a static file answers.
export async function startMockApi(t) {
  const port = await freePort();
  const server = spawn('python3', ['-I', '../scripts/ui/mock-api.py', '../web', String(port)], { stdio: 'ignore' });
  t.after(() => server.kill());
  const base = 'http://127.0.0.1:' + port;
  await waitHttp(base + '/api/rc/sessions', (body) => (JSON.parse(body).sessions || []).some((s) => s.id === 's1'));
  return base;
}

// First render after a page load: goto, then wait for `ready` (a selector, in `state`). Returns
// the response of the navigation that rendered it. The first wait gets a third
// of the budget; if it lapses, log what the page shows and which requests failed or never
// finished, reload once, and wait the rest of the budget. A loaded machine has been seen to leave
// the very first render without its cards (not reproduced elsewhere yet); the log says why. The
// app retries the session list itself, but not the tower probe (initTower), so a single slow
// /api/rc/tower hides the Tower tab until the next page load.
export async function openDeck(page, url, ready = '.card[data-id="s1"]', state = 'attached') {
  const pending = new Map(); const failed = [];
  const t0 = Date.now(); const since = () => Date.now() - t0 + 'ms';
  const on = {
    request: (r) => pending.set(r, since()),
    requestfinished: (r) => pending.delete(r),
    requestfailed: (r) => { pending.delete(r); failed.push(`${since()} ${r.url()} ${r.failure()?.errorText}`); },
  };
  for (const [ev, fn] of Object.entries(on)) page.on(ev, fn);
  try {
    const resp = await page.goto(url);
    const first = Math.round(E2E_TIMEOUT_MS / 3);
    try {
      await page.locator(ready).waitFor({ state, timeout: first });
      return resp;
    } catch {
      const text = (sel) => page.locator(sel).first().innerText({ timeout: 1000 }).catch(() => '?');
      console.error(`openDeck: ${ready} not rendered after ${since()}; reloading once.\n` +
        `  connection: ${await text('#connText')}\n  deck: ${(await text('#deck')).slice(0, 200).replace(/\s+/g, ' ')}\n` +
        `  failed: ${failed.join(' | ') || 'none'}\n` +
        `  pending: ${[...pending].map(([r, at]) => at + ' ' + r.url()).join(' | ') || 'none'}`);
    }
    const again = await page.reload();
    await page.locator(ready).waitFor({ state, timeout: E2E_TIMEOUT_MS - first });
    return again;
  } finally {
    for (const [ev, fn] of Object.entries(on)) page.off(ev, fn);
  }
}
