'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const N = require('./network.js');

test('deadline aborts both a hung connection and a stalled response body', async () => {
  for (const stalledBody of [false, true]) {
    let signal;
    const never = new Promise(() => {});
    const fetcher = async (_, opts) => { signal = opts.signal; return stalledBody ? { text: () => never } : never; };
    await assert.rejects(N.request(fetcher, '/fake', {}, r => r.text(), 15), /Host did not respond/);
    assert.equal(signal.aborted, true);
  }
});
test('completed requests retain results and rejection clears deadline', async () => {
  assert.equal(await N.request(async () => ({ text: async () => 'ok' }), '/', {}, r => r.text(), 20), 'ok');
  await assert.rejects(N.request(async () => { throw new Error('network down'); }, '/', {}, r => r, 20), /network down/);
});
test('reconnect feedback includes count, countdown, error, contact and sleep hint after three failures', () => {
  const state = { attempt: 4, failures: 3, nextRetry: 6000, lastContact: 1000, error: 'lost' };
  const text = N.feedback(state, 3100);
  for (const part of ['attempt 4', 'next retry in 3 s', 'last error: lost', '1970-01-01T00:00:01.000Z', 'cached data', 'Host may be asleep']) assert.ok(text.includes(part), text);
  assert.equal(N.retryDelay(50), 30000);
  assert.ok(!N.feedback({ attempt: 1, failures: 1 }, 0).includes('asleep'));
});
