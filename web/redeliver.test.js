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
  assert.match(sendStatusLabel(conv[0]), /lost.*unconfirmed/);
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
  assert.equal(conv[0].reason, 'Seen in the target transcript');
  assert.equal(conv[0].redeliveryReason, '');
  assert.equal(conv[0].statusTs, 1010);
  assert.equal(canRetry(conv[0]), false);
});
