// Run: node --test web/runner.test.js
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const M = require('./runner.js');

const REV = '50b7656d9999e475';
const NONCE = '0123456789abcdef0123456789abcdef';
const ELEV = 'ask1:elev:50b7656d:20261103T010203Z';

// top(o): a run/verify answer for a 3-step runner ask; o overrides runner / seq fields.
function top(o = {}) {
  const t = {
    ask: 'ask1', status: 'open', arm_device: 'dev1',
    items: [
      { index: 0, class: 'ro', sha256: 'a'.repeat(64), run: { state: 'done', rc: 0, id: 'ask1:0:aaaaaaaa:r50b7656d' } },
      { index: 1, class: 'prog', sha256: 'b'.repeat(64), run: null },
      { index: 2, class: 'prog', sha256: 'c'.repeat(64), run: null },
    ],
    seq: { title: 'A5 deploy', rev: REV, sig: 'ok', pointer: 1, state: 'ready', arm: { state: 'missing' },
      steps: [{ index: 0, title: 'check' }, { index: 1, title: 'build' }, { index: 2, title: 'ssh hop' }] },
    runner: { source: { path: '/Users/op/runners/a5.sh', sha256: 'e3b0c442' + 'f'.repeat(56) }, prelude_lines: 12, needs_arm: false,
      phone_steps: [0, 1], laptop_steps: [2], elevation: { state: 'none' }, gate: { pending: false } },
  };
  Object.assign(t.runner, o.runner || {});
  Object.assign(t.seq, o.seq || {});
  return t;
}

test('sheetModel: file, revision, every step with class and phone/laptop, prelude size', () => {
  const m = M.sheetModel(top(), 'ask1');
  assert.strictEqual(m.ok, true);
  assert.strictEqual(m.startable, true);
  assert.deepStrictEqual(m.source, { path: '/Users/op/runners/a5.sh', sha8: 'e3b0c442' });
  assert.strictEqual(m.rev, REV);
  assert.strictEqual(m.preludeLines, 12);
  assert.deepStrictEqual(m.steps.map((s) => [s.index, s.title, s.class, s.where]),
    [[0, 'check', 'ro', 'phone'], [1, 'build', 'prog', 'phone'], [2, 'ssh hop', 'prog', 'laptop']]);
});

test('sheetModel: not a runner, other ask, bad signature are not startable', () => {
  const t = top();
  delete t.runner;
  const nr = M.sheetModel(t, 'ask1');
  assert.strictEqual(nr.ok, false);
  assert.match(nr.why, /not a runner/);
  assert.strictEqual(M.sheetModel(top(), 'ask2').ok, false);
  const bad = M.sheetModel(top({ seq: { sig: 'tampered' } }), 'ask1');
  assert.strictEqual(bad.startable, false);
  assert.match(bad.why, /tampered/);
});

test('sheetModel: needs_arm without an arm offers Arm…; armed is startable; ro/mut-only needs none', () => {
  const un = M.sheetModel(top({ runner: { needs_arm: true } }), 'ask1');
  assert.strictEqual(un.offerArm, true);
  assert.strictEqual(un.startable, false);
  assert.match(un.why, /arm-missing/);
  const armed = M.sheetModel(top({ runner: { needs_arm: true }, seq: { arm: { state: 'armed' } } }), 'ask1');
  assert.strictEqual(armed.offerArm, false);
  assert.strictEqual(armed.startable, true);
  assert.strictEqual(M.sheetModel(top(), 'ask1').offerArm, false);
});

test('sheetModel: this phone\'s live elevation resumes; another device\'s blocks', () => {
  const mine = M.sheetModel(top({ runner: { elevation: { state: 'elevated', id: ELEV, device: 'dev1', exp: 1792720983 } } }), 'ask1');
  assert.strictEqual(mine.resume, true);
  assert.strictEqual(mine.startable, false);
  const other = M.sheetModel(top({ runner: { elevation: { state: 'elevated', id: ELEV, device: 'dev9' } } }), 'ask1');
  assert.strictEqual(other.resume, false);
  assert.match(other.why, /another device/);
});

test('sheetModel: a runner file changed since signing is not startable', () => {
  const m = M.sheetModel(top({ runner: { file: 'sha-mismatch' } }), 'ask1');
  assert.strictEqual(m.startable, false);
  assert.match(m.why, /sha-mismatch/);
  assert.strictEqual(M.sheetModel(top({ runner: { file: 'ok' } }), 'ask1').startable, true);
});

