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
  const docLs = {};
  const mkEl = (sel) => {
    const e = {
      sel, hidden: false, value: '', textContent: '', className: '', dataset: {}, children: [], style: {}, _ls: {}, _html: '',
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
    querySelector: get, querySelectorAll: () => [], createElement: () => mkEl('new'), createTextNode: (t) => ({ textContent: String(t), children: [] }), addEventListener(ev, fn) { (docLs[ev] = docLs[ev] || []).push(fn); },
    body: mkEl('body'), documentElement: mkEl('html'), visibilityState: 'visible', getElementById: get,
  };
  const win = { addEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }) };
  const fetch = async (url, opts) => {
    const u = String(url);
    let body = u.includes('/api/rc/notifications') ? { notifications: notifs } : {};
    if (routes) { const r = await routes(u, opts || {}); if (r !== undefined) body = r; }
    return { ok: true, status: 200, text: async () => JSON.stringify(body), json: async () => body, body: null };
  };
  const ctx = {
    document, window: win, localStorage: { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k) },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    navigator: { serviceWorker: { addEventListener() {}, register: async () => ({}), ready: new Promise(() => {}) }, clipboard: {} }, location: { href: 'http://x/', origin: 'http://x', search: '', hash: '', pathname: '/' },
    history: { replaceState() {} }, fetch, console, setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
    URL, URLSearchParams, EventSource: function () { this.readyState = 1; this.close = () => { this.readyState = 2; }; Promise.resolve().then(() => { if (this.readyState === 1 && this.onopen) this.onopen(); }); }, WebSocket: function () { this.close = () => {}; },
    DeckNetwork: require('./network.js'), DeckGestures: { wireSwipeDismiss: () => () => {} }, DeckMarkdown: Object.assign({}, require('./markdown.js'), { render: (s) => String(s) }), DeckThread: {}, DeckPulse: require('./pulse.js'), DeckPair: require('./pair.js'), DeckVersion: require('./version.js'), DeckDrafts: require('./towerdraft.js'), Notification: { permission: 'default' },
    confirm: () => true, requestAnimationFrame: (f) => f(), AbortController, Date, Promise, JSON, Math, Set, Map, Array, Object, String, Number, Error,
  };
  ctx.window = Object.assign(win, ctx);
  ctx.self = ctx.globalThis = ctx.window;
  return { ctx, store, listeners, els, mkEl, docLs };
}

test('Terminal fallback navigates without exposing the stored bearer', async () => {
  const env = makeEnv([], null);
  const token = 'terminal-fallback-fixture-secret';
  env.store.set('deck_remote_token', token);
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx);
  await new Promise((resolve) => setImmediate(resolve));
  let prevented = false;
  env.listeners.get('#escapeTerm').click({ preventDefault() { prevented = true; } });
  assert.strictEqual(prevented, true);
  assert.strictEqual(env.ctx.location.href, '/');
  assert.strictEqual(env.store.get('deck_remote_token'), token, 'authentication remains in localStorage');
});

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
  assert.match(env.els.get('#versionText').textContent, /server aaa1111 \u2260 web bbb2222: rebuild \+ restart needed \(server built 60 min ago\)/);
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

test('Tower badge paints from the cheap overview without waiting for the slow model', async () => {
  const gen = new Date().toISOString();
  const env = makeEnv([], null, async (u) => {
    if (u.startsWith('/api/rc/tower/whoami')) return { actions_enabled: true, act: true };
    if (u.startsWith('/api/rc/tower/model')) { await new Promise((r) => setTimeout(r, 600)); return { enabled: true, model: { header: 'H', generated_at: gen, sections: [], palette: {}, legend: [] } }; }
    if (u.startsWith('/api/rc/tower')) return { enabled: true, mtime: 'x', overview: { generated_at: gen, counts: { waiting: 3, urgent: 0 } } };
    return undefined;
  });
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  await new Promise((r) => setTimeout(r, 200));
  assert.strictEqual(env.els.get('#towerBadge').textContent, '3', 'badge is set while the model is still loading');
  assert.ok(env.els.get('#towerList').children.length > 0, 'the legacy list shows meanwhile');
});

