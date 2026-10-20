// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const T = require('./theme.js');

function mem(init) {
  const m = new Map(Object.entries(init || {}));
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
    has: (k) => m.has(k),
  };
}

function fakeEl(initial) {
  const a = new Map(Object.entries(initial || {}));
  return {
    setAttribute: (k, v) => a.set(k, v),
    removeAttribute: (k) => a.delete(k),
    snapshot: () => Object.fromEntries(a),
  };
}

test('nothing stored: the default leaves <html> exactly as shipped', () => {
  const p = T.load(mem());
  assert.deepStrictEqual(p, T.DEFAULTS);
  assert.ok(T.isDefault(p));
  const el = fakeEl({ lang: 'en', 'data-theme': 'dark' });
  T.apply(el, p);
  assert.deepStrictEqual(el.snapshot(), { lang: 'en', 'data-theme': 'dark' });
});

test('save then load round-trips every preset and a custom mix', () => {
  for (const name of Object.keys(T.PRESETS)) {
    const s = mem();
    T.save(s, T.PRESETS[name]);
    assert.strictEqual(T.presetOf(T.load(s)), name);
  }
  const s = mem();
  const mix = { theme: 'light', density: 'compact', shape: 'round', turns: 'rail' };
  T.save(s, mix);
  assert.deepStrictEqual(T.load(s), mix);
  assert.strictEqual(T.presetOf(mix), '');
});

test('saving the default removes the key instead of storing it', () => {
  const s = mem();
  T.save(s, T.PRESETS.opencode);
  assert.ok(s.has(T.KEY));
  T.save(s, T.PRESETS.classic);
  assert.ok(!s.has(T.KEY));
});

test('garbage in storage falls back to defaults, per axis', () => {
  assert.deepStrictEqual(T.load(mem({ [T.KEY]: '{not json' })), T.DEFAULTS);
  assert.deepStrictEqual(T.load(mem({ [T.KEY]: '"opencode"' })), T.DEFAULTS);
  const p = T.load(mem({ [T.KEY]: JSON.stringify({ theme: 'neon', density: 'compact', x: 1 }) }));
  assert.deepStrictEqual(p, { ...T.DEFAULTS, density: 'compact' });
});

test('apply sets only non-default axes and clears them again', () => {
  const el = fakeEl({ 'data-theme': 'dark' });
  T.apply(el, T.PRESETS.opencode);
  assert.deepStrictEqual(el.snapshot(), {
    'data-theme': 'opencode', 'data-density': 'compact', 'data-shape': 'flat', 'data-turns': 'block',
  });
  T.apply(el, T.PRESETS.paper);
  assert.deepStrictEqual(el.snapshot(), { 'data-theme': 'light', 'data-turns': 'block' });
  T.apply(el, T.DEFAULTS);
  assert.deepStrictEqual(el.snapshot(), { 'data-theme': 'dark' });
});

test('?preview= picks a preset, off resets, anything else is ignored', () => {
  assert.deepStrictEqual(T.fromQuery('?token=x&preview=Opencode'), T.normalize(T.PRESETS.opencode));
  assert.deepStrictEqual(T.fromQuery('?preview=off'), T.DEFAULTS);
  assert.deepStrictEqual(T.fromQuery('?preview=classic'), T.DEFAULTS);
  assert.strictEqual(T.fromQuery('?preview=neon'), null);
  assert.strictEqual(T.fromQuery('?token=x'), null);
  assert.strictEqual(T.fromQuery(''), null);
});
