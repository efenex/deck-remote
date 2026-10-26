// Run: node --test web/
// F7 = a: the typed-input endpoints refuse ?token= and need the Authorization header, so the PWA
// must call them through api() (which sends authHeaders()) and never through tokenURL().
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const src = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const TYPED = ['/api/rc/ask', '/api/rc/slash', '/api/rc/approve', '/api/rc/attention/respond', '/api/rc/interrupt'];

// Pull api() out of app.js and run it against a fake fetch: it must send the bearer header.
function loadApi(token) {
  const start = src.indexOf('  function authHeaders(');
  const apiStart = src.indexOf('  async function api(');
  const apiEnd = src.indexOf('\n  }\n', apiStart) + 4;
  const authEnd = src.indexOf('\n  }\n', start) + 4;
  const calls = [];
  const body = 'let TOKEN = ' + JSON.stringify(token) + '; let CURRENT_PROFILE = "";' +
    'function withProfileParam(p) { return p; } function forgetToken() {}' +
    src.slice(start, authEnd) + src.slice(apiStart, apiEnd) + 'return api;';
  const fetch = async (url, opts) => { calls.push({ url, opts }); return { status: 202, ok: true, text: async () => '{}' }; };
  return { api: new Function('fetch', 'DeckNetwork', 'connection', 'renderConnection', body)(fetch, require('./network.js'), {}, () => {}), calls };
}

test('api() sends Authorization: Bearer and keeps the token out of the URL', async () => {
  const { api, calls } = loadApi('tok-123');
  for (const p of TYPED) await api('POST', p, { sessionId: 's' });
  assert.strictEqual(calls.length, TYPED.length);
  for (const c of calls) {
    assert.strictEqual(c.opts.headers.Authorization, 'Bearer tok-123');
    assert.ok(!String(c.url).includes('tok-123'), 'token must not be in ' + c.url);
  }
});

test('the PWA calls every typed endpoint it uses through api(), never tokenURL()', () => {
  for (const p of TYPED) {
    for (const m of src.matchAll(new RegExp("['\"]" + p.replace(/\//g, '\\/') + "['\"]", 'g'))) {
      const line = src.slice(src.lastIndexOf('\n', m.index) + 1, src.indexOf('\n', m.index));
      if (line.trim().startsWith('*') || line.trim().startsWith('//')) continue;
      assert.ok(!/tokenURL\(/.test(line), p + ' must not use tokenURL: ' + line.trim());
      assert.ok(/api\('POST'|const path = /.test(line), p + ' must go through api(): ' + line.trim());
    }
  }
  // the composer's path variable is passed to api()
  assert.match(src, /const path = slash \? '\/api\/rc\/slash' : '\/api\/rc\/ask';\s*\n\s*const r = await api\('POST', path,/);
});
