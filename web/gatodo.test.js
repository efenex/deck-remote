// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const G = require('./gatodo.js');

const SHA = '0123456789abcdef0123456789abcdef01234567';
const NOW = Date.parse('2026-11-25T12:00:00Z');
const hex12 = (n) => n.toString(16).padStart(12, '0');

function row(key, title, prio, extra) {
  return Object.assign({
    key, doc: 'docs/TODO-backlog.md', line_start: 10, line_end: 14, parent: null, title, section: 'Sec', doc_state: 'open',
    closable: true, closing_subentries: 0, body_sha: '00112233445566ff', body_head: 'x'.repeat(1500),
    body_url: 'https://git.example/blob/' + SHA + '/docs/TODO-backlog.md#L10', view_url: 'file:///Users/x/view.html',
  }, prio ? { priority: prio } : {}, extra || {});
}

// The bounded overview block tower_model.py passes through: 6 explicit P0-P2 rows (one of them a
// pending overlay), 28 unprioritized + 2 P3 rows (30 others: the tab keeps 25), every string worst case.
function fixture() {
  const items = [];
  ['P0', 'P1', 'P1', 'P2', 'P2'].forEach((p, i) => items.push(row('back@' + hex12(i), 'prio ' + p + ' ' + i, { p, rank: i, pending: false })));
  items.push(row('back@' + hex12(9), 'moved to P1 (pending)', { p: 'P1', rank: 5, pending: true }));
  for (let i = 0; i < 30; i++) items.push(row('todo@' + hex12(0x100 + i), 'other ' + i, i >= 28 ? { p: 'P3', rank: i - 28, pending: i === 29 } : null));
  return {
    schema_version: 1, source_sha: SHA, source_ref: 'origin/main', built_at: '2026-11-25T11:00:00Z', input_digest: 'a'.repeat(64),
    total_units: 1208, total_open: 300, more: 260, collision_count: 0, orphan_count: 5, stale: false, error: '',
    items, in_flight_keys: ['todo@' + hex12(0x101)],
    pending: [
      { id: 'ga-prio-20261125T100000Z-aaaaaaaaaaaa', edits: [{ key: 'back@' + hex12(9), p: 'P1' }], status: 'open' },
      { id: 'pick-gatodo-design-' + 'f'.repeat(20) + '-0a0b0c0d', key: 'back@' + hex12(1), mode: 'design', status: 'open' },
      { id: 'ga-prio-20261125T100500Z-bbbbbbbbbbbb', edits: [{ key: 'back@' + hex12(2), p: 'P0', after: null }, { key: 'back@' + hex12(3), p: 'P2', after: 'back@' + hex12(4) }, { key: 'todo@' + hex12(0x100), p: null }], status: 'open' },
    ],
    payload_budget: 81920,
  };
}

test('bounded section: every explicit P0-P2 row plus 25 others; the cut is added to more', () => {
  const sec = G.section({ ga_todo: fixture() }, NOW);
  const bands = sec.rows.map(G.band);
  assert.strictEqual(sec.rows.length, 31);
  assert.strictEqual(bands.filter((b) => ['P0', 'P1', 'P2'].includes(b)).length, 6); // the pending P1 overlay counts as explicit
  assert.strictEqual(bands.filter((b) => b === null || b === 'P3').length, 25);
  assert.strictEqual(sec.more, 260 + 5);
  assert.strictEqual(sec.header, 'GA TODO (31 + 265 more)');
  assert.strictEqual(sec.stale, '');
  // the order is the block's (tower's band order), never re-sorted
  assert.deepStrictEqual(sec.rows.slice(0, 6).map((r) => r.key), fixture().items.slice(0, 6).map((r) => r.key));
});

test('explicit P0-P2 rows are never dropped to fit a cap; P3 rows count with the unprioritized', () => {
  const b = fixture();
  b.items = [];
  for (let i = 0; i < 80; i++) b.items.push(row('back@' + hex12(i), 't', { p: 'P' + (i % 3), rank: i, pending: false }));
  for (let i = 0; i < 40; i++) b.items.push(row('arch@' + hex12(i), 't', { p: 'P3', rank: i, pending: false }));
  const sec = G.section({ ga_todo: b }, NOW);
  assert.strictEqual(sec.rows.length, 80 + 25);
  assert.strictEqual(sec.more, 260 + 15);
});

