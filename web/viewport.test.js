// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { keyboardUp } = require('./viewport.js');

test('iOS home-screen app, keyboard up (device overlay 13:36:37Z): innerH shrank to 447 but clientH is 793', () => {
  const vv = { height: 449, offsetTop: 344, scale: 1 };
  assert.strictEqual(keyboardUp(vv, 447, 793), true);
  // the old test (vv < innerHeight - 120) said no, leaving the 852 root pinned over the keyboard
  assert.strictEqual(vv.height < 447 - 120, false);
});

test('keyboard down: visual viewport equals the layout viewport', () => {
  assert.strictEqual(keyboardUp({ height: 793, scale: 1 }, 793, 793), false);
  assert.strictEqual(keyboardUp({ height: 852, scale: 1 }, 852, 852), false);
});

test('Safari-style keyboard: innerHeight stays tall, visual viewport shrinks', () => {
  assert.strictEqual(keyboardUp({ height: 500, scale: 1 }, 793, 793), true);
});

test('pinch zoom is not a keyboard; small dips (URL bar) are not either; no visualViewport', () => {
  assert.strictEqual(keyboardUp({ height: 300, scale: 2.5 }, 793, 793), false);
  assert.strictEqual(keyboardUp({ height: 700, scale: 1 }, 793, 793), false);
  assert.strictEqual(keyboardUp(null, 793, 793), false);
});