test('Tapping a .doclink opens the viewer via /api/rc/doc (tower tab: no session; thread: the open session)', async () => {
  const calls = [];
  const env = makeEnv([], null, async (u) => {
    if (u.includes('/api/rc/doc')) { calls.push(u); return { path: '/r/docs/a.md', root: '/r', size: 5, mtime: '', content: '# hi\n' }; }
  });
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  await new Promise((r) => setTimeout(r, 50));
  let prevented = false;
  const tap = (anchorPath, line, inTower) => {
    const a = { dataset: { path: anchorPath, line }, closest: (sel) => (sel === 'a.doclink' ? a : (inTower && sel === '#screenTower' ? a : null)) };
    env.docLs.click.forEach((fn) => fn({ target: a, preventDefault() { prevented = true; } }));
  };
  tap('docs/a.md', '12', true);
  await new Promise((r) => setTimeout(r, 20));
  assert.ok(prevented);
  assert.strictEqual(calls.length, 1);
  assert.ok(calls[0].includes('path=docs%2Fa.md') && !calls[0].includes('session='), calls[0]);
  assert.strictEqual(env.els.get('#docView').hidden, false, 'overlay opened');
  assert.strictEqual(env.els.get('#docTitle').textContent, 'a.md');
});

test('A refused doc shows a Not readable toast and leaves the viewer closed', async () => {
  const env = makeEnv([], null, async () => undefined);
  const baseFetch = env.ctx.fetch;
  env.ctx.fetch = async (u, o) => (String(u).includes('/api/rc/doc')
    ? { ok: false, status: 403, text: async () => JSON.stringify({ error: 'path is outside the allowed roots' }) }
    : baseFetch(u, o));
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  await new Promise((r) => setTimeout(r, 50));
  const a = { dataset: { path: '/etc/x.md' }, closest: (sel) => (sel === 'a.doclink' ? a : null) };
  env.docLs.click.forEach((fn) => fn({ target: a, preventDefault() {} }));
  await new Promise((r) => setTimeout(r, 20));
  assert.ok(!env.els.has('#docTitle'), 'the viewer was never populated');
  const toastEl = env.ctx.document.body.children.find((c) => /Not readable/.test(c.innerHTML));
  assert.ok(toastEl && toastEl.innerHTML.includes('path is outside the allowed roots'), 'toast carries the server refusal');
});

test('all tab refresh buttons stop spinning and toast when the host stops responding', async () => {
  let offline = false;
  const env = makeEnv([], null, async (u) => {
    if (offline) return new Promise(() => {});
    if (u.includes('/tower/whoami')) return {};
    if (u.includes('/tower/model')) return { model: { generated_at: '2026-10-25T15:02:00Z', header: 'TOWER', sections: [] } };
    if (u.includes('/tower')) return { enabled: true, overview: { generated_at: '2026-10-25T15:02:00Z', counts: {} } };
  });
  env.ctx.setTimeout = (fn, delay) => delay >= 1000 ? 0 : setTimeout(fn, delay);
  env.ctx.EventSource = function () { this.readyState = 0; this.close = () => { this.readyState = 2; }; Promise.resolve().then(() => { if (!offline && this.readyState !== 2) { this.readyState = 1; this.onopen(); } }); };
  const N = require('./network.js');
  env.ctx.DeckNetwork = { ...N, request: (f, u, o, read) => N.request(f, u, o, read, 20) };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx);
  await new Promise(r => setTimeout(r, 50));
  offline = true;
  for (const [sel, label] of [['#goRefresh', 'Deck'], ['#inboxRefresh', 'Inbox'], ['#towerRefresh', 'Tower']]) {
    const pending = env.listeners.get(sel).click();
    assert.equal(env.els.get(sel).disabled, true);
    await pending;
    assert.equal(env.els.get(sel).disabled, false);
    assert.ok(env.ctx.document.body.children.some(c => c.innerHTML.includes(label + ' refresh failed') && c.innerHTML.includes('Host did not respond')));
  }
  assert.match(env.els.get('#towerAsOf').textContent, /2026-10-25T15:02:00Z.*OFFLINE \/ STALE/);
});

