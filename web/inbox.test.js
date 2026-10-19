// Run: node --test web/
// Drives the real app.js against a minimal stub DOM to exercise the inbox
// "Mark all read" cursor (localStorage; there is no server-side read state).
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

function makeEnv(notifs, seenAt, routes) {
  const store = new Map([['deck_remote_token', 't']]);
  if (seenAt != null) store.set('dr-inbox-seen-at', String(seenAt));
  const listeners = new Map(); // selector -> {event -> fn}
  const els = new Map();
  const mkEl = (sel) => {
    const e = {
      sel, hidden: false, textContent: '', className: '', dataset: {}, children: [], style: {}, _ls: {}, _html: '',
      get innerHTML() { return this._html; }, set innerHTML(v) { this._html = v; this.children = []; },
      classList: { toggle() {}, add() {}, remove() {}, contains() { return false; } },
      addEventListener(ev, fn) { this._ls[ev] = fn; if (!listeners.has(sel)) listeners.set(sel, {}); listeners.get(sel)[ev] = fn; },
      removeEventListener() {}, append(...c) { this.children.push(...c); }, appendChild(c) { this.children.push(c); return c; },
      querySelector: () => mkEl('q'), querySelectorAll: () => [], remove() {}, focus() {}, scrollIntoView() {},
      closest: () => null, setAttribute() {}, getAttribute: () => null,
    };
    return e;
  };
  const get = (sel) => { if (!els.has(sel)) els.set(sel, mkEl(sel)); return els.get(sel); };
  const document = {
    querySelector: get, querySelectorAll: () => [], createElement: () => mkEl('new'), createTextNode: (t) => ({ textContent: String(t), children: [] }), addEventListener() {},
    body: mkEl('body'), documentElement: mkEl('html'), visibilityState: 'visible', getElementById: get,
  };
  const win = { addEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }) };
  const fetch = async (url, opts) => {
    const u = String(url);
    let body = u.includes('/api/rc/notifications') ? { notifications: notifs } : {};
    if (routes) { const r = routes(u, opts || {}); if (r !== undefined) body = r; }
    return { ok: true, status: 200, text: async () => JSON.stringify(body), json: async () => body, body: null };
  };
  const ctx = {
    document, window: win, localStorage: { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k) },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    navigator: { serviceWorker: { addEventListener() {}, register: async () => ({}), ready: new Promise(() => {}) }, clipboard: {} }, location: { href: 'http://x/', origin: 'http://x', search: '', hash: '', pathname: '/' },
    history: { replaceState() {} }, fetch, console, setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
    URL, URLSearchParams, EventSource: function () { this.close = () => {}; }, WebSocket: function () { this.close = () => {}; },
    DeckGestures: { wireSwipeDismiss: () => () => {} }, DeckMarkdown: { render: (s) => String(s) }, DeckThread: {}, DeckVersion: require('./version.js'), Notification: { permission: 'default' },
    confirm: () => true, requestAnimationFrame: (f) => f(), AbortController, Date, Promise, JSON, Math, Set, Map, Array, Object, String, Number, Error,
  };
  ctx.window = Object.assign(win, ctx);
  ctx.self = ctx.globalThis = ctx.window;
  return { ctx, store, listeners, els, mkEl };
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

function walk(n, fn) { fn(n); (n.children || []).forEach((c) => c && typeof c === 'object' && walk(c, fn)); }

