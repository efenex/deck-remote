// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { mergeFreshHistory, trimOverlap, applyAskState, sendStatusLabel, canRetry, promoteDelivered, foldSummary, firstLine } = require('./thread.js');

const h = (role, content, ts) => ({ role, content, ts, hist: true });
const live = (role, content, ts) => ({ role, content, ts });

test('reopening a thread does not duplicate history', () => {
  let conv = [];
  const page = [h('me', 'a'), h('reply', 'b'), h('me', 'c')];
  for (let i = 0; i < 3; i++) conv = mergeFreshHistory(page.map((t) => ({ ...t })), conv);
  assert.deepStrictEqual(conv.map((t) => t.content), ['a', 'b', 'c']);
});

test('live rows survive a reload; ones now in the transcript are not doubled', () => {
  const conv = [h('me', 'a'), live('me', 'sent'), live('reply', 'answer'), live('me', 'pending')];
  const fresh = [h('me', 'a'), h('me', 'sent'), h('reply', 'answer')];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.content), ['a', 'sent', 'answer', 'pending']);
});

test('notes and errors are kept', () => {
  const conv = [live('note', 'x'), live('error', 'boom')];
  assert.strictEqual(mergeFreshHistory([h('me', 'a')], conv).length, 3);
});

test('a genuine repeat is kept', () => {
  const conv = [h('me', 'yes', 1000), live('me', 'yes', 5000.5)];
  const fresh = [h('me', 'yes', 1000)];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.content), ['yes', 'yes']);
});

test('older page overlapping the head is trimmed at the boundary only', () => {
  const head = [h('me', 'c'), h('reply', 'd'), h('me', 'e')];
  const older = [h('me', 'ok'), h('reply', 'b'), h('me', 'c'), h('reply', 'd')];
  assert.deepStrictEqual(trimOverlap(older, head).map((t) => t.content), ['ok', 'b']);
  const repeat = [h('me', 'c'), h('reply', 'x')];
  assert.strictEqual(trimOverlap(repeat, head).length, 2);
});

test('a live row is matched to its transcript copy by time', () => {
  const conv = [h('me', 'yes', 1000), live('me', 'yes', 5000.5)];
  const fresh = [h('me', 'yes', 1000), h('reply', 'ok', 4990), h('me', 'yes', 5001)];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.ts), [1000, 4990, 5001]);
});

const { bashBlockHTML, turnKey } = require('./thread.js');

