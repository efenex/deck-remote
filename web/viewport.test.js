// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { keyboardUp, visibleTop, sheetTop } = require('./viewport.js');

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
  assert.strictEqual(sheetTop(344, 0, 0), 344);
  assert.strictEqual(sheetTop(344, 0.3, 0), 344); // sub-pixel noise
  // painted 58px above the visible top: move it down 58
  assert.strictEqual(sheetTop(0, -58, 0), 58);
  assert.strictEqual(sheetTop(50, -8, 0), 58);
  // painted 20px too low: move it up (never above 0)
  assert.strictEqual(sheetTop(50, 20, 0), 30);
  assert.strictEqual(sheetTop(10, 20, 0), 0);
  assert.strictEqual(sheetTop(undefined, 0, 0), 0);
});

test('visibleTop: visual viewport in document coordinates mapped through where the document is painted', () => {
  // plain Safari pan, document not shifted
  assert.strictEqual(visibleTop({ offsetTop: 120, pageTop: 120 }, 0), 120);
  // no pageTop: fall back to offsetTop
  assert.strictEqual(visibleTop({ offsetTop: 120 }, 0), 120);
  assert.strictEqual(visibleTop(null, -5), 0);
});

test('REGRESSION 2026-11-02 23:02Z (iPhone PWA overlay): doubled offsetTop painted the sheet 344pt low', () => {
  // overlay: vv 449 off 344, html/body/#app top -344 bottom 449, #sheet style top 688 -> rect top 344.
  const vv = { height: 449, offsetTop: 344, pageTop: 344, scale: 1 };
  const vis = visibleTop(vv, -344);
  assert.strictEqual(vis, 0);
  // inline top 344 paints at rect 0 (688 painted at 344 = fixed anchored to the shifted document)
  const paintedAt = (top) => top - 344;
  assert.strictEqual(sheetTop(344, paintedAt(344), vis), 344); // already right: untouched
  // round 1 started from 344, compared rect 0 with offsetTop 344 and wrote 688
  assert.strictEqual(sheetTop(688, paintedAt(688), vis), 344); // the observed state recovers
});
