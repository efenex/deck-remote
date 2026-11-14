const test = require('node:test');
const assert = require('node:assert/strict');
const { applyAskState, canRetry, sendStatusLabel, promoteDelivered } = require('./thread.js');

test('delivery reason, timestamp and original ID survive state updates', () => {
  const conv = [{ role: 'me', content: 'hello', requestId: 'r', status: 'sent', queued: true }];
  applyAskState(conv, { requestId: 'r', sendId: 's', state: 'queued', reason: 'target busy', statusTs: 100, targetStatus: 'running', attempts: 1 });
  applyAskState(conv, { requestId: 'r', sendId: 's', state: 'queued', reason: 'composer occupied', statusTs: 101, attempts: 2 });
  assert.equal(conv[0].reason, 'composer occupied');
  assert.equal(conv[0].statusTs, 101);
  assert.equal(conv[0].sendId, 's');
  assert.equal(conv[0].targetStatus, 'running');
  assert.equal(conv[0].attempts, 2);
  assert.equal(canRetry(conv[0]), true);
  applyAskState(conv, { requestId: 'r', state: 'sent' });
  assert.equal(conv[0].status, 'queued', 'late acceptance must not erase a queue receipt');
  applyAskState(conv, { requestId: 'r', sendId: 's', state: 'queued', reason: 'old reason', statusTs: 100 });
  assert.equal(conv[0].reason, 'composer occupied');
});

test('lost is uncertain and offers only a same-ID guarded check', () => {
  const conv = [{ role: 'me', requestId: 'r', status: 'sent' }];
  applyAskState(conv, { requestId: 'r', sendId: 's', state: 'lost', reason: 'no landed trace after 2 minutes', statusTs: 200 });
  assert.match(sendStatusLabel(conv[0]), /may have landed; not re-sent/);
  assert.equal(conv[0].queued, false);
  assert.equal(canRetry(conv[0]), true);
  conv[0].redelivering = true;
  assert.equal(canRetry(conv[0]), false, 'double taps cannot launch a parallel request');
  conv[0].redelivering = false;
  applyAskState(conv, { requestId: 'r', sendId: 's', state: 'lost', statusTs: 200, redeliveryReason: 'Re-delivery blocked: may already have reached target' });
  assert.match(conv[0].redeliveryReason, /blocked/);
  applyAskState(conv, { requestId: 'r', sendId: 's', state: 'delivered', statusTs: 190, landedRowId: 'row' });
  assert.equal(conv[0].status, 'delivered', 'late-discovered transcript proof wins over later uncertainty');
  assert.equal(canRetry(conv[0]), false);
  applyAskState(conv, { requestId: 'r', sendId: 's', state: 'lost', statusTs: 300 });
  assert.equal(conv[0].status, 'delivered');
});

test('no receipt means no retry; attachments are never converted to a new text send', () => {
  assert.equal(canRetry({ status: 'failed', retryable: true, content: 'hello' }), false);
  assert.equal(canRetry({ status: 'failed', requestId: 'r', sendId: 's', images: 2 }), true, 'the action checks/releases the retained attachment queue ID');
});

test('transcript recovery clears stale diagnostics on a lost bubble', () => {
  const conv = [{ role: 'me', requestId: 'r', sendId: 's', content: 'hello', ts: 1000, status: 'lost', reason: 'timeout', redeliveryReason: 'blocked' }];
  assert.equal(promoteDelivered(conv, [{ role: 'me', content: 'hello', ts: 1010 }]), 1);
  assert.equal(conv[0].reason, 'Content seen; delivery attribution is unverified');
  assert.equal(conv[0].redeliveryReason, '');
  assert.equal(conv[0].statusTs, 1010);
  assert.equal(canRetry(conv[0]), true);
});

