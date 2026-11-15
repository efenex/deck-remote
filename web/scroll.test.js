// Run: node --test web/
// The thread must keep a reader's place when it re-renders (phone 2026-11-15: scrolling back
// in time snapped to the newest message on every redraw).
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const DeckThread = require('./thread.js');

test('nearBottom / scrollAfterRender policy', () => {
  assert.equal(DeckThread.nearBottom(1000, 920, 80), true);   // at the tail
  assert.equal(DeckThread.nearBottom(1000, 700, 80), false);  // scrolled back
  assert.equal(DeckThread.nearBottom(0, 0, 0), true);         // empty list
  const up = { top: 300, wasNear: false }, tail = { top: 920, wasNear: true };
  assert.equal(DeckThread.scrollAfterRender(up, {}, 2000), 300);
  assert.equal(DeckThread.scrollAfterRender(up, { stick: true }, 2000), 2000);
  assert.equal(DeckThread.scrollAfterRender(tail, {}, 2000), 2000);
  assert.equal(DeckThread.scrollAfterRender(up, { keepScroll: true }, 2000), null);
  assert.equal(DeckThread.scrollAfterRender(up, {}, 100), 100); // list shrank: clamp
});

// Run the real renderConvo against a fake scroller that clamps like a browser.
const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const body = source.slice(source.indexOf('  function renderConvo('), source.indexOf('  // Escape-first markdown'));
const ROW = 50, VIEW = 400;
function setup(rows) {
  const convo = {
    children: [], _top: 0, clientHeight: VIEW,
    get scrollHeight() { return this.children.length * ROW; },
    get scrollTop() { return this._top; },
    set scrollTop(v) { this._top = Math.max(0, Math.min(v, Math.max(0, this.scrollHeight - VIEW))); },
    set innerHTML(_) { this.children = []; this._top = 0; }, // a rebuild loses the offset
    appendChild(c) { this.children.push(c); return c; },
  };
  const conv = Array.from({ length: rows }, (_, i) => ({ role: 'note', content: 'n' + i }));
  const pill = { hidden: false, dataset: {} };
  const ctx = {
    state: { openSheetId: 's', history: new Map(), pending: new Map(), sessions: new Map() },
    $: (sel) => (sel === '#convo' ? convo : sel === '#newPill' ? pill : null),
    ensureConvo: () => conv,
    el: () => ({ innerHTML: '', appendChild() {}, classList: { add() {} } }),
    esc: (x) => x, DeckThread, hideNewPill: () => { pill.hidden = true; },
  };
  vm.runInNewContext(body + '\nthis.renderConvo = renderConvo;', ctx);
  return { convo, conv, pill, render: (o) => ctx.renderConvo('s', o) };
}

test('renderConvo keeps the offset of a reader scrolled back, unchanged content', () => {
  const t = setup(40);
  t.render(); // initial open: empty list -> sticks to the tail
  assert.equal(t.convo.scrollTop, 40 * ROW - VIEW);
  t.convo.scrollTop = 500;
  t.render(); t.render(); // e.g. poll/SSE/pending redraws
  assert.equal(t.convo.scrollTop, 500);
});

test('renderConvo keeps the offset when new messages arrive below the fold', () => {
  const t = setup(40);
  t.render();
  t.convo.scrollTop = 500;
  t.conv.push({ role: 'note', content: 'new' });
  t.render();
  assert.equal(t.convo.scrollTop, 500);
});

test('renderConvo follows new content when already at the tail', () => {
  const t = setup(40);
  t.render();
  t.convo.scrollTop = t.convo.scrollHeight - VIEW - 30; // within the 80px threshold
  t.conv.push({ role: 'note', content: 'new' });
  t.render();
  assert.equal(t.convo.scrollTop, t.convo.scrollHeight - VIEW);
  assert.equal(t.pill.hidden, true);
});

test('renderConvo { stick } jumps to the tail even when scrolled back (own send)', () => {
  const t = setup(40);
  t.render();
  t.convo.scrollTop = 500;
  t.render({ stick: true });
  assert.equal(t.convo.scrollTop, t.convo.scrollHeight - VIEW);
});

test('renderConvo { keepScroll } leaves the offset to the caller', () => {
  const t = setup(40);
  t.render();
  t.convo.scrollTop = 500;
  t.render({ keepScroll: true });
  assert.equal(t.convo.scrollTop, 0); // rebuilt; caller (loadOlderHistory/reconcile) restores
});