test('sheetModel: a complete runner is not startable', () => {
  assert.strictEqual(M.sheetModel(top({ seq: { pointer: null } }), 'ask1').startable, false);
});

test('gateView: buttons per gate kind (run, failed, word, laptop, unknown)', () => {
  const g = (kind, o) => M.gateView(Object.assign({ pending: true, step: 1, attempt: 1, kind, nonce: NONCE, title: 'build', class: 'prog' }, o || {}));
  const ds = (v) => v.buttons.map((b) => b.decision);
  assert.deepStrictEqual(ds(g('run')), ['run', 'skip', 'quit']);
  assert.deepStrictEqual(ds(g('failed', { attempt: 2 })), ['retry', 'skip', 'quit']);
  const w = g('word', { word_len: 13 });
  assert.deepStrictEqual(ds(w), ['run', 'skip', 'quit']);
  assert.strictEqual(w.word, true);
  assert.strictEqual(w.wordLen, 13);
  const l = g('laptop');
  assert.deepStrictEqual(ds(l), ['skip', 'quit']);
  assert.match(l.note, /do it at the laptop, or Skip/);
  assert.deepStrictEqual(ds(g('mystery')), ['quit']);
  assert.strictEqual(M.gateView({ pending: false }), null);
  assert.strictEqual(M.gateView(null), null);
});

test('decisionBody: only offered decisions, nonce required, the word only for a word gate\'s run', () => {
  const word = { pending: true, step: 1, kind: 'word', nonce: NONCE };
  assert.deepStrictEqual(M.decisionBody(ELEV, word, 'run', 'orchid-delete'), { elevation: ELEV, nonce: NONCE, decision: 'run', word: 'orchid-delete' });
  assert.throws(() => M.decisionBody(ELEV, word, 'run', ''), /retype the word/);
  assert.throws(() => M.decisionBody(ELEV, word, 'run', 'x'.repeat(65)), /retype the word/);
  assert.throws(() => M.decisionBody(ELEV, word, 'run', 'a\nb'), /retype the word/);
  assert.deepStrictEqual(M.decisionBody(ELEV, word, 'skip', 'ignored'), { elevation: ELEV, nonce: NONCE, decision: 'skip' });
  const laptop = { pending: true, step: 2, kind: 'laptop', nonce: NONCE };
  assert.throws(() => M.decisionBody(ELEV, laptop, 'run'), /not offered/);
  assert.throws(() => M.decisionBody(ELEV, { pending: true, kind: 'run' }, 'run'), /no nonce/);
  assert.throws(() => M.decisionBody(ELEV, { pending: false }, 'run'), /no gate/);
});

test('currentRun: the gate poll\'s running step wins, else a running item from verify', () => {
  assert.deepStrictEqual(M.currentRun({ pending: false, running: { step: 1, id: 'ask1:1:bbbbbbbb:r50b7656d' } }, top()), { step: 1, id: 'ask1:1:bbbbbbbb:r50b7656d' });
  const t = top();
  t.items[2].run = { state: 'running', id: 'ask1:2:cccccccc:r50b7656d' };
  assert.deepStrictEqual(M.currentRun({ pending: false }, t), { step: 2, id: 'ask1:2:cccccccc:r50b7656d' });
  assert.strictEqual(M.currentRun({ pending: true, step: 1 }, top()), null);
});

test('banner: title, step k/n, expiry and the connection state', () => {
  const m = M.sheetModel(top({ runner: { elevation: { state: 'elevated', id: ELEV, device: 'dev1', exp: 1792720983, idle_exp: 1792714683 } } }), 'ask1');
  const b = M.banner(m, { pending: true, step: 1 }, {});
  assert.match(b, /^Runner A5 deploy · step 2\/3 · ends \d\d:\d\dZ \(idle \d\d:\d\dZ\) · waiting for you$/);
  assert.match(M.banner(m, null, { reconnecting: true }), /reconnecting/);
});

test('backoff: 2 s, doubling on errors, capped at 30 s', () => {
  assert.strictEqual(M.backoff(0), 2000);
  assert.strictEqual(M.backoff(1), 4000);
  assert.strictEqual(M.backoff(2), 8000);
  assert.strictEqual(M.backoff(9), 30000);
});

const err = (status, data) => Object.assign(new Error('x'), { status, data });