test('landed row ID outranks content, reserves proof first, and survives refresh', () => {
  const thread = require('./thread.js');
  const unproven = { role: 'me', content: '[REDACTED]', ts: 100, sendId: 's1', requestId: 'r1', status: 'lost' };
  const proven = { ...unproven, sendId: 's2', requestId: 'r2', landedRowId: 'right', status: 'delivered' };
  const wrong = { role: 'me', content: '[REDACTED]', ts: 101, id: 'wrong', hist: true };
  const right = { ...wrong, id: 'right', ts: 102 };
  assert.equal(promoteDelivered([proven], [wrong]), 0);
  const conv = [unproven, proven];
  assert.equal(promoteDelivered(conv, [right]), 1);
  assert.equal(unproven.status, 'lost', 'content cannot take a claimed row');
  assert.equal(proven.status, 'delivered');
  assert.equal(proven.ts, 102);
  const merged = thread.mergeFreshHistory([{ ...right }], conv);
  assert.equal(merged.find(e => e.id === 'right').sendId, 's2');
  assert.equal(merged.find(e => e.id === 'right').status, 'delivered');
  assert.ok(merged.some(e => e.sendId === 's1'));
});

test('content-only history merges keep the original receipt available for checking', () => {
  const thread = require('./thread.js');
  const e = { role: 'me', content: '[REDACTED]', ts: 100, sendId: 's1', requestId: 'r1', status: 'lost' };
  const merged = thread.mergeFreshHistory([{ role: 'me', content: '[REDACTED]', ts: 101, hist: true }], [e]);
  assert.equal(merged.length, 1);
  assert.equal(merged[0].status, 'seen');
  assert.equal(merged[0].reason, 'Content seen; delivery attribution is unverified');
  assert.equal(canRetry(merged[0]), true);
  assert.equal(thread.retryLabel(merged[0]), 'Check again');
  applyAskState(merged, { requestId: 'r1', sendId: 's1', state: 'delivered', landedRowId: 'proof' });
  assert.equal(sendStatusLabel(merged[0]), 'landed');
});

test('every R0 UI result is honest and a refusal allows a fresh tap', () => {
  const thread = require('./thread.js');
  for (const [status, label] of [['blocked_unknown', 'may have landed; not re-sent · check again'], ['unsupported', 'unverifiable'], ['delivered', 'landed'], ['pending', 'in progress'], ['refused', 're-delivery unavailable']]) {
    const e = { role: 'me', requestId: 'r', sendId: 's', status, queued: status === 'pending' };
    assert.equal(sendStatusLabel(e), label);
    assert.equal(canRetry(e), status !== 'delivered');
  }
  assert.equal(thread.retryLabel({status:'lost'}), 'Check again');
});

function memoryStorage() {
  const values = new Map();
  return { get length() { return values.size; }, key: i => [...values.keys()][i], getItem: k => values.get(k) || null,
    setItem: (k, v) => values.set(k, v), removeItem: k => values.delete(k) };
}

test('tap key is durable before sending, retried after reload, then replaced after refusal', () => {
  const thread = require('./thread.js'), storage = memoryStorage();
  const e = { requestId:'r', sendId:'s', ts:100 };
  assert.equal(thread.beginRedelivery(storage, 'phone', 'session', e, () => 'K1'), 'K1');
  const restored = thread.pendingRedeliveries(storage, 'phone', 'session');
  assert.equal(restored.length, 1);
  assert.equal(thread.beginRedelivery(storage, 'phone', 'session', restored[0], () => 'K2'), 'K1');
  assert.equal(thread.pendingRedeliveries(storage, 'other', 'session').length, 0);
  thread.finishRedelivery(storage, 'phone', 'session', e, 'stale');
  assert.equal(storage.length, 1);
  thread.finishRedelivery(storage, 'phone', 'session', e, 'K1');
  assert.equal(storage.length, 0);
  assert.equal(thread.beginRedelivery(storage, 'phone', 'session', e, () => 'K2'), 'K2');
  assert.throws(() => thread.beginRedelivery({ getItem: () => null, setItem: () => { throw Error('quota'); } }, 'phone', 'session', e, () => 'K3'), /quota/);
});

