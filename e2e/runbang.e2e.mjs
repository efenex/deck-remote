// End-to-end: run-bang phase 2 #7 (docs/design/run-bang-phase2-review.md §5 W3) in a real browser.
// Run: make e2e   (installs e2e/ deps if missing; needs Google Chrome; downloads no browser)
//
// Boots a freshly built deck-remote on a free loopback port with a temp HOME and state dir, a fake
// tower root (scripts/run_bang.py prints a fixture verify and records argv; nothing runs) and a fake
// tower_model.py. The page talks to it as `tailscale serve` would relay it (loopback peer plus the
// operator's Tailscale-User-Login), with Chrome's virtual WebAuthn authenticator standing in for
// Face ID. The enrol code is requested the laptop way (loopback, no serve headers) and read from
// its 0600 file. It never touches the real tower root, journal, state dir or port.
import test from 'node:test';
import assert from 'node:assert';
import { spawn, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { freePort, waitHttp, launchChrome, withDefaults, openDeck } from './harness.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const repo = path.dirname(here);
const OPERATOR = 'op@example.com';
const SHARED = 'shared-e2e-token';
const sha = (line) => createHash('sha256').update(line.slice(2), 'utf8').digest('hex');

const LINES = {
  mut: '! git pull --ff-only',
  laptop: '! bash /abs/scripts/deploy.sh',
  bidi: '! ls /tmp/a‮b',
  zw: '! git pu​ll',
};

function writeFixture(dir) {
  const root = path.join(dir, 'tower');
  fs.mkdirSync(path.join(root, 'scripts'), { recursive: true });
  const cwd = path.join(dir, 'cwd');
  fs.mkdirSync(cwd);
  const items = [
    { index: 1, sha256: sha(LINES.mut), class: 'mut', phone_ok: true, laptop_only: null, sig: 'ok', runnable: true, why: null, run: { id: 'ask1:1:' + sha(LINES.mut).slice(0, 8), state: 'done', rc: 1, attempt: 1, tail: ['error: cannot pull with rebase'] }, cwd, timeout: 900, exp: Math.floor(Date.now() / 1000) + 43200, kid: 'k-e2e' },
    { index: 2, sha256: sha(LINES.laptop), class: 'mut', phone_ok: false, laptop_only: 'opaque code', sig: 'ok', runnable: false, why: 'laptop-only', run: null, cwd, timeout: 900, exp: Math.floor(Date.now() / 1000) + 43200, kid: 'k-e2e' },
    { index: 3, sha256: sha(LINES.bidi), class: 'ro', phone_ok: true, sig: 'ok', runnable: true, why: null, run: null, cwd, timeout: 120, exp: Math.floor(Date.now() / 1000) + 86400, kid: 'k-e2e' },
    { index: 4, sha256: sha(LINES.zw), class: 'copy', phone_ok: false, sig: 'ok', runnable: false, why: 'class:copy', run: null, cwd, timeout: 120, kid: 'k-e2e' },
  ];
  const ago = (h) => new Date(Date.now() - h * 3600e3).toISOString().replace(/\.\d+Z$/, 'Z');
  fs.writeFileSync(path.join(root, 'verify.json'), JSON.stringify({ ask: 'ask1', status: 'open', ask_ts: ago(5), sig_ts: ago(2), items }));
  fs.writeFileSync(path.join(root, 'scripts', 'run_bang.py'), `import json, os, sys
root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
with open(os.path.join(root, "calls.jsonl"), "a") as f:
    f.write(json.dumps(sys.argv[1:]) + "\\n")
cmd = sys.argv[1]
if cmd == "verify":
    print(open(os.path.join(root, "verify.json")).read().strip())
elif cmd == "run":
    a = sys.argv
    print(json.dumps({"id": "%s:%s:%s" % (a[a.index("--ask") + 1], a[a.index("--item") + 1], a[a.index("--sha") + 1][:8]), "class": "mut", "log": "x", "pid": 1, "timeout": 900}))
elif cmd == "log":
    print("fake runner: nothing ran")
else:
    print(json.dumps({"error": "unexpected"}))
    sys.exit(1)
`);
  const rb = (k, it, extra) => Object.assign({ index: it.index, sha256: it.sha256, class: it.class, sig: 'ok', ok: it.runnable, phone_ok: it.phone_ok, why: it.why, run: it.run || null }, extra || {});
  const model = {
    generated_at: new Date().toISOString(), header: 'e2e fixture', legend: [],
    sections: [{ id: 'waiting', title: 'Waiting on you', count: 1, items: [{
      kind: 'ask', id: 'ask1', key: 'ask:ask1', glyph: '?', meta: 'e2e',
      blocks: [
        { k: 'p', t: 'Pull the branch, then deploy.' },
        { k: 'cmd', t: LINES.mut, rb: rb('mut', items[0]) },
        { k: 'cmd', t: LINES.laptop, rb: rb('laptop', items[1]) },
        { k: 'cmd', t: LINES.bidi, rb: rb('bidi', items[2]) },
        { k: 'cmd', t: LINES.zw, rb: rb('zw', items[3]) },
      ],
    }] }],
  };
  fs.writeFileSync(path.join(dir, 'model.json'), JSON.stringify(model));
  fs.writeFileSync(path.join(dir, 'tower_model.py'), `import sys\nprint(open(${JSON.stringify(path.join(dir, 'model.json'))}).read())\n`);
  fs.writeFileSync(path.join(dir, 'overview.json'), JSON.stringify({ generated_at: new Date().toISOString(), counts: { waiting: 1 }, waiting: [{ id: 'ask1', text: 'e2e', items: Object.values(LINES) }], requests: [], in_flight: [], finished: [], deadlines: [] }));
  return { root, cwd };
}

test('run-bang phase 2 in Chrome: CSP, passkey enrolment, confirm sheet, Face ID run', { timeout: 180e3 }, async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'deck-remote-e2e-'));
  const home = path.join(dir, 'home');
  const state = path.join(home, '.agent-deck');
  fs.mkdirSync(state, { recursive: true, mode: 0o700 });
  const { root, cwd } = writeFixture(dir);
  const bin = path.join(dir, 'deck-remote');
  execFileSync('go', ['build', '-o', bin, '.'], { cwd: repo, stdio: 'inherit' });
  const port = await freePort();
  const origin = 'http://localhost:' + port;
  const base = 'http://127.0.0.1:' + port;
  const srv = spawn(bin, ['--listen', '127.0.0.1:' + port, '--agentdeck-url', 'http://127.0.0.1:1', '--bin', '/nonexistent/agent-deck',
    '--web', path.join(repo, 'web'), '--token', SHARED, '--tower-overview', path.join(dir, 'overview.json'), '--tower-root', root,
    '--tower-model-script', path.join(dir, 'tower_model.py'), '--tower-operator', OPERATOR, '--public-url', origin],
  { env: { PATH: process.env.PATH, HOME: home, DECK_REMOTE_STATE_DIR: state }, stdio: ['ignore', 'ignore', 'pipe'] });
  let srvErr = '';
  srv.stderr.on('data', (d) => { srvErr += d; });
  t.after(() => { srv.kill(); fs.rmSync(dir, { recursive: true, force: true }); });
  await waitHttp(base + '/healthz');

  // The laptop mints the phone's token (run:* caps are minted locally only, W1 #4).
  const mint = await (await fetch(base + '/api/rc/devices/mint', { method: 'POST', headers: { Authorization: 'Bearer ' + SHARED, 'Content-Type': 'application/json' }, body: JSON.stringify({ name: 'e2e-phone', caps: ['act', 'run:mut'] }) })).json();
  assert.ok(mint.token, 'mint: ' + JSON.stringify(mint));

  const browser = await launchChrome();
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ extraHTTPHeaders: { 'Tailscale-User-Login': OPERATOR }, serviceWorkers: 'block' }));
  await ctx.addInitScript((tok) => { localStorage.setItem('deck_remote_token', tok); }, mint.token);
  const page = await ctx.newPage();
  const problems = [];
  page.on('console', (m) => { if (/Content Security Policy|Refused to/i.test(m.text())) problems.push('console: ' + m.text()); });
  page.on('pageerror', (e) => problems.push('pageerror: ' + e.message));
  const cdp = await ctx.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', { options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true } });

  // CSP header present, and the app loads under it with no violation.
  const resp = await openDeck(page, origin + '/', '#tabTower', 'visible');
  const csp = resp.headers()['content-security-policy'] || '';
  assert.match(csp, /script-src 'self';/);
  assert.match(csp, /connect-src 'self'/);
  assert.match(csp, /frame-ancestors 'none'/);

  // Settings → Passkey: code from the laptop's file, Face ID (virtual), enrolled.
  await page.click('.tab[data-tab="settings"]');
  await page.locator('#passkeySection').waitFor({ state: 'visible', timeout: 10e3 });
  const issued = await (await fetch(base + '/api/rc/passkey/enrol-code', { method: 'POST', headers: { Authorization: 'Bearer ' + SHARED } })).json();
  assert.ok(issued.ok && !JSON.stringify(issued).match(/\b\d{6}\b/), 'enrol-code response: ' + JSON.stringify(issued));
  const codeFile = path.join(state, 'deck-remote-enrol-code');
  assert.strictEqual(fs.statSync(codeFile).mode & 0o777, 0o600);
  const code = fs.readFileSync(codeFile, 'utf8').trim();
  await page.fill('#passkeyCode', code);
  await page.click('#passkeyEnrol');
  await page.locator('#passkeyMsg', { hasText: 'enrolled' }).waitFor({ timeout: 15e3 });
  await page.locator('#passkeyList [data-passkey]').first().waitFor();
  assert.ok(!fs.existsSync(codeFile), 'the code file must be gone after use');

  // Tower tab rows: laptop-only has Copy and no Run; invisible characters render visibly.
  await page.click('#tabTower');
  const row = (line) => page.locator('.tcmdwrap').filter({ has: page.locator('pre', { hasText: line.slice(2, 12) }) });
  await row(LINES.mut).locator('button.trun').waitFor({ timeout: 15e3 });
  const lap = row(LINES.laptop);
  assert.strictEqual(await lap.locator('button.trun').count(), 0);
  assert.strictEqual(await lap.locator('button.tcopy', { hasText: 'Copy' }).count(), 1);
  assert.match(await lap.textContent(), /laptop only/);
  assert.strictEqual(await page.locator('.tcmdwrap pre').filter({ hasText: '⟨U+200B⟩' }).count(), 1);
  assert.strictEqual(await page.locator('.tcmdwrap pre .rbesc', { hasText: '⟨U+200B⟩' }).count(), 1);

  // Unfold reads the redacted log and never invokes run.
  await row(LINES.mut).getByRole('button', { name: 'Unfold log' }).click();
  await page.locator('[data-rb="full-log"]').filter({ hasText: 'fake runner: nothing ran' }).waitFor();
  await page.locator('.rbsheet').getByRole('button', { name: 'Refresh log' }).click();
  await page.waitForFunction(() => ![...document.querySelectorAll('.rbsheet button')].find(b => b.textContent === 'Refresh log').disabled);
  await page.locator('.rbsheet').getByRole('button', { name: 'Close', exact: true }).click();
  const beforeRun = fs.readFileSync(path.join(root, 'calls.jsonl'), 'utf8').trim().split('\n').map(l => JSON.parse(l));
  assert.ok(beforeRun.some(a => a[0] === 'log'));
  assert.ok(!beforeRun.some(a => a[0] === 'run'), 'unfold must not run commands');

  // A bidi character inside an ro line: the sheet shows it and offers Copy only.
  await row('! ls /tmp/a').locator('button.trun').click();
  const sheet = page.locator('.rbsheet');
  await sheet.locator('[data-rb="fact-class"]').waitFor();
  assert.match(await sheet.locator('[data-rb="line"]').textContent(), /⟨U\+202E⟩/);
  assert.ok(await sheet.locator('[data-rb="run"]').isHidden());
  await sheet.locator('button', { hasText: 'Cancel' }).click();

  // The mut line's sheet: every field, then Run with Face ID.
  await row(LINES.mut).locator('button.trun').click();
  await sheet.locator('[data-rb="fact-class"]').waitFor();
  const facts = {};
  for (const k of ['class', 'cwd', 'timeout', 'signature', 'proposed', 'expires', 'attempt']) facts[k] = await sheet.locator('[data-rb="fact-' + k + '"] .rbv').textContent();
  assert.match(facts.class, /mut .*phone list/);
  assert.strictEqual(facts.cwd, cwd);
  assert.strictEqual(facts.timeout, '900 s');
  assert.match(facts.signature, /✓ signed 2 h ago · key k-e2e/);
  assert.strictEqual(facts.proposed, '5 h ago');
  assert.match(facts.attempt, /^2 \(previous rc=1\)/);
  assert.match(await sheet.textContent(), /cannot pull with rebase/);
  assert.match(await sheet.locator('[data-rb="note"]').textContent(), /Runs as you on blackbird with your credentials/);
  await sheet.locator('[data-rb="run"]', { hasText: 'Face ID' }).click();
  await sheet.locator('[data-rb="note"]', { hasText: 'started ask1:1:' }).waitFor({ timeout: 15e3 });

  const calls = fs.readFileSync(path.join(root, 'calls.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l));
  const run = calls.filter((a) => a[0] === 'run');
  assert.strictEqual(run.length, 1, 'exactly one run');
  const flag = (f) => run[0][run[0].indexOf(f) + 1];
  assert.strictEqual(flag('--expect-cwd'), cwd);
  assert.strictEqual(flag('--expect-timeout'), '900');
  const auth = JSON.parse(flag('--auth'));
  assert.strictEqual(auth.method, 'passkey');
  assert.strictEqual(auth.challenge.length, 8);
  assert.ok(!run[0].join(' ').includes(mint.token), 'the token reached argv');

  assert.deepStrictEqual(problems, [], 'CSP violations or page errors');
  void srvErr;
});
