// Run: node --test web/
// Drives the real app.js against a minimal stub DOM to exercise the inbox
// "Mark all read" cursor (localStorage; there is no server-side read state).
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

function makeEnv(notifs, seenAt) {
  const store = new Map([['deck_remote_token', 't']]);
  if (seenAt != null) store.set('dr-inbox-seen-at', String(seenAt));
  const listeners = new Map(); // selector -> {event -> fn}
  const els = new Map();
  const mkEl = (sel) => {
    const e = {
      sel, hidden: false, textContent: '', innerHTML: '', className: '', dataset: {}, children: [], style: {},
      classList: { toggle() {}, add() {}, remove() {}, contains() { return false; } },
      addEventListener(ev, fn) { if (!listeners.has(sel)) listeners.set(sel, {}); listeners.get(sel)[ev] = fn; },
      removeEventListener() {}, append(...c) { this.children.push(...c); }, appendChild(c) { this.children.push(c); return c; },
      querySelector: () => mkEl('q'), querySelectorAll: () => [], remove() {}, focus() {}, scrollIntoView() {},
      closest: () => null, setAttribute() {}, getAttribute: () => null,
    };
    return e;
  };
  const get = (sel) => { if (!els.has(sel)) els.set(sel, mkEl(sel)); return els.get(sel); };
  const document = {
    querySelector: get, querySelectorAll: () => [], createElement: () => mkEl('new'), addEventListener() {},
    body: mkEl('body'), documentElement: mkEl('html'), visibilityState: 'visible', getElementById: get,
  };
  const win = { addEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }) };
  const fetch = async (url) => {
    const u = String(url);
    const body = u.includes('/api/rc/notifications') ? { notifications: notifs } : {};
    return { ok: true, status: 200, text: async () => JSON.stringify(body), json: async () => body, body: null };
  };
  const ctx = {
    document, window: win, localStorage: { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k) },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    navigator: { serviceWorker: { addEventListener() {}, register: async () => ({}), ready: new Promise(() => {}) }, clipboard: {} }, location: { href: 'http://x/', origin: 'http://x', search: '', hash: '', pathname: '/' },
    history: { replaceState() {} }, fetch, console, setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
    URL, URLSearchParams, EventSource: function () { this.close = () => {}; }, WebSocket: function () { this.close = () => {}; },
    DeckGestures: { wireSwipeDismiss: () => () => {} }, DeckMarkdown: { render: (s) => String(s) }, DeckThread: {}, Notification: { permission: 'default' },
    confirm: () => true, requestAnimationFrame: (f) => f(), AbortController, Date, Promise, JSON, Math, Set, Map, Array, Object, String, Number, Error,
  };
  ctx.window = Object.assign(win, ctx);
  ctx.self = ctx.globalThis = ctx.window;
  return { ctx, store, listeners, els };
}

test('Mark all read moves the cursor to the newest notification and clears the badge', async () => {
  const notifs = [{ id: 'c', at: 3000, kind: 'reply' }, { id: 'b', at: 2000, kind: 'approval' }, { id: 'a', at: 1000, kind: 'reply' }];
  const env = makeEnv(notifs, 1500);
  vm.createContext(env.ctx);
  const src = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  vm.runInContext(src, env.ctx, { filename: 'app.js' });
  await new Promise((r) => setTimeout(r, 50));
  const badge = env.els.get('#inboxBadge');
  assert.ok(badge, 'inbox badge element was queried');
  assert.strictEqual(badge.textContent, '2', 'two items are newer than the seen cursor');
  env.listeners.get('#inboxMarkRead').click();
  assert.strictEqual(env.store.get('dr-inbox-seen-at'), '3000');
  assert.strictEqual(badge.hidden, true);
});

test('Mark all read also covers entries whose `at` is later than the newest-arrived one (clock stepped back)', async () => {
  // newest-first by arrival, but the server clock went backwards: the oldest entry carries the largest `at`.
  const notifs = [{ id: 'c', at: 2000, kind: 'reply' }, { id: 'b', at: 5000, kind: 'reply' }, { id: 'a', at: 1000, kind: 'reply' }];
  const env = makeEnv(notifs, 500);
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  await new Promise((r) => setTimeout(r, 50));
  const badge = env.els.get('#inboxBadge');
  assert.strictEqual(badge.textContent, '3');
  env.listeners.get('#inboxMarkRead').click();
  assert.strictEqual(env.store.get('dr-inbox-seen-at'), '5000');
  assert.strictEqual(badge.hidden, true, 'no entry may stay unread after Mark all read');
});
