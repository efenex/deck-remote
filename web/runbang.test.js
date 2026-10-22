// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const R = require('./runbang.js');

const rb = (o) => Object.assign({ index: 0, sha256: 'a'.repeat(64), class: 'ro', sig: 'ok', ok: true, why: null, run: null }, o);

test('a signed runnable ro line offers Run', () => {
  const b = R.badge(rb({}), 'ask1');
  assert.strictEqual(b.offer, 'run');
  assert.strictEqual(b.mark, '✓');
  assert.strictEqual(b.tone, 'ok');
});

test('mut and secret are copy-only with "needs phase 2"; destructive/interactive/copy never run', () => {
  for (const c of ['mut', 'secret']) {
    const b = R.badge(rb({ class: c, ok: false, why: 'class ' + c + ' is copy-only in phase 1' }), 'a');
    assert.strictEqual(b.offer, 'phase2');
    assert.match(b.why, /needs phase 2/);
  }
  for (const c of ['destructive', 'interactive', 'copy']) assert.strictEqual(R.badge(rb({ class: c, ok: false }), 'a').offer, 'never');
});

test('sig-v1 and sig-stale-params block with the re-sign hint', () => {
  const v1 = R.badge(rb({ sig: 'sig-v1', ok: false, why: 'signature: sig-v1' }), 'ask1');
  assert.strictEqual(v1.offer, 'blocked');
  assert.strictEqual(v1.mark, '✗re-sign v2');
  assert.strictEqual(v1.hint, 're-sign as v2: run_bang.py resign --ask ask1');
  const st = R.badge(rb({ sig: 'sig-stale-params', ok: false, why: 'signature: sig-stale-params' }), 'ask1');
  assert.strictEqual(st.tone, 'urgent');
  assert.match(st.hint, /cwd or timeout changed after signing/);
  const un = R.badge(rb({ sig: 'unsigned', ok: false, why: 'signature: unsigned' }), 'a');
  assert.strictEqual(un.offer, 'blocked');
  assert.strictEqual(un.hint, '');
});

test('expiry is re-checked at draw time; already ran rc=0 blocks', () => {
  assert.strictEqual(R.badge(rb({ sig_exp: 100 }), 'a', 200).offer, 'blocked');
  assert.strictEqual(R.badge(rb({ ok: false, why: 'already ran, rc=0' }), 'a').why, 'already ran, rc=0');
  assert.strictEqual(R.badge(null), null);
});

test('runState glyphs', () => {
  assert.strictEqual(R.runState({ state: 'done', rc: 0 }).glyph, '✓');
  assert.strictEqual(R.runState({ state: 'done', rc: 2 }).text, 'rc=2');
  assert.strictEqual(R.runState({ state: 'running', attempt: 2 }).glyph, '⏳');
  assert.strictEqual(R.runState({ state: 'refused', reason: 'expired' }).text, 'refused: expired');
  assert.strictEqual(R.runState(null), null);
});

test('parseSSE keeps a partial frame for the next chunk', () => {
  const a = R.parseSSE('event: line\ndata: "x"\n\nevent: done\ndata: {"rc":0');
  assert.deepStrictEqual(a.events, [{ event: 'line', data: 'x' }]);
  const b = R.parseSSE(a.rest + '}\n\n');
  assert.deepStrictEqual(b.events, [{ event: 'done', data: { rc: 0 } }]);
  assert.strictEqual(b.rest, '');
});

// --- phase 2 (#7): mut behind Face ID, the confirm sheet's model, invisible characters, codecs ---

test('mut: Run behind a passkey when the feed offers it; laptop-only lines are copy', () => {
  const run = R.badge(rb({ class: 'mut', ok: true, phone_ok: true }), 'a');
  assert.strictEqual(run.offer, 'run');
  assert.strictEqual(run.passkey, true);
  const lap = R.badge(rb({ class: 'mut', ok: true, phone_ok: false }), 'a');
  assert.strictEqual(lap.offer, 'laptop');
  assert.match(lap.hint, /laptop only/);
  assert.strictEqual(R.badge(rb({ class: 'secret', ok: true }), 'a').offer, 'phase2');
  assert.strictEqual(R.badge(rb({}), 'a').passkey, false);
});