test('payload: the bounded fixture (1500-byte body heads) stays under the ~80KB target; over budget is flagged, not cut', () => {
  const b = fixture();
  b.payload_bytes = JSON.stringify(b).length;
  assert.ok(b.payload_bytes < 81920, 'fixture ' + b.payload_bytes + ' bytes');
  assert.strictEqual(G.section({ ga_todo: b }, NOW).overBudget, false);
  b.payload_bytes = 90000;
  const sec = G.section({ ga_todo: b }, NOW);
  assert.strictEqual(sec.overBudget, true);
  assert.strictEqual(sec.rows.length, 31);
});

// --- feed schema 2 (meta/status/body_head_shed/budget) --------------------------------------------

// fixture() as a schema-2 feed: every row gets meta + status; row 0 is a fallback row with an EMPTY summary
// (and an enrich.summary that must never stand in for it), row 1 is shed, row 2 has a stale sidecar summary.
function fixtureV2(over) {
  const b = fixture();
  b.schema_version = 2;
  b.items.forEach((r, i) => {
    r.meta = { title: 'Meta title ' + i, summary: 'Meta summary ' + i, source: 'sidecar', kind: 'bug', area: 'bot', size: 'S' };
    r.status = 'open';
  });
  Object.assign(b.items[0].meta, { summary: '', source: 'fallback' });
  b.items[0].enrich = { summary: 'ENRICH SUMMARY must not show' };
  b.items[1].body_head = ''; b.items[1].body_head_shed = true;
  b.items[2].meta.stale = true;
  b.budget = { target_bytes: 80000, items_bytes: 53248, shed: 1, over: false };
  return Object.assign(b, over || {});
}

test('schema gate is strict: only the numbers 1 and 2; anything else is refused visibly with no rows', () => {
  for (const v of [1, 2]) {
    const sec = G.section({ ga_todo: Object.assign(fixtureV2(), { schema_version: v }) }, NOW);
    assert.strictEqual(sec.unsupported, '', 'v' + v);
    assert.strictEqual(sec.stale, '');
    assert.strictEqual(sec.rows.length, 31);
  }
  for (const v of [3, 0, -1, 1.5, '1', '2', true, null, undefined, [2], { v: 2 }]) {
    const sec = G.section({ ga_todo: Object.assign(fixtureV2(), { schema_version: v }) }, NOW);
    assert.match(sec.unsupported, /schema 1 or 2 only/, String(v));
    assert.strictEqual(sec.stale, sec.unsupported);
    assert.deepStrictEqual(sec.rows, [], 'no rows from an unsupported block: ' + String(v));
    assert.strictEqual(sec.more, 0);
    assert.strictEqual(sec.header, 'GA TODO · UNSUPPORTED');
    assert.match(G.staleReason(sec.block, NOW), /schema 1 or 2 only/);
  }
  const noVersion = fixtureV2();
  delete noVersion.schema_version;
  assert.match(G.section({ ga_todo: noVersion }, NOW).unsupported, /schema 1 or 2 only/);
});

test('the full view (units) and an absent or non-list items are refused, whatever else the block holds', () => {
  for (const patch of [{ units: [{ key: 'FULL', body: 'FULL BODY' }] }, { units: [] }, { units: null }, { items: undefined }, { items: null },
    { items: {} }, { items: 'rows' }, { items: { 0: row('back@' + hex12(1), 't', null) } }]) {
    for (const v of [1, 2]) {
      const sec = G.section({ ga_todo: Object.assign(fixtureV2(), { schema_version: v }, patch) }, NOW);
      assert.match(sec.unsupported, /full view or no item list/, JSON.stringify(Object.keys(patch)));
      assert.deepStrictEqual(sec.rows, []);
      assert.ok(sec.stale);
    }
  }
  assert.strictEqual(G.section({ ga_todo: { schema_version: 2, units: [{ key: 'FULL' }] } }, NOW).rows.length, 0);
});