test('Tower tab: Queue with tower POSTs op=pick for the suggestion after a confirm; declining sends nothing', async () => {
  const model = {
    header: 'TOWER 17:35Z', generated_at: new Date().toISOString(), palette: { sec: { fg: '#87d7ff', bold: true } }, legend: [],
    sections: [
      { id: 'waiting', title: 'WAITING ON YOU', count: '1', urgent: 0, folded: null, items: [
        { kind: 'ask', id: 'hh-flux-pr', key: 'ask:hh-flux-pr', status: 'waiting', glyph: '◐', style: 'st_wait', fold_ok: true, confirm_ok: true, folded: { since: '2026-10-19T16:45:47Z' }, summary: 'merge the flux fix', blocks: [], bangs: [], head: 'x' }] },
      { id: 'suggested', title: 'SUGGESTED', count: '1', urgent: 0, folded: null, items: [
        { kind: 'suggest', id: 'sg1', key: 'suggest:sg1', status: 'suggest', glyph: '✦', style: 'st_suggest', title: 'Do the thing', prompt: 'p', why_blocks: [{ k: 'p', t: 'because' }], action_blocks: [], bangs: [], head: 'sg1' }] },
    ],
  };
  const posts = [];
  const env = makeEnv([], null, (u, opts) => {
    if (u.startsWith('/api/rc/tower/whoami')) return { actions_enabled: true, act: true };
    if (u.startsWith('/api/rc/tower/model')) return { enabled: true, actions: true, model };
    if (u.startsWith('/api/rc/tower/act')) { posts.push(JSON.parse(opts.body)); return { ok: true, msg: 'queued → tower' }; }
    if (u.startsWith('/api/rc/tower')) return { enabled: true, mtime: 'x', overview: { generated_at: model.generated_at, counts: { waiting: 1 } } };
    return undefined;
  });
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  await new Promise((r) => setTimeout(r, 100));
  const list = env.els.get('#towerList');
  const btns = []; const heads = [];
  walk(list, (n) => { if (/\btbtn\b/.test(n.className)) btns.push(n); if (/tsechead/.test(n.className)) heads.push(n); });
  assert.strictEqual(heads.length, 2, 'both sections rendered');
  const queue = btns.find((b) => b.textContent === 'Queue with tower');
  assert.ok(queue, 'a Queue button exists for the suggestion');
  assert.ok(btns.some((b) => b.textContent === 'Dismiss'));
  let asked = '';
  env.ctx.confirm = (m) => { asked = m; return true; };
  queue._ls.click({ stopPropagation() {} });
  await new Promise((r) => setTimeout(r, 50));
  assert.match(asked, /Do the thing/);
  assert.deepStrictEqual(posts, [{ op: 'pick', id: 'sg1' }]);
  // declining the confirm sends nothing
  posts.length = 0;
  env.ctx.confirm = () => false;
  const dismiss = (() => { const b = []; walk(list, (n) => { if (n.textContent === 'Dismiss') b.push(n); }); return b[0]; })();
  dismiss._ls.click({ stopPropagation() {} });
  await new Promise((r) => setTimeout(r, 20));
  assert.deepStrictEqual(posts, []);
});

test('Version banner: shown on server/web mismatch, dismissal sticks until the situation changes', async () => {
  let v = { server_sha: 'aaa1111', web_sha: 'bbb2222', built_at: new Date(Date.now() - 3600e3).toISOString(), web_dirty: false };
  const env = makeEnv([], null, (u) => (u.startsWith('/api/rc/version') ? v : undefined));
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  await new Promise((r) => setTimeout(r, 80));
  const box = env.els.get('#versionBanner');
  assert.strictEqual(box.hidden, false);
  assert.match(env.els.get('#versionText').textContent, /server aaa1111 \u2260 web bbb2222: rebuild \+ restart needed \(server built 1\.0 h ago\)/);
  env.listeners.get('#versionClose').click();
  assert.strictEqual(box.hidden, true);
  assert.ok(env.store.get('dr-version-dismissed'), 'dismissal is remembered');
});

test('Version banner: no banner when the shas match', async () => {
  const env = makeEnv([], null, (u) => (u.startsWith('/api/rc/version') ? { server_sha: 'aaa1111', web_sha: 'aaa1111', web_dirty: false } : undefined));
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  await new Promise((r) => setTimeout(r, 80));
  assert.strictEqual(env.els.get('#versionBanner').hidden, true);
});
