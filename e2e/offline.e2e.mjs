// Uses the existing UI fixture server; no real sessions or sends.
import test from 'node:test';
import assert from 'node:assert/strict';
import { startMockApi, openApp, launchBrowser, withDefaults } from './harness.mjs';

test('phone: offline refresh feedback, transcript failure and resume layout recovery', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchBrowser();
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' }));
  const page = await ctx.newPage(); const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.route('**/api/rc/tower**', route => {
    const path = new URL(route.request().url()).pathname;
    const data = path.endsWith('/model') ? { model: { header: 'TOWER 15:02Z', generated_at: '2026-10-25T15:02:00Z', sections: [] } } :
      path.endsWith('/whoami') ? {} : { enabled: true, overview: { generated_at: '2026-10-25T15:02:00Z', counts: {} } };
    return route.fulfill({ json: data });
  });
  await openApp(page, base + '/?token=fake');
  // A black-holed response must have a finite spinner, including on the session shield.
  await page.route('**/api/rc/**', route => new Promise(resolve => {
    setTimeout(() => route.abort().catch(() => {}).finally(resolve), 13500);
  }));
  await page.click('#goRefresh');
  assert.equal(await page.locator('#goRefresh').isDisabled(), true);
  await page.waitForFunction(() => !document.querySelector('#goRefresh').disabled, { }, { timeout: 15000 });
  assert.match(await page.locator('.toast').last().innerText(), /Deck refresh failed.*Host did not respond/s);
  await page.unroute('**/api/rc/**');
  await page.route('**/api/rc/**', route => route.abort('internetdisconnected'));
  for (const [tab, id, label] of [['inbox', 'inboxRefresh', 'Inbox'], ['tower', 'towerRefresh', 'Tower']]) {
    await page.click('.tab[data-tab="' + tab + '"]'); await page.click('#' + id);
    await page.waitForFunction(id => !document.getElementById(id).disabled, id);
    assert.match(await page.locator('.toast').last().innerText(), new RegExp(label + ' refresh failed'));
  }
  assert.match(await page.locator('#towerAsOf').innerText(), /2026-10-25T15:02:00Z.*STALE/);
  await page.click('.tab[data-tab="deck"]'); await page.click('.card[data-id="s1"]');
  await page.waitForFunction(() => document.querySelector('#convo').textContent.includes('Transcript unavailable'));
  assert.match(await page.locator('#sheetConnection').innerText(), /Host unreachable.*last contact/s);
  await page.click('#sheetCheckPerm');
  await page.waitForFunction(() => !document.querySelector('#sheetCheckPerm').disabled);
  assert.match(await page.locator('.toast').last().innerText(), /Attention refresh failed/);
  await page.evaluate(() => {
    document.activeElement.blur(); document.documentElement.classList.add('kb');
    const sheet = document.querySelector('#sheet'); sheet.style.height = '400px'; sheet.style.bottom = 'auto';
    window.dispatchEvent(new Event('pageshow'));
  });
  assert.equal(await page.evaluate(() => document.documentElement.classList.contains('kb')), false);
  assert.equal(await page.locator('#sheet').evaluate(el => el.style.height), '');
  assert.deepEqual(errors, []);
});