test('v2 rows: meta.title preferred, meta.summary shown as its own text (empty stays empty, enrich.summary never)', () => {
  const b = fixtureV2();
  const q = new Set();
  const v0 = G.rowView(b.items[0], q);
  assert.strictEqual(v0.title, 'Meta title 0');
  assert.strictEqual(v0.hasSummary, true);
  assert.strictEqual(v0.summary, '', 'empty meta.summary is not replaced by enrich.summary');
  assert.ok(!JSON.stringify(v0).includes('ENRICH SUMMARY'));
  assert.strictEqual(v0.derived, true);
  const v2 = G.rowView(b.items[2], q);
  assert.strictEqual(v2.summary, 'Meta summary 2');
  assert.strictEqual(v2.metaStale, true);
  assert.strictEqual(v2.derived, false);
  assert.deepStrictEqual(v2.chips, ['open', 'bug', 'bot', 'S']);
  // v1 rows (no meta) fall back to the raw heading and have no summary slot, even with an enrich.summary
  const v1 = G.rowView(row('back@' + hex12(1), 'Raw heading', null, { enrich: { summary: 'ENRICH' } }), q);
  assert.strictEqual(v1.title, 'Raw heading');
  assert.strictEqual(v1.hasSummary, false);
  assert.strictEqual(v1.summary, '');
  assert.deepStrictEqual(v1.chips, []);
  // a v2 row with an unusable meta.title (empty / not a string / meta not an object) keeps the raw heading
  for (const meta of [{ title: '' }, { title: '  ' }, { title: 7 }, null, 'x', [], { summary: 5 }]) {
    const v = G.rowView(row('back@' + hex12(1), 'Raw heading', null, { meta }), q);
    assert.strictEqual(v.title, 'Raw heading', JSON.stringify(meta));
    assert.strictEqual(v.hasSummary, false);
  }
  // the dispatch data keeps the raw heading: meta.title is a label, never an identity
  const r = Object.assign({}, b.items[2], { w_key: 'back:1@' + hex12(2) });
  assert.strictEqual(G.startRequestData(r, 'design', b).title, 'prio P1 2');
  assert.ok(!('title' in G.startBody(r, 'design', b)));
  assert.strictEqual(G.startBody(r, 'design', b).key, r.key);
});