test('bashBlockHTML: command and stdout, escaped', () => {
  const html = bashBlockHTML({ role: 'bash', content: "echo '<b>'", stdout: '<script>alert(1)</script> & "x"', stderr: '' });
  assert.match(html, /<div class="bash-cmd">echo &#39;&lt;b&gt;&#39;<\/div>/);
  assert.match(html, /<pre class="bash-out">&lt;script&gt;alert\(1\)&lt;\/script&gt; &amp; &quot;x&quot;<\/pre>/);
  assert.ok(!html.includes('<script>') && !html.includes('<b>'));
  assert.ok(!html.includes('bash-err'));
});

test('bashBlockHTML: no `$` prompt of its own (the row gutter already shows one)', () => {
  const html = bashBlockHTML({ role: 'bash', content: 'ls -la', stdout: 'total 0', stderr: '' });
  assert.ok(!html.includes('bash-prompt'));
  assert.strictEqual(html.indexOf('$'), -1);
  assert.match(html, /<div class="bash-cmd">ls -la<\/div>/);
});

test('bashBlockHTML: empty stdout and stderr shows just the command', () => {
  const html = bashBlockHTML({ role: 'bash', content: 'true', stdout: '', stderr: '' });
  assert.match(html, /bash-cmd/);
  assert.ok(!html.includes('<pre'));
});

test('bashBlockHTML: stderr only renders in the error element', () => {
  const html = bashBlockHTML({ role: 'bash', content: 'ls /nope', stdout: '', stderr: 'ls: /nope: No such file' });
  assert.ok(!html.includes('bash-out'));
  assert.match(html, /<pre class="bash-err">ls: \/nope: No such file<\/pre>/);
});

test('bashBlockHTML: multi-line output keeps its lines', () => {
  const html = bashBlockHTML({ role: 'bash', content: 'x', stdout: 'l1\nl2\n\nl4', stderr: 'e1\ne2' });
  assert.ok(html.includes('l1\nl2\n\nl4'));
  assert.ok(html.includes('e1\ne2'));
});

test('bashBlockHTML: nothing at all renders nothing; output-only is kept', () => {
  assert.strictEqual(bashBlockHTML({ role: 'bash', content: '', stdout: '', stderr: ' \n' }), '');
  assert.match(bashBlockHTML({ role: 'bash', content: '', stdout: 'late', stderr: '' }), /bash-out/);
});

test('turnKey: bash rows with the same command but different output differ', () => {
  const a = { role: 'bash', content: 'date', stdout: '1' };
  const b = { role: 'bash', content: 'date', stdout: '2' };
  assert.notStrictEqual(turnKey(a), turnKey(b));
});

const { newTailTurns } = require('./thread.js');

test('newTailTurns: a bash row in the tail is appended once, not on every poll', () => {
  const tail = [
    h('me', 'run it', 10),
    { role: 'bash', content: 'ls', stdout: 'a\nb', stderr: '', ts: 11, hist: true },
    h('reply', 'done', 12),
  ];
  const conv = [];
  const reconcile = () => newTailTurns(conv, tail).forEach((t) => conv.push(t));
  reconcile();
  reconcile();
  reconcile();
  assert.strictEqual(conv.filter((e) => e.role === 'bash').length, 1);
  assert.strictEqual(conv.length, 3);
});

test('newTailTurns: a genuine repeat bash row (same cmd and output) still counts', () => {
  const b = { role: 'bash', content: 'date', stdout: 'x', stderr: '', ts: 1, hist: true };
  const conv = [{ ...b }];
  assert.strictEqual(newTailTurns(conv, [b, { ...b }]).length, 1);
});

// ----- phone-message delivery status -----
const me = (content, extra) => ({ role: 'me', content, queued: true, status: 'sent', ...extra });

test('ask-state walks a bubble sent -> queued (n) -> delivering -> delivered', () => {
  const conv = [me('hi')];
  applyAskState(conv, { requestId: 'r1', state: 'sent' });
  assert.strictEqual(conv[0].requestId, 'r1'); // tagged even if it beat the POST response
  assert.strictEqual(sendStatusLabel(conv[0]), 'sent');
  applyAskState(conv, { requestId: 'r1', state: 'queued', position: 3 });
  assert.strictEqual(sendStatusLabel(conv[0]), 'queued (3)');
  applyAskState(conv, { requestId: 'r1', state: 'delivering' });
  assert.strictEqual(conv[0].queued, true);
  applyAskState(conv, { requestId: 'r1', state: 'delivered' });
  assert.strictEqual(sendStatusLabel(conv[0]), 'delivered');
  assert.strictEqual(conv[0].queued, false);
});

test('events land on their own bubble, not a neighbour', () => {
  const conv = [me('one', { requestId: 'r1' }), me('two', { requestId: 'r2' })];
  applyAskState(conv, { requestId: 'r2', state: 'queued', position: 2 });
  assert.strictEqual(conv[0].status, 'sent');
  assert.strictEqual(conv[1].status, 'queued');
  assert.strictEqual(applyAskState(conv, { requestId: 'nope', state: 'delivered' }), null);
});

test('failed shows its reason and offers Retry; unconfirmed does not', () => {
  const conv = [me('a', { requestId: 'r1' }), me('b', { requestId: 'r2' })];
  applyAskState(conv, { requestId: 'r1', state: 'failed', error: 'Not delivered: target not running', retryable: true });
  assert.strictEqual(sendStatusLabel(conv[0]), 'failed: Not delivered: target not running');
  assert.strictEqual(conv[0].queued, false);
  assert.ok(canRetry(conv[0]));
  applyAskState(conv, { requestId: 'r2', state: 'unconfirmed' });
  assert.ok(!canRetry(conv[1])); // may already be in the composer: no blind resend
  assert.match(sendStatusLabel(conv[1]), /unconfirmed/);
});

test('a reply clearing queued silences a stale in-flight label', () => {
  const e = me('x', { status: 'queued', position: 1 });
  e.queued = false;
  assert.strictEqual(sendStatusLabel(e), '');
});

test('the transcript showing a bubble promotes it to delivered, even after "unconfirmed"', () => {
  const conv = [me('ship it', { requestId: 'r1', status: 'unconfirmed', queued: false, ts: 1000 })];
  assert.strictEqual(promoteDelivered(conv, [h('me', 'ship it', 1010)]), 1);
  assert.strictEqual(sendStatusLabel(conv[0]), 'delivered');
  // a late agent-deck verdict cannot take it back
  applyAskState(conv, { requestId: 'r1', state: 'unconfirmed' });
  assert.strictEqual(conv[0].status, 'delivered');
});

test('promotion is one-for-one and an older identical row proves nothing', () => {
  const conv = [me('yes', { ts: 1000 }), me('yes', { ts: 1001 })];
  assert.strictEqual(promoteDelivered(conv, [h('me', 'yes', 10)]), 0); // from long ago
  assert.strictEqual(promoteDelivered(conv, [h('me', 'yes', 1005)]), 1); // one row, one bubble
  assert.deepStrictEqual(conv.map((e) => e.status), ['delivered', 'sent']);
  assert.strictEqual(promoteDelivered([me('a', { ts: 1 })], [h('reply', 'a', 5)]), 0); // only user rows count
});

test('peer rows collapse to sender + first line; slash rows to the command', () => {
  const names = { abc12345: 'tower' };
  const nameOf = (id) => names[id] || '';
  const peer = { role: 'peer', origin: 'peer:abc12345', content: '\n  [tower → dr] task x\nmore detail' };
  assert.strictEqual(foldSummary(peer, nameOf), 'tower \u00b7 [tower → dr] task x');
  // unknown session: a short id; an envelope-provided name wins
  assert.strictEqual(foldSummary({ role: 'peer', origin: 'peer:zzzzzzzz-1792', content: 'hi' }, nameOf), 'zzzzzzzz \u00b7 hi');
  assert.strictEqual(foldSummary({ role: 'peer', origin: 'peer:q', sender: 'ga-offload', content: 'eta' }, nameOf), 'ga-offload \u00b7 eta');
  assert.strictEqual(foldSummary({ role: 'slash', content: '/model opus\nignored' }, nameOf), '/model opus');
  assert.strictEqual(firstLine(null), '');
});

// Stale-state repro (2026-10-21): the server omits falsy live fields (omitempty),
// so a session that went working -> idle overnight arrives WITHOUT `working`.
// A plain Object.assign(prev, fresh) kept last night's "running" until restart.
const { mergeSessionRow } = require('./thread.js');
test('mergeSessionRow: a field the server dropped is cleared, not kept', () => {
  const prev = { id: 'a', title: 'A', working: true, stalled: true, activity: 'Channelling… (3m)', currentTool: 'Bash(x)', needsAttention: true, lastReply: 'old', lastActivity: 100, state: 'working' };
  const fresh = { id: 'a', title: 'A', status: 'waiting', state: 'idle', lastReply: 'new', lastActivity: 200 };
  const naive = Object.assign({}, prev, fresh);
  assert.strictEqual(naive.working, true, 'the old merge keeps the stale flag (the bug)');
  const m = mergeSessionRow(prev, fresh);
  assert.strictEqual(m.working, false);
  assert.strictEqual(m.stalled, false);
  assert.strictEqual(m.needsAttention, false);
  assert.strictEqual(m.activity, '');
  assert.strictEqual(m.currentTool, '');
  assert.strictEqual(m.state, 'idle');
  assert.strictEqual(m.lastReply, 'new');
  assert.strictEqual(m.lastActivity, 200);
});

test('mergeSessionRow: fresh truthy fields win; unknown client fields survive; no prev', () => {
  const m = mergeSessionRow({ id: 'a', clientOnly: 7 }, { id: 'a', working: true, activity: 'x' });
  assert.strictEqual(m.working, true);
  assert.strictEqual(m.activity, 'x');
  assert.strictEqual(m.clientOnly, 7);
  assert.strictEqual(mergeSessionRow(undefined, { id: 'b' }).working, false);
});