test('phone: Tower BACKLOG section, defer picker and the act payload', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchBrowser();
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' }));
  const page = await ctx.newPage(); const errors = []; const posts = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('dialog', d => d.accept());
  const gen = new Date().toISOString();
  const backlog = [
    { id: 'T-42.fix_Thing', title: 'Fix the thing', status: 'backlog', age_days: 12, goal: 'g-home', deps_open: ['T-7'], not_before: null },
    { id: 'T-43', title: 'Later', status: 'backlog', age_days: 2, goal: 'g-x', deps_open: [], not_before: '2026-11-20T08:00:00Z' },
  ];
  await page.route('**/api/rc/tower**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/act')) { posts.push(route.request().postDataJSON()); return route.fulfill({ json: { ok: true, msg: 'queued → tower' } }); }
    const data = path.endsWith('/model') ? { enabled: true, actions: true, model: { header: 'TOWER', generated_at: gen, sections: [], palette: {}, legend: [], backlog, counts: { backlog: 2 } } } :
      path.endsWith('/whoami') ? { actions_enabled: true, act: true } : { enabled: true, overview: { generated_at: gen, counts: {} } };
    return route.fulfill({ json: data });
  });
  await openApp(page, base + '/?token=fake');
  await page.click('.tab[data-tab="tower"]');
  const head = page.locator('.tsechead.tbacklog');
  await head.waitFor();
  const txt = async l => (await l.textContent()).replace(/\s+/g, ' ');
  assert.match(await txt(head), /▸ BACKLOG \(2\)/);
  assert.equal(await page.locator('.tbrow').count(), 0, 'collapsed by default');
  await head.click();
  assert.equal(await page.locator('.tbrow').count(), 2);
  assert.match(await txt(page.locator('.tbrow').first()), /12 d T-42\.fix_Thing · g-home.*Fix the thing.*waits on T-7/);
  await page.locator('.tbrow').first().getByRole('button', { name: 'Defer…' }).click();
  const date = page.locator('.tbrow input.tdate');
  const tomorrow = await page.evaluate(() => window.DeckBacklog.tomorrow());
  assert.equal(await date.inputValue(), tomorrow);
  await date.fill('2026-12-01');
  await page.locator('.tbrow').first().getByRole('button', { name: 'Defer', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('.tbrow').textContent.includes('queued with tower'));
  assert.deepEqual(posts, [{ op: 'backlog_defer', id: 'T-42.fix_Thing', not_before: '2026-12-01T08:00:00Z' }]);
  const box = await page.locator('.tbrow').first().boundingBox();
  assert.ok(box.x >= 0 && box.x + box.width <= 390, 'row fits the phone width');
  await page.screenshot({ path: process.env.E2E_SHOT || '/dev/null' }).catch(() => {});
  assert.deepEqual(errors, []);
});

test('phone: Tower GA TODO section (bounded feed), filter keeps focus, start + priority payloads', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchBrowser();
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' }));
  const page = await ctx.newPage(); const errors = []; const posts = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('dialog', d => d.accept());
  const gen = new Date().toISOString();
  const sha = '0123456789abcdef0123456789abcdef01234567';
  const row = (key, title, prio, extra) => Object.assign({ key, doc: 'docs/TODO-backlog.md', line_start: 3, line_end: 5, parent: null, title, section: 'Sec',
    doc_state: 'open', closable: true, closing_subentries: 0, body_sha: '00112233445566ff', body_head: 'head of ' + key + ' ghp_' + 'A'.repeat(30),
    body_url: 'https://git.example/blob/' + sha + '/docs/TODO-backlog.md#L3', view_url: 'file:///x' }, prio ? { priority: prio } : {}, extra || {});
  const items = [row('back@aaaaaaaaaaaa', 'First P1', { p: 'P1', rank: 0, pending: false }), row('back@bbbbbbbbbbbb', 'Second P1', { p: 'P1', rank: 1, pending: true })];
  for (let i = 0; i < 30; i++) items.push(row('todo@' + i.toString(16).padStart(12, '0'), 'Other ' + i, null));
  const ga_todo = { schema_version: 1, source_sha: sha, source_ref: 'origin/main', built_at: gen, input_digest: 'a'.repeat(64), total_units: 99, total_open: 50,
    more: 10, collision_count: 0, orphan_count: 0, stale: false, error: '', items, in_flight_keys: [],
    pending: [{ id: 'ga-prio-20261125T100000Z-aaaaaaaaaaaa', edits: [{ key: 'back@bbbbbbbbbbbb', p: 'P1' }] }], payload_bytes: 1000, payload_budget: 81920 };
  await page.route('**/api/rc/tower**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/act')) { posts.push(route.request().postDataJSON()); return route.fulfill({ json: { ok: true, msg: 'queued → tower' } }); }
    const data = path.endsWith('/model') ? { enabled: true, actions: true, model: { header: 'TOWER', generated_at: gen, sections: [{ id: 'gatodo', title: 'GA TODO', count: '1', items: [{ kind: 'gatodo', id: 'FULLVIEW', key: 'gatodo:FULLVIEW', head: 'FULLVIEW' }] }], palette: {}, legend: [], ga_todo } } :
      path.endsWith('/whoami') ? { actions_enabled: true, act: true } : { enabled: true, overview: { generated_at: gen, counts: {} } };
    return route.fulfill({ json: data });
  });
  await openApp(page, base + '/?token=fake');
  await page.click('.tab[data-tab="tower"]');
  const head = page.locator('.tsechead.tgatodo');
  await head.waitFor();
  const txt = async l => (await l.textContent()).replace(/\s+/g, ' ');
  assert.match(await txt(head), /▸ GA TODO \(27 \+ 15 more\)/);
  assert.equal(await page.locator('.tgarow').count(), 0, 'collapsed by default');
  assert.ok(!(await txt(page.locator('#towerList'))).includes('FULLVIEW'), 'the TUI full-view section is never drawn');
  await head.click();
  assert.equal(await page.locator('.tgarow').count(), 27);
  assert.match(await txt(page.locator('.tgapending')), /Pending with tower \(1/);
  assert.match(await txt(page.locator('.tgarow').nth(1)), /\[P1 pending\] back@bbbbbbbbbbbb/);
  // filter in place: the input node survives typing
  const f = page.locator('.tgafilter');
  await f.click();
  await page.keyboard.type('second p1');
  assert.equal(await page.evaluate(() => document.activeElement && document.activeElement.classList.contains('tgafilter')), true);
  assert.equal(await page.locator('.tgarow').count(), 1);
  await f.fill('');
  // expand the first row: redacted body head, SHA-pinned link, then Implement and a move down
  await page.locator('.tgarow').first().locator('.thead').click();
  const first = page.locator('.tgarow').first();
  assert.match(await txt(first.locator('.tgahead')), /\[redacted\]/);
  assert.equal(await first.locator('a.tlink').getAttribute('href'), items[0].body_url);
  await first.getByRole('button', { name: '↓' }).click();
  await page.waitForFunction(() => document.querySelectorAll('.tgarow').length > 0);
  await page.locator('.tgarow').first().getByRole('button', { name: 'Implement' }).click();
  await page.waitForFunction(() => document.querySelector('.tgarow').textContent.includes('queued with tower'));
  assert.deepEqual(posts, [
    { op: 'gatodo_prio', key: 'back@aaaaaaaaaaaa', p: 'P1', after: 'back@bbbbbbbbbbbb', view_sha: sha },
    { op: 'gatodo_start', key: 'back@aaaaaaaaaaaa', mode: 'implement', view_sha: sha, body_sha: '00112233445566ff' },
  ]);
  const box = await page.locator('.tgarow').first().boundingBox();
  assert.ok(box.x >= 0 && box.x + box.width <= 390, 'row fits the phone width');
  assert.deepEqual(errors, []);
});

