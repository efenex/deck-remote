// Shared start-up helpers for the e2e files (make e2e).
import { spawn } from 'node:child_process';
import net from 'node:net';
import http from 'node:http';
import { once } from 'node:events';
import { chromium, webkit } from 'playwright-core';

// One budget for every per-wait timeout that is not a deliberate UI assertion: server start-up,
// Browser launch, and Playwright's default action/navigation/wait timeout (also 30 s upstream).
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

// Mock-fixture specs run on either engine. CDP virtual-authenticator specs keep launchChrome.
export function launchBrowser(opts = {}) {
  const project = process.env.E2E_BROWSER || 'chromium';
  if (project === 'chromium') return launchChrome(opts);
  if (project === 'webkit') return webkit.launch({ headless: true, timeout: E2E_TIMEOUT_MS, ...opts });
  throw new Error(`Unknown E2E_BROWSER: ${project}; use chromium or webkit`);
}

// Apply the budget to every Playwright wait in a context that has no explicit timeout.
export function withDefaults(ctx) { ctx.setDefaultTimeout(E2E_TIMEOUT_MS); return ctx; }

// The UI fixture (scripts/ui/mock-api.py) on a free port, returned once its session API serves
// the deck the tests open (card s1), not just once a static file answers.
// onAsk optionally receives { headers, body, url } at the actual HTTP boundary and returns
// { status, json }. WebKit's intercepted postDataBuffer omits multipart file bytes, so upload
// assertions must inspect what a server receives. Other requests proxy to the same UI fixture.
export async function startMockApi(t, { onAsk } = {}) {
  const port = await freePort();
  const server = spawn('python3', ['-I', '../scripts/ui/mock-api.py', '../web', String(port)], { stdio: 'ignore' });
  t.after(() => server.kill());
  const base = 'http://127.0.0.1:' + port;
  await waitHttp(base + '/api/rc/sessions', (body) => (JSON.parse(body).sessions || []).some((s) => s.id === 's1'));
  if (!onAsk) return base;
  const proxy = http.createServer(async (req, res) => {
    try {
      if (req.method === 'POST' && new URL(req.url, base).pathname === '/api/rc/ask') {
        const chunks = [];
        for await (const chunk of req) chunks.push(chunk);
        const reply = await onAsk({ headers: req.headers, body: Buffer.concat(chunks), url: 'http://' + req.headers.host + req.url });
        res.writeHead(reply.status, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify(reply.json));
        return;
      }
      const upstream = http.request(new URL(req.url, base), { method: req.method, headers: req.headers }, response => {
        res.writeHead(response.statusCode, response.headers);
        response.pipe(res);
      });
      upstream.on('error', err => res.destroy(err));
      res.on('close', () => upstream.destroy());
      req.pipe(upstream);
    } catch (err) { res.destroy(err); }
  });
  proxy.listen(0, '127.0.0.1'); await once(proxy, 'listening');
  t.after(() => { proxy.closeAllConnections(); return new Promise(resolve => proxy.close(resolve)); });
  return 'http://127.0.0.1:' + proxy.address().port;
}

// First render after a page load: goto, then wait for `ready` (a selector, in `state`). Returns
// the response of the navigation that rendered it. The first wait gets a third
// of the budget; if it lapses, log what the page shows and which requests failed or never
// finished, reload once, and wait the rest of the budget. A loaded machine has been seen to leave
// the very first render without its cards (not reproduced elsewhere yet); the log says why. The
// app retries the session list itself, but not the tower probe (initTower), so a single slow
// /api/rc/tower hides the Tower tab until the next page load.
export async function openApp(page, url, ready = '.card[data-id="s1"]', state = 'attached') {
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
      console.error(`openApp: ${ready} not rendered after ${since()}; reloading once.\n` +
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