test('event stream retries have an owned deadline and growing, visible attempts', async () => {
  const env = makeEnv([]);
  const sockets = [], timers = new Map(); let seq = 0;
  env.ctx.EventSource = function () { this.close = () => {}; sockets.push(this); };
  env.ctx.setTimeout = (fn, delay) => { timers.set(++seq, { fn, delay }); return seq; };
  env.ctx.clearTimeout = (id) => timers.delete(id);
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx);
  await new Promise(r => setImmediate(r));
  for (let i = 1; i <= 3; i++) {
    sockets.at(-1).onerror();
    assert.match(env.els.get('#connText').textContent, new RegExp('attempt ' + i + '.*next retry in'));
    const retry = [...timers.entries()].find(([, t]) => t.delay === Math.min(30000, 1000 * 2 ** i));
    assert.ok(retry, 'retry scheduled');
    timers.delete(retry[0]); retry[1].fn();
  }
  assert.match(env.els.get('#connText').textContent, /Host may be asleep/);
  sockets.at(-1).onopen();
  assert.equal(env.els.get('#connStrip').hidden, true);
  assert.ok(![...timers.values()].some(t => t.delay === 12000), 'connection deadline cleared on recovery');
});

// A synced agent-deck view filter hides rows from /api/rc/sessions; the unfiltered
// list (?all=1) keeps inbox and toast lookups working for them.
function hiddenFilterEnv(hiddenNeedsAttention, onES) {
  const shown = { id: 'shown', title: 'Shown', status: 'idle' };
  const hidden = { id: 'hid', title: 'Hidden', status: 'stopped', needsAttention: hiddenNeedsAttention };
  const notifs = [{ id: 'n1', at: 1000, kind: 'approval', sessionId: 'hid', title: 'Approve?' }];
  const env = makeEnv(notifs, null, (u) => {
    if (u.startsWith('/api/rc/sessions')) {
      return u.includes('all=1') ? { sessions: [shown, hidden] } : { sessions: [shown], view: { label: 'Open', hidden: 1 } };
    }
    return undefined;
  });
  env.ctx.DeckThread = require('./thread.js');
  if (onES) { const Base = env.ctx.EventSource; env.ctx.EventSource = function () { Base.call(this); onES(this); }; }
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  return env;
}

test('Filtered view: a hidden session’s approval is handled once needsAttention clears', async () => {
  for (const [needs, handled] of [[true, false], [false, true]]) {
    const env = hiddenFilterEnv(needs);
    await new Promise((r) => setTimeout(r, 80));
    env.els.get('#screenInbox').hidden = false;
    env.listeners.get('#inboxRefresh').click();
    await new Promise((r) => setTimeout(r, 50));
    let card = null;
    walk(env.els.get('#inboxList'), (n) => { if (/\bnotif\b/.test(n.className || '')) card = n; });
    assert.ok(card, 'inbox card rendered');
    assert.strictEqual(/\bhandled\b/.test(card.className), handled, 'needsAttention=' + needs);
    assert.ok(!/Session no longer listed/.test(card.innerHTML), 'hidden session is still known');
  }
});

test('Filtered view: a foreground toast for a hidden session opens its sheet', async () => {
  const sources = [];
  const env = hiddenFilterEnv(false, (es) => sources.push(es));
  await new Promise((r) => setTimeout(r, 80));
  const es = sources[sources.length - 1];
  assert.ok(es && es.onmessage, 'event stream is open');
  es.onmessage({ data: JSON.stringify({ type: 'reply', sessionId: 'hid', error: 'boom' }) });
  const toastEl = env.ctx.document.body.children.find((c) => c && c.className === 'toast');
  assert.ok(toastEl && toastEl._ls.click, 'the toast is tappable');
  toastEl._ls.click();
  await new Promise((r) => setTimeout(r, 50));
  const opened = env.ctx.document.body.children.some((c) => c && c.id === 'sheet');
  assert.ok(opened, 'the hidden session\u2019s sheet opened');
});

// --- Tower tab BACKLOG (backlog.js) -------------------------------------------------