test('visibleParts shows zero-width, bidi and control characters, keeps tabs and plain text', () => {
  const p = R.visibleParts('git pu​ll‮ && x\t­y');
  assert.deepStrictEqual(p.map((x) => x.s), ['git pu', '⟨U+200B⟩', 'll', '⟨U+202E⟩', ' && x\t', '⟨U+00AD⟩', 'y']);
  assert.ok(R.hasInvisible('a⁦b'));
  assert.ok(R.hasInvisible('a\u0007b'));
  assert.ok(R.hasInvisible('a﻿b'));
  assert.ok(!R.hasInvisible('git pull --ff-only\torigin main'));
  assert.ok(!R.hasInvisible('écho ünïcode ✓ 日本'));
});

test('age reads ISO strings and unix seconds', () => {
  const now = Date.parse('2026-10-21T23:00:00Z');
  assert.strictEqual(R.age('2026-10-21T20:00:00Z', now), '3 h ago');
  assert.strictEqual(R.age(now / 1000 - 600, now), '10 min ago');
  assert.strictEqual(R.age(null, now), '');
});

const item = (o) => Object.assign({ index: 1, sha256: 'b'.repeat(64), class: 'mut', phone_ok: true, phone_cap: true, passkey: true, sig: 'ok', runnable: true, why: null, run: null, cwd: '/Users/op/src', timeout: 900, exp: 1900000000, kid: 'k1' }, o);
const top = { ask_ts: '2026-10-21T20:00:00Z', sig_ts: '2026-10-21T22:00:00Z' };

test('sheet: every review §2 field comes from verify JSON', () => {
  const m = R.sheet(item({}), top, Date.parse('2026-10-21T23:00:00Z'));
  const f = Object.fromEntries(m.facts);
  assert.strictEqual(m.state, 'run');
  assert.strictEqual(m.passkey, true);
  assert.match(f.class, /mut .*phone list/);
  assert.strictEqual(f.cwd, '/Users/op/src');
  assert.strictEqual(f.timeout, '900 s');
  assert.match(f.signature, /✓ signed 1 h ago · key k1/);
  assert.strictEqual(f.proposed, '3 h ago');
  assert.strictEqual(f.attempt, '1');
});

test('sheet: previous attempt, laptop-only, runner refusal, never', () => {
  const again = R.sheet(item({ run: { state: 'done', rc: 2, attempt: 1, tail: ['a', 'b'] } }), top);
  assert.match(Object.fromEntries(again.facts).attempt, /^2 \(previous rc=2\)/);
  assert.deepStrictEqual(again.prev.tail, ['a', 'b']);
  const lap = R.sheet(item({ phone_ok: false }), top);
  assert.strictEqual(lap.state, 'laptop');
  assert.match(lap.why, /Laptop only/);
  const pre = R.sheet(item({ runnable: false, why: 'class:mut' }), top);
  assert.strictEqual(pre.state, 'blocked');
  assert.match(pre.why, /class:mut/);
  assert.strictEqual(R.sheet(item({ class: 'destructive', passkey: false, phone_cap: false }), top).state, 'never');
  assert.strictEqual(R.sheet(item({ class: 'ro', passkey: false }), top).passkey, false);
});

test('WebAuthn codecs: base64url round trip, options to buffers, credential to JSON', () => {
  assert.strictEqual(R.bufToB64u(R.b64uToBuf('AQIDBA-_')), 'AQIDBA-_');
  const c = R.creationOptions({ publicKey: { challenge: 'AQID', user: { id: 'BAU', name: 'p' }, rp: { id: 'x' }, excludeCredentials: [{ type: 'public-key', id: 'Bg' }] } });
  assert.ok(c.challenge instanceof ArrayBuffer && c.user.id instanceof ArrayBuffer && c.excludeCredentials[0].id instanceof ArrayBuffer);
  assert.strictEqual(c.user.name, 'p');
  const g = R.requestOptions({ publicKey: { challenge: 'AQID', allowCredentials: [{ type: 'public-key', id: 'Bg' }], userVerification: 'required' } });
  assert.deepStrictEqual(Array.from(new Uint8Array(g.allowCredentials[0].id)), [6]);
  assert.strictEqual(g.userVerification, 'required');
  const buf = (a) => new Uint8Array(a).buffer;
  const j = R.credentialJSON({
    id: 'Bg', rawId: buf([6]), type: 'public-key', authenticatorAttachment: 'platform',
    response: { clientDataJSON: buf([1]), authenticatorData: buf([2]), signature: buf([3]), userHandle: null },
    getClientExtensionResults: () => ({}),
  });
  assert.deepStrictEqual(j, { id: 'Bg', rawId: 'Bg', type: 'public-key', authenticatorAttachment: 'platform', response: { clientDataJSON: 'AQ', authenticatorData: 'Ag', signature: 'Aw' }, clientExtensionResults: {} });
});