test('v2 text is plain text with every string redacted, nested meta included', () => {
  const tok = { ghp: 'ghp_' + 'A'.repeat(30), bearer: 'Bearer ' + 'c'.repeat(30), sk: 'sk-' + 'b'.repeat(24), gl: 'glpat-' + 'g'.repeat(24) };
  const all = Object.values(tok).join(' ');
  const r = row('back@' + hex12(1), 'raw', { p: 'P1', rank: 0, pending: false }, {
    meta: { title: '<img src=x onerror=alert(1)> ' + all + '\u001b[31m', summary: '**bold** <b>x</b> ' + all + '‮', source: 'sidecar', kind: all, area: tok.ghp, size: tok.sk },
    status: 'open ' + tok.ghp,
  });
  const v = G.rowView(r, new Set());
  const strings = [v.title, v.summary, ...v.chips];
  for (const s of strings) for (const t of Object.values(tok)) assert.ok(!s.includes(t), 'leaked ' + t + ' in ' + s);
  assert.match(v.title, /^<img src=x onerror=alert\(1\)> /); // text, not markup: the DOM code uses textContent
  assert.match(v.title, /<U\+001B>\[31m$/);
  assert.match(v.summary, /^\*\*bold\*\* <b>x<\/b> .*<U\+202E>$/);
  assert.ok(v.chips.length === 4 && v.chips.every((c) => !/ghp_|sk-/.test(c)));
  // the filter sees meta text too, but never enrich.summary
  const b = fixtureV2();
  b.items[4].meta.summary = 'zebra-unique summary';
  b.items[5].meta.title = 'quokka-unique title';
  assert.deepStrictEqual(G.filter(b.items, 'zebra-unique').map((r) => r.key), [b.items[4].key]);
  assert.deepStrictEqual(G.filter(b.items, 'quokka-unique').map((r) => r.key), [b.items[5].key]);
  assert.strictEqual(G.filter(b.items, 'ENRICH SUMMARY').length, 0);
  assert.strictEqual(G.filter(b.items, 'fallback').length, 0);
  assert.strictEqual(G.filter(b.items, 'bug').length, b.items.length);
});

test('shed rows: no body head, a visible shed mark, the source link kept', () => {
  const b = fixtureV2();
  const shed = G.rowView(b.items[1], new Set());
  assert.strictEqual(shed.shed, true);
  assert.strictEqual(shed.bodyHead, '');
  assert.strictEqual(shed.bodyUrl, b.items[1].body_url);
  const kept = G.rowView(b.items[3], new Set());
  assert.strictEqual(kept.shed, false);
  assert.ok(kept.bodyHead.length > 0);
  // only the boolean true counts
  assert.strictEqual(G.rowView(row('back@' + hex12(1), 't', null, { body_head_shed: 'true' }), new Set()).shed, false);
  assert.strictEqual(G.rowView(row('back@' + hex12(1), 't', null, { body_head: '', body_head_shed: true }), new Set()).shed, true);
});

test('producer budget is separate from the wire estimate: both can be flagged independently', () => {
  const quiet = G.section({ ga_todo: fixtureV2() }, NOW);
  assert.strictEqual(quiet.producerOver, false);
  assert.strictEqual(quiet.overBudget, false);
  assert.deepStrictEqual(quiet.budget, { target: 80000, items: 53248, shed: 1, over: false });
  assert.strictEqual(quiet.header, 'GA TODO (31 + 265 more)');
  // producer over, wire estimate fine
  const p = G.section({ ga_todo: fixtureV2({ budget: { target_bytes: 80000, items_bytes: 98765, shed: 31, over: true }, payload_bytes: 1000 }) }, NOW);
  assert.strictEqual(p.producerOver, true);
  assert.strictEqual(p.overBudget, false);
  assert.match(p.header, / · over size budget \(99 KB\)$/);
  assert.strictEqual(p.rows.length, 31, 'over budget never drops rows');
  // wire estimate over, producer fine
  const w = G.section({ ga_todo: fixtureV2({ payload_bytes: 90000, payload_budget: 81920 }) }, NOW);
  assert.strictEqual(w.overBudget, true);
  assert.strictEqual(w.producerOver, false);
  assert.ok(!/budget/.test(w.header));
  // no budget (schema 1) or a malformed one: nothing invented
  assert.strictEqual(G.section({ ga_todo: fixture() }, NOW).budget, null);
  for (const bd of ['x', 7, [], { over: 'true', shed: -1, items_bytes: 1.5 }]) {
    const s = G.section({ ga_todo: fixtureV2({ budget: bd }) }, NOW);
    assert.strictEqual(s.producerOver, false, JSON.stringify(bd));
    assert.strictEqual(s.rows.length, 31);
  }
});

test('size units: producer budget is decimal KB rounded up, the wire estimate is KiB; exactly 80,001 bytes is over', () => {
  assert.strictEqual(G.kb(80000), '80 KB');
  assert.strictEqual(G.kb(80001), '81 KB');
  assert.strictEqual(G.kb(0), '0 KB');
  assert.strictEqual(G.kb(1), '1 KB');
  assert.strictEqual(G.kib(81920), '80 KiB');
  assert.strictEqual(G.kib(80000), '78 KiB');
  const at = G.section({ ga_todo: fixtureV2({ budget: { target_bytes: 80000, items_bytes: 80000, shed: 0, over: false } }) }, NOW);
  assert.strictEqual(at.producerOver, false);
  assert.ok(!/budget/.test(at.header));
  assert.strictEqual(at.budget.items, 80000);
  // GA reports over only when even the all-shed baseline exceeds the target: 80,001 bytes
  const over = G.section({ ga_todo: fixtureV2({ budget: { target_bytes: 80000, items_bytes: 80001, shed: 31, over: true } }) }, NOW);
  assert.strictEqual(over.producerOver, true);
  assert.match(over.header, / · over size budget \(81 KB\)$/); // never "80 KB" for an over-budget block
  assert.strictEqual(over.rows.length, 31);
});

test('shed line wording: singular and plural', () => {
  assert.strictEqual(G.shedText(1), '1 row trimmed to its source link');
  assert.strictEqual(G.shedText(2), '2 rows trimmed to their source link');
  assert.strictEqual(G.shedText(31), '31 rows trimmed to their source link');
});

test('the tower_model.py refusal marker (unsupported:true) is honored even with version 2 and items []', () => {
  const refusal = (over) => Object.assign({ schema_version: 2, items: [], pending: [], stale: true, unsupported: true, more: 0,
    error: 'unsupported GA TODO block: the full view ghp_' + 'A'.repeat(30), payload_bytes: 0, payload_budget: 81920 }, over || {});
  const sec = G.section({ ga_todo: refusal() }, NOW);
  assert.strictEqual(sec.header, 'GA TODO · UNSUPPORTED');
  assert.deepStrictEqual(sec.rows, []);
  assert.strictEqual(sec.stale, sec.unsupported);
  assert.strictEqual(sec.unsupported, 'unsupported GA TODO block: the full view [redacted]'); // the visible reason, redacted
  assert.strictEqual(G.unsupportedReason(refusal({ error: '' })), 'unsupported GA TODO block');
  assert.strictEqual(G.unsupportedReason(refusal({ error: undefined })), 'unsupported GA TODO block');
  // the marker wins over everything else: version 1, rows present, fresh, not stale
  const rows = fixtureV2().items;
  for (const v of [1, 2]) {
    const s = G.section({ ga_todo: refusal({ schema_version: v, items: rows, stale: false, built_at: '2026-11-25T11:00:00Z' }) }, NOW);
    assert.ok(s.unsupported && s.rows.length === 0, 'v' + v);
  }
  // only the boolean true is a marker
  for (const m of [false, 'true', 1, null]) {
    assert.strictEqual(G.unsupportedReason(Object.assign(fixtureV2(), { unsupported: m })), '', String(m));
  }
});

test('unknown item fields and a future budget field never break a v2 row or the selection', () => {
  const b = fixtureV2();
  b.items.forEach((r) => { r.future = { deep: [1, { x: 'y' }] }; });
  b.budget.future = 'x';
  const sec = G.section({ ga_todo: b }, NOW);
  assert.strictEqual(sec.rows.length, 31);
  assert.strictEqual(sec.unsupported, '');
  assert.strictEqual(G.rowView(sec.rows[0], new Set()).title, 'Meta title 0');
});

test('no ga_todo block (an older tower): no section', () => {
  assert.strictEqual(G.section({ sections: [] }, NOW), null);
  assert.strictEqual(G.section(null, NOW), null);
});

test('stale / refused snapshots are visible and block actions', () => {
  const sec = (patch) => G.section({ ga_todo: Object.assign(fixture(), patch) }, NOW);
  assert.strictEqual(sec({}).stale, '');
  assert.match(sec({ built_at: '2026-11-25T09:59:00Z' }).stale, /older than 120 minutes/);
  assert.match(sec({ built_at: '2026-11-25T12:05:00Z' }).stale, /future/);
  assert.match(sec({ built_at: 'nope' }).stale, /built_at/);
  assert.match(sec({ stale: true }).stale, /stale/);
  assert.match(sec({ error: 'feed refused ghp_' + 'A'.repeat(30) }).stale, /^feed refused \[redacted\]$/);
  assert.strictEqual(sec({ schema_version: 2 }).stale, ''); // feed schema 2 is a strict superset of 1
  assert.match(sec({ stale: true }).header, / · STALE$/);
});

test('all-string redaction: tokens and control characters never reach the DOM strings', () => {
  const tok = { ghp: 'ghp_' + 'A'.repeat(30), sk: 'sk-' + 'b'.repeat(24), bearer: 'Bearer ' + 'c'.repeat(30), jwt: 'eyJ' + 'd'.repeat(24) + '.' + 'e'.repeat(12),
    vault: 'hvs.' + 'f'.repeat(24), gl: 'glpat-' + 'g'.repeat(24), slack: 'xoxb-' + '1'.repeat(24), aws: 'AKIA' + 'Z'.repeat(16) };
  const all = Object.values(tok).join(' ');
  const r = row('back@' + hex12(1), 'title ' + all + '\u001b[31m', { p: 'P1', rank: 0, pending: false },
    { section: 'sec ' + tok.ghp, body_head: 'head\n' + all + '‮', w_key: 'back:1@' + hex12(1), parent: 'back@' + hex12(2) });
  const v = G.rowView(r, new Set());
  for (const s of Object.values(v).filter((x) => typeof x === 'string')) {
    for (const t of Object.values(tok)) assert.ok(!s.includes(t), 'leaked ' + t + ' in ' + s);
  }
  assert.match(v.title, /<U\+001B>\[31m$/);
  assert.match(v.bodyHead, /^head\n.*<U\+202E>$/s);
  assert.strictEqual(G.redact('ask-' + 'b'.repeat(24)), 'ask-' + 'b'.repeat(24)); // sk- only as its own word
  assert.strictEqual(G.redact('x sk-' + 'b'.repeat(24)), 'x [redacted]');
  const pv = G.pendingView({ pending: [{ id: 'pick-gatodo-x', key: tok.ghp, mode: 'design' }] });
  assert.ok(!JSON.stringify(pv).includes(tok.ghp));
});

test('row view: band/pending marks, safe links only, startable doc states, queued keys', () => {
  const b = fixture();
  const q = G.queuedKeys(b, new Set(['todo@' + hex12(0x102)]));
  assert.deepStrictEqual([...q].sort(), ['back@' + hex12(1), 'todo@' + hex12(0x101), 'todo@' + hex12(0x102)].sort());
  const v = G.rowView(b.items[5], q);
  assert.strictEqual(v.band, 'P1');
  assert.strictEqual(v.pending, true);
  assert.strictEqual(G.rowView(b.items[0], q).pending, false);
  assert.strictEqual(G.rowView(b.items[6], q).band, '—');
  assert.strictEqual(G.rowView(b.items[1], q).queued, true);
  assert.strictEqual(v.where, 'docs/TODO-backlog.md:10–14');
  assert.strictEqual(v.bodyUrl, b.items[5].body_url);
  assert.strictEqual(G.rowView(row('back@' + hex12(1), 't', null, { body_url: 'javascript:alert(1)' })).bodyUrl, '');
  assert.strictEqual(G.rowView(row('back@' + hex12(1), 't', null, { body_url: 'file:///etc/passwd' })).bodyUrl, '');
  for (const [state, ok] of [['open', true], ['shipped-marked', true], ['checked', false], ['struck', false], ['container', false]]) {
    assert.strictEqual(G.rowView(row('back@' + hex12(1), 't', null, { doc_state: state })).startable, ok, state);
  }
  assert.strictEqual(G.rowView(b.items[6]).title.length <= 160, true);
});

test('filter: all words, over key/title/section/body/band', () => {
  const rows = fixture().items;
  assert.strictEqual(G.filter(rows, '').length, rows.length);
  assert.deepStrictEqual(G.filter(rows, 'other 2').map((r) => r.title).slice(0, 2), ['other 2', 'other 12']);
  assert.strictEqual(G.filter(rows, 'p0').length, 1);
  assert.deepStrictEqual(G.filter(rows, 'back@' + hex12(9)).map((r) => r.key), ['back@' + hex12(9)]);
  assert.strictEqual(G.filter(rows, 'nothing-matches').length, 0);
});

test('action payloads: exact key, mode, band, view_sha; titles are never sent', () => {
  const b = fixture();
  const r = b.items[0];
  assert.deepStrictEqual(G.startBody(r, 'implement', b), { op: 'gatodo_start', key: r.key, mode: 'implement', view_sha: SHA, body_sha: '00112233445566ff' });
  assert.throws(() => G.startBody(r, 'ship', b), /mode/);
  assert.deepStrictEqual(G.prioBody(r, 'P2', b), { op: 'gatodo_prio', key: r.key, p: 'P2', view_sha: SHA });
  assert.deepStrictEqual(G.prioBody(r, null, b), { op: 'gatodo_prio', key: r.key, p: null, view_sha: SHA });
  assert.throws(() => G.prioBody(r, 'P4', b), /priority/);
  assert.ok(!('title' in G.startBody(r, 'design', b)));
});

test('move within a band (J/K): after = new predecessor, null at the top; P3 and edges refuse', () => {
  const b = fixture();
  const rows = G.section({ ga_todo: b }, NOW).rows;
  const p1 = G.bandRows(rows, 'P1').map((r) => r.key);
  assert.deepStrictEqual(p1, ['back@' + hex12(1), 'back@' + hex12(2), 'back@' + hex12(9)]);
  const byKey = Object.fromEntries(rows.map((r) => [r.key, r]));
  assert.deepStrictEqual(G.moveBody(byKey[p1[1]], -1, rows, b), { op: 'gatodo_prio', key: p1[1], p: 'P1', after: null, view_sha: SHA });
  assert.deepStrictEqual(G.moveBody(byKey[p1[0]], +1, rows, b), { op: 'gatodo_prio', key: p1[0], p: 'P1', after: p1[1], view_sha: SHA });
  assert.deepStrictEqual(G.moveBody(byKey[p1[1]], +1, rows, b), { op: 'gatodo_prio', key: p1[1], p: 'P1', after: p1[2], view_sha: SHA });
  assert.strictEqual(G.moveBody(byKey[p1[0]], -1, rows, b), null);
  assert.strictEqual(G.moveBody(byKey[p1[2]], +1, rows, b), null);
  assert.strictEqual(G.moveBody(rows.find((r) => G.band(r) === 'P3'), -1, rows, b), null); // incomplete band in the bounded block
  assert.strictEqual(G.moveBody(rows.find((r) => !G.band(r)), -1, rows, b), null);
});

test('start request data: tower ga_todo.start_request with from:"phone" (TestGatodoStartJournalsExactKeyRequest)', () => {
  const b = fixture();
  const r = Object.assign({}, b.items[0], { w_key: 'back:12@' + hex12(0) });
  assert.deepStrictEqual(Object.keys(G.startRequestData(r, 'verify', b)), ['key', 'w_key_seen', 'mode', 'title', 'doc', 'body_sha', 'view_sha', 'from', 'text']);
  assert.deepStrictEqual(G.startRequestData(r, 'verify', b), { key: r.key, w_key_seen: 'back:12@' + hex12(0), mode: 'verify', title: 'prio P0 0',
    doc: 'docs/TODO-backlog.md', body_sha: '00112233445566ff', view_sha: SHA, from: 'phone', text: 'GA TODO verify: prio P0 0 (' + r.key + ')' });
  assert.strictEqual(G.startRequestData(b.items[1], 'design', b).w_key_seen, null);
});

test('pending requests keep journal order and show after semantics (end / top / anchor / clear)', () => {
  const pv = G.pendingView(fixture());
  assert.deepStrictEqual(pv.map((p) => p.kind), ['priority', 'start', 'priority']);
  assert.deepStrictEqual(pv.map((p) => p.text), [
    'P1 back@' + hex12(9),
    'design back@' + hex12(1),
    'P0 back@' + hex12(2) + ' (top) · P2 back@' + hex12(3) + ' after back@' + hex12(4) + ' · clear todo@' + hex12(0x100),
  ]);
  assert.deepStrictEqual(G.pendingView({ pending: [{ id: 'ga-prio-x', edits: 'bad' }] }), [{ id: 'ga-prio-x', kind: 'priority', text: '(no edits)' }]);
});