function backlogEnv(modelExtra, overviewExtra) {
  const model = Object.assign({
    header: 'TOWER', generated_at: new Date().toISOString(), palette: {}, legend: [],
    sections: [{ id: 'suggested', title: 'SUGGESTED', count: '2', urgent: 0, folded: null, items: [
      { kind: 'suggest', id: 'sg-a', key: 'suggest:sg-a', status: 'suggest', title: 'Shown', prompt: 'p', why_blocks: [], action_blocks: [], bangs: [], head: 'sg-a' },
      { kind: 'suggest', id: 'sg-b', key: 'suggest:sg-b', status: 'suggest', title: 'Hidden', prompt: 'p', why_blocks: [], action_blocks: [], bangs: [], head: 'sg-b', folded: { since: null } }] }],
  }, modelExtra);
  const posts = [];
  const env = makeEnv([], null, (u, opts) => {
    if (u.startsWith('/api/rc/tower/whoami')) return { actions_enabled: true, act: true };
    if (u.startsWith('/api/rc/tower/model')) return { enabled: true, actions: true, model };
    if (u.startsWith('/api/rc/tower/act')) { posts.push(JSON.parse(opts.body)); return { ok: true, msg: 'queued → tower' }; }
    if (u.startsWith('/api/rc/tower')) return { enabled: true, mtime: 'x', overview: Object.assign({ generated_at: model.generated_at, counts: { waiting: 0 } }, overviewExtra) };
    return undefined;
  });
  env.ctx.window.DeckBacklog = require('./backlog.js');
  env.ctx.confirm = () => true;
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  const list = () => env.els.get('#towerList');
  const find = (pred) => { const out = []; walk(list(), (n) => { if (pred(n)) out.push(n); }); return out; };
  const text = (n) => { let t = n.textContent || ''; (n.children || []).forEach((c) => { if (c && typeof c === 'object') t += text(c); }); return t; };
  const click = async (n) => { n._ls.click({ stopPropagation() {} }); await new Promise((r) => setTimeout(r, 30)); };
  const backlogHead = () => find((n) => /\btbacklog\b/.test(n.className || ''))[0];
  return { env, posts, list, find, text, click, backlogHead };
}

const BACKLOG = [
  { id: 'T-42.fix_Thing', title: 'Fix the thing that has been broken for a while', status: 'backlog', age_days: 12.4, created: '2026-10-22T08:00:00Z', goal: 'g-home', project: 'p', class: 'c', not_before: null, deps_open: ['T-7'] },
  { id: 'T-43', title: 'Later', status: 'backlog', age_days: 3, goal: 'g-x', not_before: '2026-11-20T08:00:00Z', deps_open: [] },
  { id: 'T-44', title: 'Already deferred', status: 'backlog', age_days: 1, goal: '', deps_open: [] },
];

test('Tower tab BACKLOG: collapsed with the count, rows on open (age, id, goal, title, deps, not before)', async () => {
  const t = backlogEnv({ backlog: BACKLOG, counts: { backlog: 3 }, open_requests: ['pick-defer-t-44'] });
  await new Promise((r) => setTimeout(r, 100));
  const head = t.backlogHead();
  assert.ok(head, 'BACKLOG header rendered');
  assert.match(t.text(head), /BACKLOG \(3\)/);
  assert.match(t.text(head), /▸/, 'collapsed by default');
  assert.strictEqual(t.find((n) => /\btbrow\b/.test(n.className || '')).length, 0);
  await t.click(head);
  const rows = t.find((n) => /\btbrow\b/.test(n.className || ''));
  assert.strictEqual(rows.length, 3);
  const r0 = t.text(rows[0]);
  assert.match(r0, /12 d/); assert.match(r0, /T-42\.fix_Thing/); assert.match(r0, /g-home/); assert.match(r0, /waits on T-7/);
  assert.ok(!/not before/.test(r0));
  assert.match(t.text(rows[1]), /not before 2026-11-20/);
  assert.ok(!/waits on/.test(t.text(rows[1])));
  // T-44 has pick-defer-t-44 open with tower: no actions, "queued with tower"
  assert.match(t.text(rows[2]), /queued with tower/);
  assert.ok(!t.text(rows[2]).includes('Start'));
});