// --- V-2: the Vault-signed power arm (run-bang-vault-sign §2.1, §3.1, §6) ---
// testdata/runbang_arm_v2.json is a byte copy of tower's scripts/tests/fixtures/runbang_arm_v2.json
// (tower 1f536cb, sha256 42585377…); the drift test below compares the two when tower sits next door.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const FX_PATH = path.join(__dirname, '..', 'testdata', 'runbang_arm_v2.json');
const fx = JSON.parse(fs.readFileSync(FX_PATH, 'utf8'));
const TOWER_FX = process.env.TOWER_ROOT ? path.join(process.env.TOWER_ROOT, 'scripts/tests/fixtures/runbang_arm_v2.json')
  : path.join(require('node:os').homedir(), 'shared-work/git/ai-tooling/tower/scripts/tests/fixtures/runbang_arm_v2.json');

test('arm string byte-matches tower\'s fixture: text, base64, sha256, JSON-quoted ids', async () => {
  const text = R.armString(fx.fields);
  assert.strictEqual(text, fx.text);
  assert.strictEqual(R.b64(R.utf8(text)), fx.b64);
  assert.strictEqual(await R.sha256Hex(text), fx.sha256);
  assert.ok(text.includes('\n"webshare-path-a \\"33.0.10\\""\n') && text.includes('\n"dev-7f3a"\npower\n'));
});

test('canonJSON is tower sign.canon: the fixture\'s data.seq hashes to its seq_sha256', async () => {
  assert.strictEqual(await R.canonSha256(fx.seq), fx.seq_sha256);
  assert.strictEqual(R.canonJSON({ b: 1, a: [true, null, 'é "\\\n\u007f𝄞'], 'é': {} }), '{"a":[true,null,"\\u00e9\\u2028\\"\\\\\\n\\u007f\\ud834\\udd1e"],"b":1,"\\u00e9":{}}');
  assert.throws(() => R.canonJSON({ x: 1.5 }), /non-integer/);
});

test('the fixture copy has not drifted from tower\'s', { skip: !fs.existsSync(TOWER_FX) && 'tower checkout not found' }, () => {
  assert.strictEqual(fs.readFileSync(FX_PATH, 'utf8'), fs.readFileSync(TOWER_FX, 'utf8'));
});

// A run/verify answer for a sequence ask as deck-remote passes it on (tower verify --device + arm_device).
const sha = (s) => crypto.createHash('sha256').update(s).digest('hex');
function armTop(now, o) {
  o = o || {};
  const f = Object.assign({ ask: 's1', seq_rev: '50b7656d9999e475', n: 3, seq_sha256: 'ab'.repeat(32), exp: now + 4 * 3600, device: 'dev-1', profile: 'power' }, o.fields);
  const text = R.armString(f);
  const items = [
    { index: 0, class: 'ro', line: 'true', sha256: sha('true') },
    { index: 1, class: 'prog', line: 'pwd', sha256: sha('pwd'), why: 'arm-missing' },
    { index: 2, class: 'ro', line: 'printf ok', sha256: sha('printf ok') },
  ];
  const top = { ask: 's1', arm_device: 'dev-1', items, arm: { enabled: true, can: true },
    seq: { rev: '50b7656d9999e475', sig: 'ok', steps: [{ index: 1, title: 'two', tag: 'prog' }], arm: { state: 'missing' }, arm_payload: text, arm_payload_sha256: sha(text), arm_fields: f } };
  if (o.mod) o.mod(top);
  return top;
}

