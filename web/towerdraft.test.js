// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const D = require('./towerdraft.js');

const mem = () => { const m = new Map(); return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), raw: m }; };
const ask = (id) => ({ kind: 'ask', id, key: 'ask:' + id });
const model = (...ids) => ({ sections: [{ id: 'waiting', items: ids.map(ask) }] });

// A tiny stand-in for the list DOM: render() makes fresh textarea objects like renderTower does.
function fakeList() {
  const list = { boxes: [], active: null };
  list.render = (store, ids) => {
    list.boxes = ids.map((id) => {
      const ta = { tagName: 'TEXTAREA', dataset: { ask: id }, value: store.get(id), selectionStart: 0, selectionEnd: 0, selectionDirection: 'none',
        focus() { list.active = ta; }, setSelectionRange(s, e, d) { ta.selectionStart = s; ta.selectionEnd = e; ta.selectionDirection = d; } };
      return ta;
    });
  };
  list.querySelectorAll = () => list.boxes;
  list.type = (store, id, text) => { const ta = list.boxes.find((t) => t.dataset.ask === id); ta.value = text; ta.selectionStart = ta.selectionEnd = text.length; store.set(id, ta.value); return ta; };
  return list;
}

test('render -> type -> refresh with the same ask: the text is restored, focus and caret too', () => {
  const store = D.createStore(mem());
  const list = fakeList();
  list.render(store, ['A', 'B']);
  const ta = list.type(store, 'A', 'my half-typed reply'); ta.focus(); ta.selectionStart = 3; ta.selectionEnd = 8;
  const snap = D.capture(list.active);
  list.render(store, ['A', 'B']); // overview refresh: fresh textareas
  assert.notStrictEqual(list.boxes[0], ta);
  assert.strictEqual(list.boxes[0].value, 'my half-typed reply');
  assert.strictEqual(list.boxes[1].value, '');
  assert.strictEqual(D.restore(list, snap), true);
  assert.strictEqual(list.active, list.boxes[0]);
  assert.deepStrictEqual([list.boxes[0].selectionStart, list.boxes[0].selectionEnd], [3, 8]);
});

test('render -> type -> ask removed: the draft is not dropped, it is an orphan until discarded or sent', () => {
  const store = D.createStore(mem());
  store.set('A', 'unsent answer'); store.set('B', 'other');
  assert.deepStrictEqual(D.orphans(store, D.presentAskIds(model('A', 'B'), null)), []);
  assert.deepStrictEqual(D.orphans(store, D.presentAskIds(model('B'), null)), [{ id: 'A', text: 'unsent answer' }]);
  assert.strictEqual(store.get('A'), 'unsent answer'); // still there for the Copy button
  store.clear('A');
  assert.deepStrictEqual(D.orphans(store, D.presentAskIds(model('B'), null)), []);
});

test('a folded ask (no reply box) and the legacy overview lists both count as present, not closed', () => {
  const store = D.createStore(mem());
  store.set('F', 'draft on a folded ask'); store.set('L', 'draft on a legacy ask'); store.set('LF', 'legacy folded');
  const present = D.presentAskIds(model('F'), { waiting: [{ id: 'L' }], waiting_folded: [{ id: 'LF' }] });
  assert.deepStrictEqual(D.orphans(store, present), []);
});

test('sent OK clears the draft; a failed send keeps it (store.clear only on success)', () => {
  const store = D.createStore(mem());
  store.set('A', 'x');
  // failure path: nothing calls clear
  assert.strictEqual(store.get('A'), 'x');
  store.clear('A');
  assert.strictEqual(store.get('A'), '');
  assert.deepStrictEqual(store.ids(), []);
});

test('typing then emptying the box removes the draft (whitespace-only is no draft)', () => {
  const m = mem();
  const store = D.createStore(m);
  store.set('A', 'abc'); store.set('A', '   ');
  assert.deepStrictEqual(store.ids(), []);
  assert.strictEqual(m.getItem(D.KEY), null);
});

test('drafts persist across a reload (new store over the same storage)', () => {
  const m = mem();
  D.createStore(m).set('A', 'survives reload');
  assert.strictEqual(D.createStore(m).get('A'), 'survives reload');
});

test('old drafts expire, the cap drops the oldest, corrupt or throwing storage is tolerated', () => {
  const m = mem(); let t = 1e12;
  const s1 = D.createStore(m, { now: () => t });
  s1.set('old', 'x'); t += D.TTL_MS + 1; s1.set('new', 'y');
  assert.deepStrictEqual(D.createStore(m, { now: () => t }).ids(), ['new']);
  const s2 = D.createStore(mem(), { now: () => t });
  for (let i = 0; i < D.MAX + 5; i++) { t += 1; s2.set('k' + i, 'v'); }
  assert.strictEqual(s2.ids().length, D.MAX);
  assert.ok(!s2.ids().includes('k0'));
  m.setItem(D.KEY, '{not json');
  assert.deepStrictEqual(D.createStore(m).ids(), []);
  const bad = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('quota'); }, removeItem() { throw new Error('denied'); } };
  const s3 = D.createStore(bad); s3.set('A', 'memory only');
  assert.strictEqual(s3.get('A'), 'memory only');
  assert.strictEqual(D.createStore(null).get('A'), '');
});

test('shouldDefer: only a focused reply box that holds text blocks a rebuild', () => {
  const box = (v) => ({ tagName: 'TEXTAREA', dataset: { ask: 'A' }, value: v });
  assert.strictEqual(D.shouldDefer(box('typed')), true);
  assert.strictEqual(D.shouldDefer(box('')), false);
  assert.strictEqual(D.shouldDefer(box('  \n')), false);
  assert.strictEqual(D.shouldDefer({ tagName: 'TEXTAREA', dataset: {}, value: 'composer elsewhere' }), false);
  assert.strictEqual(D.shouldDefer({ tagName: 'BUTTON', dataset: { ask: 'A' }, value: 'x' }), false);
  assert.strictEqual(D.shouldDefer(null), false);
});

test('capture ignores other elements; restore tolerates a vanished box', () => {
  assert.strictEqual(D.capture({ tagName: 'INPUT', dataset: {} }), null);
  assert.strictEqual(D.capture(null), null);
  assert.strictEqual(D.restore({ querySelectorAll: () => [] }, { id: 'gone' }), false);
  assert.strictEqual(D.restore(null, { id: 'A' }), false);
});

test('towerdraft.js is loaded before app.js and cached by the service worker', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const sw = fs.readFileSync(path.join(__dirname, 'sw.js'), 'utf8');
  assert.match(html, /<script src="\/towerdraft\.js\?v=\d+"><\/script>\s*<script src="\/app\.js/);
  assert.match(sw, /'\/towerdraft\.js\?v=\d+'/);
});