test('Tower tab BACKLOG: start / defer / drop confirm, then POST tower/act with the exact task id', async () => {
  const t = backlogEnv({ backlog: BACKLOG, counts: { backlog: 3 } });
  await new Promise((r) => setTimeout(r, 100));
  await t.click(t.backlogHead());
  const btn = (row, label) => t.find((n) => /\btbtn\b/.test(n.className || '') && n.textContent === label)[row];
  // declining a confirm sends nothing
  t.env.ctx.confirm = () => false;
  await t.click(btn(0, 'Start'));
  await t.click(btn(0, 'Drop'));
  assert.deepStrictEqual(t.posts, []);
  // start
  let asked = '';
  t.env.ctx.confirm = (m) => { asked = m; return true; };
  await t.click(btn(0, 'Start'));
  assert.match(asked, /T-42\.fix_Thing/);
  assert.deepStrictEqual(t.posts, [{ op: 'backlog_start', id: 'T-42.fix_Thing' }]);
  // the row now reads "queued with tower" (the overview has not caught up yet)
  let rows = t.find((n) => /\btbrow\b/.test(n.className || ''));
  assert.match(t.text(rows[0]), /queued with tower/);
  // T-42 has no buttons now, so the first row with actions is T-43.
  // defer: the picker defaults to tomorrow; the confirm posts not_before at 08:00Z
  await t.click(btn(0, 'Defer…'));
  const inp = t.find((n) => n.className === 'tdate')[0];
  assert.ok(inp, 'date picker shown');
  const B = require('./backlog.js');
  assert.strictEqual(inp.value, B.tomorrow());
  inp.value = '2026-12-01';
  await t.click(btn(0, 'Defer'));
  assert.match(asked, /T-43 until 2026-12-01/);
  assert.deepStrictEqual(t.posts[1], { op: 'backlog_defer', id: 'T-43', not_before: '2026-12-01T08:00:00Z' });
  // T-43 is queued now too, so the remaining Drop belongs to T-44
  rows = t.find((n) => /\btbrow\b/.test(n.className || ''));
  assert.match(t.text(rows[1]), /queued with tower/);
  await t.click(btn(0, 'Drop'));
  assert.deepStrictEqual(t.posts[2], { op: 'backlog_drop', id: 'T-44' });
  assert.strictEqual(t.posts.length, 3);
});

test('Tower tab: an overview without backlog keys shows no BACKLOG section and no errors', async () => {
  const errors = [];
  const t = backlogEnv({});
  const orig = t.env.ctx.console;
  t.env.ctx.console = Object.assign({}, orig, { error: (...a) => errors.push(a) });
  await new Promise((r) => setTimeout(r, 100));
  assert.strictEqual(t.backlogHead(), undefined);
  assert.ok(t.find((n) => /tsechead/.test(n.className || '')).length === 1, 'the other sections still render');
  assert.deepStrictEqual(errors, []);
});

test('Tower tab legacy (overview only) lists the backlog read-only', async () => {
  const t = backlogEnv({}, { backlog: BACKLOG, counts: { waiting: 0, backlog: 3 } });
  t.env.ctx.fetch = ((f) => async (u, o) => (String(u).startsWith('/api/rc/tower/model') ? { ok: true, status: 200, text: async () => JSON.stringify({ enabled: true, error: 'x' }) } : f(u, o)))(t.env.ctx.fetch);
  await new Promise((r) => setTimeout(r, 100));
  const head = t.backlogHead();
  assert.ok(head);
  await t.click(head);
  assert.strictEqual(t.find((n) => /\btbrow\b/.test(n.className || '')).length, 3);
  assert.strictEqual(t.find((n) => n.textContent === 'Start').length, 0, 'no actions without the model');
});

test('Tower tab suggestions: folded ones hide behind "N folded"; Fold posts the ask fold shape', async () => {
  const t = backlogEnv({});
  await new Promise((r) => setTimeout(r, 100));
  const det = t.find((n) => /\btsugfolded\b/.test(n.className || ''))[0];
  assert.ok(det, 'a folded toggle exists');
  assert.match(t.text(det), /^1 folded/);
  assert.match(t.text(det), /Hidden/);
  const top = t.list().children.filter((c) => c !== det).map(t.text).join('|');
  assert.ok(top.includes('Shown') && !top.includes('Hidden'), 'the folded suggestion is not shown outside the toggle');
  const fold = t.find((n) => /\btbtn\b/.test(n.className || '') && n.textContent === 'Fold')[0];
  await t.click(fold);
  assert.deepStrictEqual(t.posts, [{ op: 'fold', id: 'sg-a' }]);
  const unfold = t.find((n) => /\btbtn\b/.test(n.className || '') && n.textContent === 'Unfold')[0];
  await t.click(unfold);
  assert.deepStrictEqual(t.posts[1], { op: 'unfold', id: 'sg-b' });
});

