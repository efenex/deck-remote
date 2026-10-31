// End-to-end: phone pairing by QR (pair.go, web/pair.js) in a real browser.
// Run: make e2e   (installs e2e/ deps if missing; needs Google Chrome, qrencode and ffmpeg)
//
// Boots a freshly built deck-remote on a free loopback port with a temp HOME and state dir. The
// laptop asks for a pairing offer (loopback, no serve headers); its URL becomes a QR still that
// Chrome's fake camera streams. The page starts with no token, scans with the real scanner (jsQR,
// under the CSP), redeems, enrols Face ID through the pairing grant (Chrome's virtual
// authenticator) and opens the app. It never touches the real state dir or port.
import test from 'node:test';
import assert from 'node:assert';
import { spawn, execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { freePort, waitHttp, launchChrome, withDefaults } from './harness.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.dirname(here);
const OPERATOR = 'op@example.com';
const SHARED = 'shared-e2e-token';

test('scan a pairing QR inside the app: token, Face ID without a code, app opens', { timeout: 120e3 }, async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'deck-pair-e2e-'));
  const home = path.join(dir, 'home');
  const state = path.join(home, '.agent-deck');
  fs.mkdirSync(state, { recursive: true, mode: 0o700 });
  fs.writeFileSync(path.join(dir, 'overview.json'), JSON.stringify({ items: [] }));
  const bin = path.join(dir, 'deck-remote');
  execFileSync('go', ['build', '-o', bin, '.'], { cwd: repo, stdio: 'inherit' });
  const port = await freePort();
  const origin = 'http://localhost:' + port;
  const base = 'http://127.0.0.1:' + port;
  const srv = spawn(bin, ['--listen', '127.0.0.1:' + port, '--agentdeck-url', 'http://127.0.0.1:1', '--bin', '/nonexistent/agent-deck',
    '--web', path.join(repo, 'web'), '--token', SHARED, '--tower-overview', path.join(dir, 'overview.json'),
    '--tower-operator', OPERATOR, '--public-url', origin],
  { env: { PATH: process.env.PATH, HOME: home, DECK_REMOTE_STATE_DIR: state }, stdio: ['ignore', 'ignore', 'pipe'] });
  let srvErr = '';
  srv.stderr.on('data', (d) => { srvErr += d; });
  t.after(() => { srv.kill(); fs.rmSync(dir, { recursive: true, force: true }); });
  await waitHttp(base + '/healthz');

  // The laptop's half: scripts/pair-phone.sh does this request and prints the URL as a QR.
  const offer = await (await fetch(base + '/api/rc/devices/pair', { method: 'POST', headers: { Authorization: 'Bearer ' + SHARED, 'Content-Type': 'application/json' }, body: JSON.stringify({ name: 'e2e-phone', caps: ['act', 'run:ro', 'run:mut'] }) })).json();
  assert.ok(offer.url && offer.url.startsWith(origin + '/#pair='), 'offer: ' + JSON.stringify(offer));
  const png = path.join(dir, 'qr.png');
  const y4m = path.join(dir, 'qr.y4m');
  execFileSync('qrencode', ['-o', png, '-s', '8', '-m', '4', offer.url]);
  execFileSync('ffmpeg', ['-loglevel', 'error', '-loop', '1', '-i', png, '-t', '2', '-r', '10', '-vf', 'scale=480:480,format=yuv420p', y4m]);

  const browser = await launchChrome({ args: [
    '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream', '--use-file-for-fake-video-capture=' + y4m] });
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ extraHTTPHeaders: { 'Tailscale-User-Login': OPERATOR }, serviceWorkers: 'block' }));
  await ctx.grantPermissions(['camera'], { origin });
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (/Content Security Policy|Refused to/i.test(m.text())) problems.push('console: ' + m.text()); });
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message));
  const cdp = await ctx.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } });

  await page.goto(origin + '/');
  await page.locator('#pairScan').waitFor({ state: 'visible', timeout: 10e3 });
  await page.click('#pairScan');
  await page.locator('#pairDone').waitFor({ state: 'visible', timeout: 20e3 });
  assert.match(await page.textContent('#pairMsg'), /Paired as e2e-phone/);
  const tok = await page.evaluate(() => localStorage.getItem('deck_remote_token'));
  assert.ok(tok && tok.length > 30, 'token stored');

  // The code was spent: a second redeem is refused.
  const again = await fetch(base + '/api/rc/devices/pair/redeem', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ code: offer.code }) });
  assert.strictEqual(again.status, 403);

  await page.click('#pairFaceID');
  await page.waitForFunction(() => /Passkey [\w-]+ enrolled|not set up/.test(document.querySelector('#pairMsg').textContent), null, { timeout: 15e3 });
  assert.match(await page.textContent('#pairMsg'), /Passkey [\w-]+ enrolled/);
  const keys = await (await fetch(base + '/api/rc/passkey/list', { headers: { Authorization: 'Bearer ' + tok, 'Tailscale-User-Login': OPERATOR } })).json();
  assert.strictEqual((keys.passkeys || []).length, 1, 'passkeys: ' + JSON.stringify(keys));

  await page.click('#pairContinue');
  await page.locator('#tabTower').waitFor({ state: 'visible', timeout: 15e3 });
  const devices = await (await fetch(base + '/api/rc/devices', { headers: { Authorization: 'Bearer ' + SHARED } })).json();
  assert.deepStrictEqual(devices.devices.map((d) => d.name), ['e2e-phone']);
  assert.deepStrictEqual(problems, [], 'browser problems:\n' + problems.join('\n') + '\nserver stderr:\n' + srvErr.slice(-2000));
});