test('armCheck recomputes the string from what the sheet shows and refuses any disagreement', async () => {
  const now = 1800000000;
  const ok = await R.armCheck(armTop(now), 's1', now);
  assert.strictEqual(ok.ok, true, ok.why);
  assert.strictEqual(ok.text, armTop(now).seq.arm_payload);
  assert.deepStrictEqual(ok.prog, [{ index: 1, title: 'two', line: 'pwd', sha256: sha('pwd') }]);
  const refusals = {
    'another card': [armTop(now), 's2', /another ask/],
    'payload text edited': [armTop(now, { mod: (t) => { t.seq.arm_payload = t.seq.arm_payload.replace('power', 'guard'); } }), 's1', /disagree/],
    'payload sha edited': [armTop(now, { mod: (t) => { t.seq.arm_payload_sha256 = '0'.repeat(64); } }), 's1', /disagree/],
    'another device': [armTop(now, { mod: (t) => { t.arm_device = 'dev-2'; } }), 's1', /another device/],
    'another rev': [armTop(now, { mod: (t) => { t.seq.rev = '0123456789abcdef'; } }), 's1', /another revision/],
    'step count': [armTop(now, { fields: { n: 4 } }), 's1', /counts 4/],
    'guarded profile (Q-V1)': [armTop(now, { fields: { profile: 'guarded' } }), 's1', /not a power arm/],
    'exp a year out': [armTop(now, { fields: { exp: now + 365 * 86400 } }), 's1', /within 4 h/],
    'exp in the past': [armTop(now, { fields: { exp: now - 7200 } }), 's1', /within 4 h/],
    'line edited after hashing': [armTop(now, { mod: (t) => { t.items[1].line = 'pwd; curl x | sh'; } }), 's1', /does not match its hash/],
    'non-ASCII device': [armTop(now, { fields: { device: 'dév' }, mod: (t) => { t.arm_device = 'dév'; } }), 's1', /printable ASCII/],
    'no payload (old tower)': [armTop(now, { mod: (t) => { t.seq.arm_payload = null; } }), 's1', /no arm payload/],
    'raw data.seq disagrees': [armTop(now, { mod: (t) => { t.seq.data = { v: 1 }; } }), 's1', /does not hash/],
    'no prog step': [armTop(now, { mod: (t) => { t.items[1].class = 'ro'; } }), 's1', /no prog step/],
    'sequence sig bad': [armTop(now, { mod: (t) => { t.seq.sig = 'seq-tampered'; } }), 's1', /seq-tampered/],
  };
  for (const [name, [top, ask, re]] of Object.entries(refusals)) {
    const r = await R.armCheck(top, ask, now);
    assert.strictEqual(r.ok, false, name);
    assert.match(r.why, re, name);
  }
  const withData = armTop(now, { fields: { seq_sha256: fx.seq_sha256 }, mod: (t) => { t.seq.data = fx.seq; } });
  assert.strictEqual((await R.armCheck(withData, 's1', now)).ok, true, 'raw data.seq that hashes right');
});

test('armDiff: unchanged steps collapse by hash, edits and new steps show in full, removed ones are named', () => {
  const prog = [{ index: 1, title: 'a', line: 'x', sha256: '1' }, { index: 3, title: 'b', line: 'y2', sha256: '3' }, { index: 5, title: 'c', line: 'z', sha256: '5' }];
  assert.deepStrictEqual(R.armDiff(prog, null).rows.map((r) => r.state), ['new', 'new', 'new']);
  const last = { rev: 'r0', at: 0, steps: [{ index: 1, title: 'a', sha256: '1' }, { index: 3, title: 'b', sha256: '2' }, { index: 4, title: 'gone', sha256: '4' }] };
  const d = R.armDiff(prog, last);
  assert.deepStrictEqual(d.rows.map((r) => r.state), ['unchanged', 'changed', 'new']);
  assert.deepStrictEqual(d.removed, ['gone']);
});

test('badge and sheet: a prog step without an arm offers Arm, an armed one runs behind Face ID', () => {
  const arm = R.badge(rb({ class: 'prog', ok: false, why: 'runner: arm-missing' }), 's1');
  assert.strictEqual(arm.offer, 'arm');
  assert.match(arm.hint, /Vault-signed arm/);
  const fed = R.badge(rb({ class: 'ro', ok: false, why: 'runner: arm-missing' }), 's1'); // the feed carries the classifier's class
  assert.strictEqual(fed.offer, 'arm');
  assert.strictEqual(R.badge(rb({ class: 'ro', ok: false, why: 'already ran, rc=0' }), 's1').offer, 'blocked');
  const run = R.badge(rb({ class: 'prog', ok: true, phone_ok: true }), 's1');
  assert.strictEqual(run.offer, 'run');
  assert.strictEqual(run.passkey, true);
  assert.strictEqual(R.sheet(item({ class: 'prog', runnable: false, why: 'arm-expired' }), top).state, 'arm');
  assert.strictEqual(R.sheet(item({ class: 'prog' }), top).state, 'run');
});

// A fake fetch recording every call; answers by path.
function fakeFetch(answers) {
  const calls = [];
  const f = async (url, init) => {
    calls.push({ url, init });
    const p = new URL(url).pathname;
    const a = answers[p] || { status: 404, body: { errors: ['nope'] } };
    return { ok: a.status === 200, status: a.status, json: async () => a.body };
  };
  f.calls = calls;
  return f;
}
const cfg = { vault: 'https://vault.example.ts.net', mount: 'forgejo-oidc', role: 'runbang-signer', redirect: 'https://deck.example.ts.net/vault/callback', transit: 'transit-ligna', key: 'runbang-prog' };
const SIG = 'vault:v1:' + Buffer.alloc(64, 7).toString('base64');