// --- Tower reply drafts (G15, towerdraft.js): the real app.js render path ----------------

test('Tower reply draft: typed text survives a refresh (same ask), shows as "Ask closed" when the ask is removed, and is cleared by a successful send', async () => {
  let asks = ['ask-a', 'ask-b'];
  const replies = [];
  const model = () => ({ header: 'TOWER', generated_at: new Date().toISOString(), palette: {}, legend: [], sections: [{ id: 'waiting', title: 'WAITING', count: String(asks.length), urgent: 0, folded: null,
    items: asks.map((id) => ({ kind: 'ask', id, key: 'ask:' + id, status: 'waiting', blocks: [{ k: 'p', t: 'Q ' + id }], bangs: [] })) }] });
  const env = makeEnv([], null, (u, opts) => {
    if (u.startsWith('/api/rc/tower/whoami')) return { actions_enabled: true, act: true };
    if (u.startsWith('/api/rc/tower/model')) return { enabled: true, actions: true, model: model() };
    if (u.startsWith('/api/rc/tower/reply')) { replies.push(JSON.parse(opts.body)); return { requestId: 'r1' }; }
    if (u.startsWith('/api/rc/tower')) return { enabled: true, mtime: 'x', overview: { generated_at: new Date().toISOString(), counts: { waiting: asks.length } } };
    return undefined;
  });
  vm.createContext(env.ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8'), env.ctx, { filename: 'app.js' });
  const wait = () => new Promise((r) => setTimeout(r, 100));
  const find = (pred) => { const out = []; walk(env.els.get('#towerList'), (n) => { if (pred(n)) out.push(n); }); return out; };
  const boxes = () => find((n) => n.dataset && n.dataset.ask);
  const text = (n) => { let t = n.textContent || ''; (n.children || []).forEach((c) => { if (c && typeof c === 'object') t += text(c); }); return t; };
  const refresh = async () => { await env.listeners.get('#towerRefresh').click(); await wait(); };
  await wait();
  const a1 = boxes().find((b) => b.dataset.ask === 'ask-a');
  assert.ok(a1, 'reply box for ask-a');
  a1.value = 'half-typed reply'; a1._ls.input();
  await refresh();
  const a2 = boxes().find((b) => b.dataset.ask === 'ask-a');
  assert.notStrictEqual(a2, a1, 'the list was rebuilt');
  assert.strictEqual(a2.value, 'half-typed reply');
  assert.strictEqual(boxes().find((b) => b.dataset.ask === 'ask-b').value, '');
  assert.strictEqual(find((n) => /\btdraft\b/.test(n.className || '')).length, 0);
  // the ask closes: no box for it, the draft is shown (with Copy) instead of dropped
  asks = ['ask-b'];
  await refresh();
  assert.ok(!boxes().some((b) => b.dataset.ask === 'ask-a'));
  const closed = find((n) => /\btdraft\b/.test(n.className || ''));
  assert.strictEqual(closed.length, 1);
  assert.match(text(closed[0]), /Ask closed.*ask-a.*half-typed reply/);
  assert.ok(find((n) => /\btbtn\b/.test(n.className || '') && n.textContent === 'Copy').length >= 1);
  // back again: restored into the box; a successful send clears the draft for good
  asks = ['ask-a', 'ask-b'];
  await refresh();
  const a3 = boxes().find((b) => b.dataset.ask === 'ask-a');
  assert.strictEqual(a3.value, 'half-typed reply');
  const rb = find((n) => n.textContent === 'Reply')[0];
  await rb._ls.click(); await wait();
  assert.deepStrictEqual(replies, [{ ask: 'ask-a', text: 'half-typed reply' }]);
  assert.strictEqual(a3.value, '');
  await refresh();
  assert.strictEqual(boxes().find((b) => b.dataset.ask === 'ask-a').value, '');
  assert.strictEqual(env.store.get('dr-tower-drafts'), undefined);
});