test('Poller: a dropped connection is never a result; the runner\'s "ended" is', async () => {
  let gateAns = () => ({ pending: true, step: 1, kind: 'run', nonce: NONCE });
  const elevated = top({ runner: { elevation: { state: 'elevated', id: ELEV, device: 'dev1' } } });
  let verifyAns = () => elevated;
  const ups = [];
  const p = new M.Poller({ gate: async () => gateAns(), verify: async () => verifyAns(), onUpdate: (u) => ups.push(u) });
  assert.strictEqual(await p.tick(), 2000);
  assert.strictEqual(ups[0].gate.nonce, NONCE);
  gateAns = () => { throw new TypeError('Failed to fetch'); }; // deck-remote restarting
  assert.strictEqual(await p.tick(), 4000);
  assert.strictEqual(ups[1].conn.reconnecting, true);
  assert.strictEqual(ups[1].conn.over, false);
  gateAns = () => { throw err(502, { error: 'bad gateway' }); };
  assert.strictEqual(await p.tick(), 8000);
  gateAns = () => { throw err(403, { error: 'not the operator' }); }; // no runner verdict: keep trying
  assert.strictEqual(await p.tick(), 16000);
  gateAns = () => ({ pending: false });
  assert.strictEqual(await p.tick(), 2000, 'back to 2 s after a good answer');
  assert.strictEqual(ups[4].conn.reconnecting, false);
  gateAns = () => { throw err(403, { refused: 'expired' }); };
  assert.strictEqual(await p.tick(), null);
  assert.strictEqual(ups[5].conn.over, true);
  assert.strictEqual(p.reason, 'expired');
  assert.strictEqual(await p.tick(), null, 'stays over');
});

test('Poller: verify reporting the elevation ended stops polling with its reason', async () => {
  const p = new M.Poller({ gate: async () => ({ pending: false }), verify: async () => top({ runner: { elevation: { state: 'ended', reason: 'complete' } } }) });
  assert.strictEqual(await p.tick(), null);
  assert.strictEqual(p.reason, 'ended (complete)');
});

test('Poller: verify 404 (the ask closed) ends the view', async () => {
  const p = new M.Poller({ gate: async () => ({ pending: false }), verify: async () => { throw err(404, { error: 'ask is no longer open' }); } });
  assert.strictEqual(await p.tick(), null);
  assert.strictEqual(p.reason, 'the ask is closed');
});

test('gateView: the exact step body and sha8 travel with the gate; a failed destructive step needs the word to retry', () => {
  const g = M.gateView({ pending: true, step: 1, kind: 'failed', attempt: 2, nonce: NONCE, body: 'rm -rf "$DIR"⟨U+200B⟩', sha256: 'abcdef12' + '0'.repeat(56), word_len: 6 });
  assert.strictEqual(g.body, 'rm -rf "$DIR"⟨U+200B⟩');
  assert.strictEqual(g.sha8, 'abcdef12');
  assert.strictEqual(g.word, true);
  assert.deepStrictEqual(g.buttons.map((b) => b.decision), ['retry', 'skip', 'quit']);
  const gate = { pending: true, step: 1, kind: 'failed', nonce: NONCE, word_len: 6 };
  assert.deepStrictEqual(M.decisionBody(ELEV, gate, 'retry', 'delete'), { elevation: ELEV, nonce: NONCE, decision: 'retry', word: 'delete' });
  assert.throws(() => M.decisionBody(ELEV, gate, 'retry', ''), /retype the word/);
  assert.strictEqual(M.decisionBody(ELEV, { pending: true, kind: 'failed', nonce: NONCE }, 'retry').word, undefined, 'plain failed: no word');
  assert.strictEqual(M.gateView({ pending: true, kind: 'run', nonce: NONCE }).body, null);
});

test('decisionBody: the word has no spaces; the nonce is exactly 32 lowercase hex', () => {
  const word = { pending: true, step: 1, kind: 'word', nonce: NONCE };
  assert.throws(() => M.decisionBody(ELEV, word, 'run', 'orchid delete'), /retype the word/);
  assert.throws(() => M.decisionBody(ELEV, Object.assign({}, word, { nonce: NONCE.slice(1) }), 'skip'), /no nonce/);
  assert.throws(() => M.decisionBody(ELEV, Object.assign({}, word, { nonce: NONCE.toUpperCase() }), 'skip'), /no nonce/);
});

