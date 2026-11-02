// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { keyboardUp, sheetTop } = require('./viewport.js');

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

test('sheetTop: leaves an already-correct top alone, corrects a sheet painted too high or too low', () => {
  assert.strictEqual(sheetTop(344, 344, 344), 344);
  assert.strictEqual(sheetTop(344, 344.3, 344), 344); // sub-pixel noise
  // painted 58px above where the visible area starts (phone 2026-11-02): move it down 58
  assert.strictEqual(sheetTop(0, -58, 0), 58);
  assert.strictEqual(sheetTop(50, -8, 50), 108);
  // painted 20px too low: move it up
  assert.strictEqual(sheetTop(50, 70, 50), 30);
  assert.strictEqual(sheetTop(undefined, 0, 0), 0);
});
