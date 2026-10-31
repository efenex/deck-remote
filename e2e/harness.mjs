// Shared start-up helpers for the e2e files (make e2e).
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
export async function waitHttp(url) {
  const deadline = Date.now() + E2E_TIMEOUT_MS;
  let last = 'no response';
  while (Date.now() < deadline) {
    try {
      const r = await fetch(url); await r.arrayBuffer();
      if (r.ok) return;
      last = 'HTTP ' + r.status;
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