test('sheetModel: prelude text and cd from the runner block', () => {
  const m = M.sheetModel(top({ runner: { prelude: 'export X=1\nLIB=⟨U+0007⟩', cd: '/Users/op/src' } }), 'ask1');
  assert.strictEqual(m.prelude, 'export X=1\nLIB=⟨U+0007⟩');
  assert.strictEqual(m.cd, '/Users/op/src');
  const n = M.sheetModel(top({ runner: { cd: null } }), 'ask1');
  assert.strictEqual(n.cd, null);
  assert.strictEqual(n.prelude, null);
});

test('ARM_FIX: arm-* and runner-prelude-unarmed are fixed by Arm…', () => {
  assert.strictEqual(M.ARM_FIX('arm-missing'), true);
  assert.strictEqual(M.ARM_FIX('runner-prelude-unarmed'), true);
  assert.strictEqual(M.ARM_FIX('runner-file-changed'), false);
});

test('sheetModel: a run:mut phone without run:prog starts a Face-ID-only runner, not an arm-needing one', () => {
  const plain = top();
  plain.runner.needs_arm = false;
  assert.equal(M.sheetModel(plain, 'ask1', { prog: false }).startable, true);
  const armed = top();
  armed.runner.needs_arm = true;
  armed.seq.arm = { state: 'armed' };
  const m = M.sheetModel(armed, 'ask1', { prog: false });
  assert.equal(m.startable, false);
  assert.match(m.why, /lacks the run:prog capability/);
  assert.equal(m.offerArm, false);
  assert.equal(M.sheetModel(armed, 'ask1', { prog: true }).startable, true);
  assert.equal(M.sheetModel(armed, 'ask1').startable, true);      // no caps given: the server decides
});

test('cleanComplete: only an ended (complete) runner whose every step ran rc=0 closes itself', () => {
  const ok = (i) => ({ index: i, run: { state: 'done', rc: 0 } });
  const t = (items) => ({ items });
  assert.equal(M.cleanComplete('ended (complete)', t([ok(0), ok(1)])), true);
  assert.equal(M.cleanComplete('ended (complete)', t([ok(0), { index: 1, run: { state: 'done', rc: 1 } }])), false, 'a failed step');
  assert.equal(M.cleanComplete('ended (complete)', t([ok(0), { index: 1, run: null }])), false, 'a skipped step');
  assert.equal(M.cleanComplete('ended (complete)', t([ok(0), { index: 1, run: { state: 'refused' } }])), false);
  assert.equal(M.cleanComplete('ended (quit)', t([ok(0)])), false);
  assert.equal(M.cleanComplete('ended (operator)', t([ok(0)])), false);
  assert.equal(M.cleanComplete('expired', t([ok(0)])), false);
  assert.equal(M.cleanComplete('ended (complete)', null), false);
  assert.equal(M.cleanComplete('ended (complete)', t([])), false);
});

test('Poller: gate refusing "ended" still reads verify for the end reason and the final step results', async () => {
  const done = top({ runner: { elevation: { state: 'ended', reason: 'complete' } }, seq: { pointer: null } });
  done.items.forEach((it) => { it.run = { state: 'done', rc: 0 }; });
  const ups = [];
  const p = new M.Poller({
    gate: async () => { const e = new Error('refused'); e.status = 403; e.data = { refused: 'ended' }; throw e; },
    verify: async () => done,
    onUpdate: (u) => ups.push(u),
  });
  assert.strictEqual(await p.tick(), null);
  assert.strictEqual(p.reason, 'ended (complete)');
  assert.strictEqual(ups[0].top, done, 'onUpdate got the fresh top');
  assert.strictEqual(M.cleanComplete(p.reason, ups[0].top), true);
});

test('Poller: gate refusing "ended" while verify still says elevated (or fails) keeps the gate reason', async () => {
  const live = top({ runner: { elevation: { state: 'elevated', device: 'dev1' } } });
  const gate = async () => { const e = new Error('refused'); e.status = 403; e.data = { refused: 'ended' }; throw e; };
  const a = new M.Poller({ gate, verify: async () => live });
  assert.strictEqual(await a.tick(), null);
  assert.strictEqual(a.reason, 'ended');
  const b = new M.Poller({ gate, verify: async () => { throw new Error('boom'); } });
  assert.strictEqual(await b.tick(), null);
  assert.strictEqual(b.reason, 'ended');
});
