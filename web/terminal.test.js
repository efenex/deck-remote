// Run: node --test web/
// F12 = a (ws_ticket.go): terminal.html fetches a single-use ticket with the bearer in the
// Authorization header before every connect and reconnect, and never puts the token in the
// WebSocket URL. The page's inline script runs here against stubbed DOM, xterm, fetch and WebSocket.
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const html = fs.readFileSync(path.join(__dirname, 'terminal.html'), 'utf8');
const script = html.slice(html.lastIndexOf('<script>') + '<script>'.length, html.lastIndexOf('</script>'));

// stub is a do-anything object: every property is another stub, calls and `new` return stubs.
function stub() {
  return new Proxy(function () {}, {
    get(t, k) {
      if (k === Symbol.toPrimitive) return () => '';
      if (!(k in t)) t[k] = stub();
      return t[k];
    },
    set(t, k, v) { t[k] = v; return true; },
    apply() { return stub(); },
    construct() { return stub(); },
  });
}

function run(ticketReplies) {
  const fetches = [];
  const sockets = [];
  const timers = [];
  const replies = ticketReplies.slice();
  function FakeWS(url) { this.url = url; this.sent = []; sockets.push(this); }
  FakeWS.OPEN = 1;
  FakeWS.prototype.send = function (d) { this.sent.push(d); };
  FakeWS.prototype.close = function () {};
  const sandbox = {
    Terminal: function () { return stub(); },
    FitAddon: { FitAddon: function () { return stub(); } },
    matchMedia: () => ({ matches: true }),
    visualViewport: null, innerHeight: 800,
    addEventListener() {},
    document: { getElementById: () => stub(), querySelector: () => stub(), querySelectorAll: () => [] },
    location: { search: '?id=sess-a', protocol: 'https:', host: 'deck.example' },
    localStorage: { getItem: () => 'tok-123' },
    URL, URLSearchParams, TextDecoder, JSON, Math, Promise,
    setTimeout: (fn) => { timers.push(fn); return timers.length; },
    clearTimeout() {},
    WebSocket: FakeWS,
    fetch: async (url, opts) => {
      fetches.push({ url, opts });
      const r = replies.shift() || { status: 500, body: {} };
      return { status: r.status, json: async () => r.body };
    },
  };
  sandbox.window = sandbox;
  vm.runInNewContext(script, sandbox);
  return { fetches, sockets, timers };
}
const tick = () => new Promise((r) => setImmediate(r));

test('connect: ticket over the header, the WebSocket URL carries the ticket and no token', async () => {
  const { fetches, sockets } = run([{ status: 200, body: { ticket: 'T1', expiresIn: 30 } }]);
  await tick();
  assert.strictEqual(fetches.length, 1);
  assert.strictEqual(fetches[0].url, '/api/rc/ws/ticket');
  assert.strictEqual(fetches[0].opts.method, 'POST');
  assert.strictEqual(fetches[0].opts.headers.Authorization, 'Bearer tok-123');
  assert.deepStrictEqual(JSON.parse(fetches[0].opts.body), { session: 'sess-a' });
  assert.strictEqual(sockets.length, 1);
  assert.strictEqual(sockets[0].url, 'wss://deck.example/ws/session/sess-a?ticket=T1');
  assert.ok(!sockets[0].url.includes('token') && !sockets[0].url.includes('tok-123'));
});

test('reconnect fetches a fresh ticket', async () => {
  const { fetches, sockets, timers } = run([
    { status: 200, body: { ticket: 'T1' } },
    { status: 200, body: { ticket: 'T2' } },
  ]);
  await tick();
  sockets[0].onclose();
  assert.ok(timers.length >= 1, 'a reconnect is scheduled');
  timers[timers.length - 1]();
  await tick();
  assert.strictEqual(fetches.length, 2);
  assert.strictEqual(sockets.length, 2);
  assert.strictEqual(sockets[1].url, 'wss://deck.example/ws/session/sess-a?ticket=T2');
});

test('a refused ticket opens no socket and does not retry', async () => {
  const { fetches, sockets, timers } = run([{ status: 403, body: { error: 'device token ro lacks the act capability' } }]);
  await tick();
  assert.strictEqual(fetches.length, 1);
  assert.strictEqual(sockets.length, 0);
  assert.strictEqual(timers.length, 0);
});

test('terminal.html never sets token on a URL', () => {
  assert.ok(!/searchParams\.set\(\s*['"]token['"]/.test(script));
  assert.ok(!/[?&]token=/.test(script.replace(/\/\/.*$/gm, '')));
});