test('later landed receipt adds proof identity and exact identity beats timestamp/content fences', () => {
  const e = { role:'me', requestId:'r', sendId:'s', content:'[REDACTED]', ts:1000, status:'delivered' };
  applyAskState([e], { requestId:'r', sendId:'s', state:'delivered', landedRowId:'proof' });
  assert.equal(e.landedRowId, 'proof');
  assert.equal(promoteDelivered([e], [{ role:'me', id:'proof', content:'[REDACTED shape differs]', ts:100 }]), 1);
  assert.equal(e.ts,100);
});

test('operation timestamp does not hide a current failed snapshot after a sticky pending result', () => {
  const e = { role:'me', requestId:'r', sendId:'s', status:'failed', statusTs:100 };
  applyAskState([e], { requestId:'r', sendId:'s', operationId:'K', state:'pending', statusTs:200 });
  assert.equal(e.status,'pending');
  applyAskState([e], { requestId:'r', sendId:'s', state:'failed', statusTs:100 });
  assert.equal(e.status,'failed');
  applyAskState([e], { requestId:'r', sendId:'s', state:'queued', statusTs:99 });
  assert.equal(e.status,'failed');
});

test('late proof corrects a content-only row and survives a far-later history page', () => {
  const thread = require('./thread.js');
  const e = { role:'me', requestId:'r', sendId:'s', content:'[REDACTED]', ts:100, status:'lost' };
  promoteDelivered([e], [{role:'me', content:'[REDACTED]', ts:101, id:'content-only'}]);
  assert.equal(e.status,'seen');
  applyAskState([e], {requestId:'r', sendId:'s', state:'delivered', landedRowId:'proof'});
  const proof = {role:'me', content:'[REDACTED]', ts:10000, id:'proof', hist:true};
  const merged = thread.mergeFreshHistory([{...proof}], [e]);
  assert.equal(merged[0].sendId,'s');
  assert.equal(merged[0].status,'delivered');
  assert.equal(promoteDelivered([e], [proof]),1);
  assert.equal(e.id,'proof');
  assert.equal(e.ts,10000);
  assert.equal(promoteDelivered([e], [proof]),0);
});

// agent-deck and deck-remote share native row IDs (Claude line uuid), but each
// synthesizes its own for queue-absorbed Claude prompts (enqueue vs absorb
// time) and id-less Codex user rows. A proven bubble must still reconcile with
// its transcript row by content there, or it shows twice.
test('a landed row ID in a synthesized scheme falls back to content and does not duplicate', () => {
  const thread = require('./thread.js');
  for (const [landed, rowId] of [['queue:2026-11-14T08:00:00.000Z:aaaa', 'queue:2026-11-14T08:00:05Z:bbbbbbbbbbbb'], ['codex:2026-11-14T08:00:00Z:cccc', undefined]]) {
    const proven = () => ({ role: 'me', content: '[REDACTED]', ts: 100, sendId: 's1', requestId: 'r1', landedRowId: landed, status: 'delivered' });
    const row = () => ({ role: 'me', content: '[REDACTED]', ts: 105, id: rowId, hist: true });
    const merged = thread.mergeFreshHistory([row()], [proven()]);
    assert.equal(merged.length, 1, landed);
    assert.equal(merged[0].status, 'delivered');
    assert.equal(merged[0].sendId, 's1');
    const conv = [proven()];
    assert.equal(promoteDelivered(conv, [row()]), 1, landed);
    assert.equal(conv[0].hist, true);
    assert.equal(conv[0].status, 'delivered');
  }
});

test('an old content-only (seen) bubble outside a fresh window is not re-appended last', () => {
  const thread = require('./thread.js');
  const seen = { role: 'me', content: '[REDACTED]', ts: 100, sendId: 's1', requestId: 'r1', status: 'seen', hist: true, id: 'old' };
  const merged = thread.mergeFreshHistory([{ role: 'reply', content: 'later', ts: 5000, hist: true }], [seen]);
  assert.deepEqual(merged.map(e => e.role), ['reply']);
});