// Feed schema 2: meta title/summary as text, shed rows, the producer budget apart from the wire estimate, and a
// visible refusal (no rows, no actions) for an unsupported version or the TUI's full view.
test('phone: Tower GA TODO feed schema 2 (meta text, shed row, budget) and visible refusal of unsupported blocks', { timeout: 60000 }, async t => {
  const base = await startMockApi(t);
  const browser = await launchBrowser();
  t.after(() => browser.close());
  const ctx = withDefaults(await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, serviceWorkers: 'block' }));
  const page = await ctx.newPage(); const errors = []; const posts = [];
  page.on('pageerror', e => errors.push(e.message));
  page.on('dialog', d => d.accept());
  const gen = new Date().toISOString();
  const sha = '0123456789abcdef0123456789abcdef01234567';
  const row = (key, p, extra) => Object.assign({ key, doc: 'docs/TODO-backlog.md', line_start: 3, line_end: 5, parent: null, title: 'RAW HEADING ' + key, section: 'Sec',
    doc_state: 'open', closable: true, closing_subentries: 0, body_sha: '00112233445566ff', body_head: 'head of ' + key,
    body_url: 'https://git.example/blob/' + sha + '/docs/TODO-backlog.md#L3', view_url: 'file:///x', status: 'open',
    meta: { title: 'Meta <b>title</b> ' + key, summary: 'Summary <i>' + key + '</i> ghp_' + 'A'.repeat(30), source: 'sidecar', kind: 'bug' },
    priority: { p, rank: 0, pending: false } }, extra || {});
  const items = [row('back@aaaaaaaaaaaa', 'P1'),
    row('back@bbbbbbbbbbbb', 'P1', { body_head: '', body_head_shed: true, meta: { title: 'Shed row', summary: '', source: 'fallback', stale: true }, enrich: { summary: 'ENRICH MUST NOT SHOW' } })];
  const v2 = { schema_version: 2, source_sha: sha, source_ref: 'origin/main', built_at: gen, input_digest: 'a'.repeat(64), total_units: 99, total_open: 50,
    more: 0, collision_count: 0, orphan_count: 0, stale: false, error: '', items, in_flight_keys: [], pending: [],
    budget: { target_bytes: 80000, items_bytes: 98765, shed: 1, over: true }, payload_bytes: 2000, payload_budget: 81920 };
  let block = v2;
  await page.route('**/api/rc/tower**', route => {
    const path = new URL(route.request().url()).pathname;
    if (path.endsWith('/act')) { posts.push(route.request().postDataJSON()); return route.fulfill({ json: { ok: true, msg: 'queued → tower' } }); }
    const data = path.endsWith('/model') ? { enabled: true, actions: true, model: { header: 'TOWER', generated_at: gen, sections: [], palette: {}, legend: [], ga_todo: block } } :
      path.endsWith('/whoami') ? { actions_enabled: true, act: true } : { enabled: true, overview: { generated_at: gen, counts: {} } };
    return route.fulfill({ json: data });
  });
  await openApp(page, base + '/?token=fake');
  await page.click('.tab[data-tab="tower"]');
  const head = page.locator('.tsechead.tgatodo');
  await head.waitFor();
  const txt = async l => (await l.textContent()).replace(/\s+/g, ' ');
  assert.match(await txt(head), /GA TODO \(2\).*over size budget \(99 KB\)/);
  await head.click();
  const rows = page.locator('.tgarow');
  assert.equal(await rows.count(), 2);
  // meta.title and meta.summary are text, never markup, and redacted; the raw heading is not the label
  assert.equal(await rows.first().locator('.ttitle').textContent(), 'Meta <b>title</b> back@aaaaaaaaaaaa');
  assert.equal(await rows.first().locator('.ttitle b').count(), 0);
  assert.match(await txt(rows.first().locator('.tgasummary')), /^Summary <i>back@aaaaaaaaaaaa<\/i> \[redacted\]$/);
  assert.ok(!(await txt(page.locator('#towerList'))).includes('RAW HEADING'));
  assert.match(await txt(page.locator('#towerList')), /Feed size budget: 1 row trimmed to its source link.*OVER even with every detail trimmed/);
  // the shed row: no body, an explicit mark, the source link, an empty summary that enrich.summary does not fill
  await rows.nth(1).locator('.thead').click();
  const shed = page.locator('.tgarow').nth(1);
  assert.match(await txt(shed.locator('.tgashed')), /Detail trimmed to fit the feed size budget/);
  assert.equal(await shed.locator('.tgahead').count(), 0);
  assert.match(await txt(shed), /\(no summary\)/);
  assert.match(await txt(shed), /summary may be stale/);
  assert.ok(!(await txt(shed)).includes('ENRICH MUST NOT SHOW'));
  assert.equal(await shed.locator('a.tlink').getAttribute('href'), items[1].body_url);
  // actions still send the exact key only
  await shed.getByRole('button', { name: 'Implement' }).click();
  await page.waitForFunction(() => document.querySelectorAll('.tgarow')[1].textContent.includes('queued with tower'));
  assert.deepEqual(posts, [{ op: 'gatodo_start', key: 'back@bbbbbbbbbbbb', mode: 'implement', view_sha: sha, body_sha: '00112233445566ff' }]);

  // unsupported: another version, and the TUI's full view. Visible refusal, nothing drawn, no buttons.
  for (const bad of [Object.assign({}, v2, { schema_version: 3 }), Object.assign({}, v2, { schema_version: '2' }),
    { schema_version: 2, units: [{ key: 'FULLVIEW', body: 'FULL BODY' }] }, Object.assign({}, v2, { items: 'rows' })]) {
    block = bad;
    await page.click('#towerRefresh');
    await page.waitForFunction(() => /UNSUPPORTED/.test(document.querySelector('.tsechead.tgatodo').textContent));
    assert.equal(await page.locator('.tgarow').count(), 0);
    assert.equal(await page.locator('.tgafilter').count(), 0);
    assert.equal(await page.locator('#towerList button', { hasText: 'Implement' }).count(), 0);
    assert.match(await txt(page.locator('#towerList')), /GA TODO unsupported:/);
    assert.ok(!(await txt(page.locator('#towerList'))).includes('FULL BODY'));
  }
  assert.deepEqual(errors, []);
});
