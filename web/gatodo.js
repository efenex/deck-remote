// Tower tab GA TODO section (S6, GA TODO-view report §4): pure helpers, no DOM. Loaded before
// app.js; the same file is required by node --test (gatodo.test.js). Modeled on backlog.js.
//
// The rows are overview.json's bounded `ga_todo` block (tower_model.py passes it through), feed schema 1
// or 2 (2 is a strict superset: item.meta{title,summary,source,stale?,kind?,area?,size?}, item.status,
// item.body_head_shed, block.budget): every explicit P0-P2 row plus at most 25 others, `more` for the
// rest. Anything else (another version, the TUI's full view `units`, no item list) is refused visibly:
// no rows, no actions. Fields are chosen by presence, so an unknown field never breaks a row. The
// producer's `budget` (compact items bytes vs its 80 KB target) is shown apart from `payload_bytes`
// (tower_model.py's estimate of this block on the wire). Actions POST /api/rc/tower/act:
//   start     {op:'gatodo_start', key, mode: design|implement|verify, view_sha, body_sha}
//   priority  {op:'gatodo_prio', key, p: 'P0'..'P3' | null, [after: null | '<key>'], view_sha}
// and the server (tower_actions.go) journals the same request records as the tower TUI
// (pick-gatodo-<mode>-… / ga-prio-…, from:"phone") after validating against the current model.
(function (root) {
  'use strict';

  const SCHEMAS = [1, 2];
  const BANDS = ['P0', 'P1', 'P2', 'P3'];
  const EXPLICIT = ['P0', 'P1', 'P2'];
  const OTHERS_MAX = 25;
  const STALE_MS = 120 * 60 * 1000;
  const SKEW_MS = 60 * 1000;
  const MODES = { design: 'Design', implement: 'Implement', verify: 'Verify' };

  // tower scripts/ga_todo.py REDACT_RE (overview.py and tower_model.py already redact; the tab
  // redacts again before anything is drawn). (?i:bearer) spelled out: no inline flags in older JS.
  // sk-: "not after a letter/digit" without lookbehind (older WebKit), via the captured prefix.
  const REDACT = /(hvs\.[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[bpa]-[A-Za-z0-9-]{20,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}|[Bb][Ee][Aa][Rr][Ee][Rr]\s+[A-Za-z0-9._-]{24,})/g;
  const REDACT_SK = /(^|[^A-Za-z0-9])sk-[A-Za-z0-9_-]{20,}/g;
  // control / format / line+paragraph separators, a line feed and a tab excepted (ga_todo.clean)
  const HIDDEN = /[^\P{Cc}\n\t]|\p{Cf}|\p{Zl}|\p{Zp}/gu;

  function redact(s) {
    return String(s == null ? '' : s).replace(REDACT, '[redacted]').replace(REDACT_SK, '$1[redacted]')
      .replace(HIDDEN, (c) => '<U+' + c.codePointAt(0).toString(16).toUpperCase().padStart(4, '0') + '>');
  }

  function band(row) {
    const p = row && row.priority;
    return p && BANDS.includes(p.p) ? p.p : null;
  }

  // select: all explicit P0-P2 rows (never capped), then at most 25 others (no band, or P3), in
  // the block's order. cut: rows dropped here (on top of the block's own `more`).
  function select(items) {
    const rows = [];
    let others = 0, cut = 0;
    (Array.isArray(items) ? items : []).forEach((r) => {
      if (!r || typeof r.key !== 'string' || !r.key) return;
      if (!EXPLICIT.includes(band(r))) {
        if (others >= OTHERS_MAX) { cut++; return; }
        others++;
      }
      rows.push(r);
    });
    return { rows, cut };
  }

  // Why this block cannot be shown at all ('' = supported). schema_version is strict: the number 1 or 2
  // (a string "2" or a missing version is refused); the full view and a missing/non-list `items` too.
  function unsupportedReason(block) {
    if (!block || typeof block !== 'object') return 'no GA TODO data';
    // tower_model.py's refusal marker: honored even when the refusal itself carries a supported-looking version and items []
    if (block.unsupported === true) return redact(block.error) || 'unsupported GA TODO block';
    if (!SCHEMAS.includes(block.schema_version)) return 'unsupported schema_version (phone reads schema 1 or 2 only)';
    if ('units' in block || !Array.isArray(block.items)) return 'unsupported GA TODO block (full view or no item list; phone reads the bounded feed only)';
    return '';
  }

  // Why the snapshot may not be acted on ('' = fresh): the server re-checks the same at act time.
  function staleReason(block, now) {
    const bad = unsupportedReason(block);
    if (bad) return bad;
    if (block.error) return redact(block.error);
    if (block.stale) return 'snapshot stale';
    const t = Date.parse(block.built_at || '');
    const n = now == null ? Date.now() : now;
    if (isNaN(t)) return 'no valid built_at';
    if (t > n + SKEW_MS) return 'built_at in the future';
    if (n - t > STALE_MS) return 'snapshot older than 120 minutes';
    return '';
  }

  function safeUrl(u) {
    const s = String(u || '');
    return /^https?:\/\/[^\s]+$/i.test(s) ? s : '';
  }

  function truncate(s, max) {
    const t = String(s || '').replace(/\s+/g, ' ').trim();
    return t.length > max ? t.slice(0, max - 1) + '…' : t;
  }

  // pick-gatodo-* keys open in the block (any mode: the exact key is the task) plus keys posted
  // from this tab; and in-flight ga_keys.
  function queuedKeys(block, local) {
    const out = new Set();
    ((block && block.pending) || []).forEach((r) => {
      if (r && String(r.id || '').startsWith('pick-gatodo-') && r.key) out.add(r.key);
    });
    ((block && block.in_flight_keys) || []).forEach((k) => out.add(k));
    if (local) local.forEach((k) => out.add(k));
    return out;
  }

  // v2 row text, chosen by presence. title: meta.title (schema 2; non-empty) else the raw heading. summary:
  // meta.summary only, which MAY be "" (a fallback row without a derived summary); enrich.summary is a
  // different cache slot (LLM enrichment) and is never shown in its place.
  function metaOf(row) {
    return row && row.meta && typeof row.meta === 'object' && !Array.isArray(row.meta) ? row.meta : null;
  }
  function titleOf(row) {
    const m = metaOf(row);
    return m && typeof m.title === 'string' && m.title.trim() ? m.title : row.title;
  }
  function summaryOf(row) {
    const m = metaOf(row);
    return m && typeof m.summary === 'string' ? m.summary : null; // null: no summary slot (v1 row)
  }

  // Sizes. The producer budget is decimal (80,000 bytes = "80 KB", like tower's heading; 80,001 rounds UP to "81 KB",
  // so an over-budget block never reads as "80 KB"). The wire estimate is binary: "KiB", 1024 bytes.
  const kb = (n) => Math.ceil(Number(n) / 1000) + ' KB';
  const kib = (n) => Math.round(Number(n) / 1024) + ' KiB';

  const shedText = (n) => (n === 1 ? '1 row trimmed to its source link' : n + ' rows trimmed to their source link');

  // The producer's size budget (block.budget), normalized; null without one (schema 1).
  function budgetView(block) {
    const bd = block && block.budget;
    if (!bd || typeof bd !== 'object' || Array.isArray(bd)) return null;
    const n = (x) => (Number.isInteger(x) && x >= 0 ? x : null);
    return { target: n(bd.target_bytes), items: n(bd.items_bytes), shed: n(bd.shed) || 0, over: bd.over === true };
  }

  // The display shape of one row: everything the DOM code prints, every string redacted.
  function rowView(row, queued) {
    const b = band(row);
    const p = row.priority || {};
    const m = metaOf(row);
    const sum = summaryOf(row);
    const chips = [];
    if (typeof row.status === 'string' && row.status) chips.push(row.status);
    if (m) ['kind', 'area', 'size'].forEach((k) => { if (typeof m[k] === 'string' && m[k]) chips.push(m[k]); });
    return {
      key: redact(row.key),
      band: b || '—',
      pending: !!(b && p.pending),
      title: truncate(redact(titleOf(row)), 160),
      hasSummary: sum !== null,
      summary: sum === null ? '' : truncate(redact(sum), 480),
      chips: chips.map((c) => truncate(redact(c), 40)),
      derived: !!(m && m.source === 'fallback'),
      metaStale: !!(m && m.stale === true),
      shed: row.body_head_shed === true,
      section: truncate(redact(row.section), 80),
      state: redact(row.doc_state),
      where: redact(row.doc) + ':' + row.line_start + (row.line_end && row.line_end !== row.line_start ? '–' + row.line_end : ''),
      wkey: row.w_key ? redact(row.w_key) : '',
      parent: row.parent ? redact(row.parent) : '',
      bodyHead: redact(row.body_head),
      bodyUrl: safeUrl(row.body_url),
      startable: row.doc_state === 'open' || row.doc_state === 'shipped-marked',
      queued: !!(queued && queued.has(row.key)),
    };
  }

  // The section the tab draws; null when the overview has no ga_todo block (an older tower).
  function section(model, now) {
    const block = model && model.ga_todo;
    if (!block || typeof block !== 'object') return null;
    const unsupported = unsupportedReason(block);
    if (unsupported) { // visible refusal: nothing from the block is drawn or acted on
      return { title: 'GA TODO', block, rows: [], more: 0, stale: unsupported, unsupported, budget: null, overBudget: false, producerOver: false,
        header: 'GA TODO · UNSUPPORTED' };
    }
    const sel = select(block.items);
    const more = (Number.isInteger(block.more) && block.more > 0 ? block.more : 0) + sel.cut;
    const stale = staleReason(block, now);
    const budget = budgetView(block);
    const producerOver = !!(budget && budget.over);
    return {
      title: 'GA TODO', block, rows: sel.rows, more, stale, unsupported: '', budget, producerOver,
      header: 'GA TODO (' + sel.rows.length + (more ? ' + ' + more + ' more' : '') + ')' + (stale ? ' · STALE' : '') +
        (producerOver ? ' · over size budget' + (budget.items != null ? ' (' + kb(budget.items) + ')' : '') : ''),
      // overBudget: this block on the phone's wire (tower_model.py's estimate); producerOver: GA's own
      // compact-items budget. Two different measures, never merged.
      overBudget: Number(block.payload_bytes) > Number(block.payload_budget || Infinity),
    };
  }

  // filter: every whitespace-separated word must appear in the row's key/title/section/body.
  function filter(rows, q) {
    const words = String(q || '').toLowerCase().split(/\s+/).filter(Boolean);
    if (!words.length) return rows;
    return rows.filter((r) => {
      const m = metaOf(r) || {};
      const hay = [r.key, r.title, titleOf(r), summaryOf(r) || '', m.kind, m.area, m.size, r.status, r.section, r.doc_state, r.body_head, band(r) || '']
        .filter((x) => typeof x === 'string').join(' ').toLowerCase();
      return words.every((w) => hay.includes(w));
    });
  }

  // --- act bodies ----------------------------------------------------------------------------
  function startBody(row, mode, block) {
    if (!MODES[mode]) throw new Error('mode must be design, implement or verify');
    return { op: 'gatodo_start', key: row.key, mode, view_sha: block.source_sha, body_sha: row.body_sha };
  }

  // p: 'P0'..'P3' (appended at the end of that band), or null to clear.
  function prioBody(row, p, block) {
    if (p !== null && !BANDS.includes(p)) throw new Error('priority must be P0..P3 or clear');
    return { op: 'gatodo_prio', key: row.key, p, view_sha: block.source_sha };
  }

  // The rows of one band in rank order.
  function bandRows(rows, p) {
    return rows.filter((r) => band(r) === p).sort((a, b) => (a.priority.rank || 0) - (b.priority.rank || 0));
  }

  // Move a row one place up (-1) or down (+1) within its band, the TUI's J/K: {p, after} where
  // after is the new predecessor, or null for the top. Only P0-P2: those bands are complete in the
  // bounded block; P3 is not (its hidden rows would make the anchor wrong). null when impossible.
  function moveBody(row, dir, rows, block) {
    const p = band(row);
    if (!EXPLICIT.includes(p)) return null;
    const keys = bandRows(rows, p).map((r) => r.key);
    const old = keys.indexOf(row.key);
    const nu = old + (dir > 0 ? 1 : -1);
    if (old < 0 || nu < 0 || nu >= keys.length) return null;
    keys.splice(old, 1);
    keys.splice(nu, 0, row.key);
    return { op: 'gatodo_prio', key: row.key, p, after: nu ? keys[nu - 1] : null, view_sha: block.source_sha };
  }

  // The journal data the server writes for a start (tower ga_todo.start_request, from:"phone"):
  // documented and asserted in tests; the server builds it from its own model.
  function startRequestData(row, mode, block) {
    return { key: row.key, w_key_seen: row.w_key || null, mode, title: row.title, doc: row.doc, body_sha: row.body_sha,
      view_sha: block.source_sha, from: 'phone', text: 'GA TODO ' + mode + ': ' + row.title + ' (' + row.key + ')' };
  }

  // Pending GA requests in journal order (the block keeps tower's order): what tower will apply.
  function pendingView(block) {
    return ((block && block.pending) || []).filter((r) => r && typeof r.id === 'string').map((r) => {
      if (r.id.startsWith('pick-gatodo-')) return { id: redact(r.id), kind: 'start', text: redact((r.mode || '?') + ' ' + (r.key || '?')) };
      const edits = Array.isArray(r.edits) ? r.edits : [];
      const text = edits.map((e) => {
        if (!e || typeof e !== 'object') return '?';
        if (e.p == null) return 'clear ' + e.key;
        return e.p + ' ' + e.key + (!('after' in e) ? '' : e.after == null ? ' (top)' : ' after ' + e.after);
      }).join(' · ');
      return { id: redact(r.id), kind: 'priority', text: redact(text || '(no edits)') };
    });
  }

  const api = { kb, kib, shedText, SCHEMAS, BANDS, MODES, OTHERS_MAX, redact, band, select, unsupportedReason, staleReason, titleOf, summaryOf, budgetView, safeUrl, queuedKeys, rowView, section, filter,
    startBody, prioBody, moveBody, bandRows, startRequestData, pendingView };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckGatodo = api;
})(typeof window !== 'undefined' ? window : globalThis);