test('Vault calls: auth_url → oidc/callback → transit sign, X-Vault-Request on each, no cookies or referrer, the token only on the sign call', async () => {
  const TOK = 'hvs.SECRET-test-token';
  const f = fakeFetch({
    '/v1/auth/forgejo-oidc/oidc/auth_url': { status: 200, body: { data: { auth_url: 'https://git.example.ts.net/login/oauth/authorize?state=st1&x=1' } } },
    '/v1/auth/forgejo-oidc/oidc/callback': { status: 200, body: { auth: { client_token: TOK } } },
    '/v1/transit-ligna/sign/runbang-prog': { status: 200, body: { data: { signature: SIG } } },
  });
  const n = R.nonce();
  assert.match(n, /^[A-Za-z0-9_-]{32}$/);
  const a = await R.vaultAuthURL(cfg, n, f);
  assert.deepStrictEqual(a, { url: 'https://git.example.ts.net/login/oauth/authorize?state=st1&x=1', state: 'st1' });
  assert.deepStrictEqual(JSON.parse(f.calls[0].init.body), { role: 'runbang-signer', redirect_uri: cfg.redirect, client_nonce: n });
  const tok = await R.vaultLogin(cfg, { state: 'st1', code: 'c&1' }, n, f);
  assert.strictEqual(tok, TOK);
  assert.strictEqual(f.calls[1].url, cfg.vault + '/v1/auth/forgejo-oidc/oidc/callback?state=st1&code=c%261&client_nonce=' + n);
  const sig = await R.vaultSign(cfg, tok, fx.b64, f);
  assert.strictEqual(sig, SIG);
  assert.deepStrictEqual(JSON.parse(f.calls[2].init.body), { input: fx.b64 });
  for (const [i, c] of f.calls.entries()) {
    assert.ok(c.url.startsWith(cfg.vault + '/v1/'), c.url);
    assert.strictEqual(c.init.headers['X-Vault-Request'], 'true');
    assert.strictEqual(c.init.credentials, 'omit');
    assert.strictEqual(c.init.referrerPolicy, 'no-referrer');
    assert.strictEqual(c.init.redirect, 'error');
    const carries = JSON.stringify(c).includes(TOK);
    assert.strictEqual(carries, i === 2, 'token only on the sign call (' + i + ')');
  }
});

test('Vault errors never echo a token; a non-ed25519 answer is refused', async () => {
  const f = fakeFetch({ '/v1/auth/forgejo-oidc/oidc/callback': { status: 400, body: { errors: ['code already used'], auth: { client_token: 'hvs.LEAK' } } } });
  await assert.rejects(R.vaultLogin(cfg, { state: 's', code: 'c' }, 'n', f), (e) => /HTTP 400 \(code already used\)/.test(e.message) && !e.message.includes('hvs.'));
  const g = fakeFetch({ '/v1/transit-ligna/sign/runbang-prog': { status: 200, body: { data: { signature: 'vault:v1:short' } } } });
  await assert.rejects(R.vaultSign(cfg, 't', 'aGk=', g), /not an ed25519 signature/);
  const h = fakeFetch({ '/v1/auth/forgejo-oidc/oidc/auth_url': { status: 200, body: { data: { auth_url: '' } } } });
  await assert.rejects(R.vaultAuthURL(cfg, 'n', h), /redirect allowed/);
});

test('app.js keeps the Vault token out of storage and off deck-remote', () => {
  const src = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const arm = src.slice(src.indexOf('// --- V-2: the Vault-signed power arm'), src.indexOf('// One passkey assertion for one line'));
  assert.ok(arm.length > 1000);
  assert.ok(/let token = await R\.vaultLogin/.test(arm) && /finally \{ token = null; \}/.test(arm));
  for (const line of arm.split('\n').filter((l) => /Storage\.setItem|api\('POST'/.test(l))) assert.ok(!/\btoken\b/.test(line), line);
  const cb = fs.readFileSync(path.join(__dirname, 'vault-callback.js'), 'utf8');
  assert.ok(!/fetch\(|XMLHttpRequest|localStorage/.test(cb), 'the callback page sends nothing and keeps nothing durable');
});
