// Tower tab BACKLOG section (tower overview-backlog): pure helpers, no DOM. Loaded before
// app.js; the same file is required by node --test (backlog.test.js).
//
// The rows come from overview.json's `backlog` (tower_model.py passes it through). Each row
// offers start / defer / drop, which POST /api/rc/tower/act {op: backlog_*, id, not_before?}.
// The server (tower_actions.go) journals one open request record per action, the same kind
// of record a picked suggestion writes:
//   start  pick-backlog-<slug(id)>  {"task": id}
//   defer  pick-defer-<slug(id)>    {"task": id, "not_before": "<YYYY-MM-DD>T08:00:00Z"}
//   drop   pick-drop-<slug(id)>     {"task": id}
(function (root) {
  'use strict';

  // slug: tower_actions.go towerSlug (the pick-<slug> request ids), byte for byte on ASCII ids.
  function slug(id) {
    let s = String(id == null ? '' : id).toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
    if (s.length > 60) s = s.slice(0, 60);
    return s || 'x';
  }

  const ACTIONS = {
    start: { op: 'backlog_start', prefix: 'pick-backlog-', label: 'Start' },
    defer: { op: 'backlog_defer', prefix: 'pick-defer-', label: 'Defer…' },
    drop: { op: 'backlog_drop', prefix: 'pick-drop-', label: 'Drop' },
  };

  function requestId(action, id) { return ACTIONS[action].prefix + slug(id); }
  function requestIds(id) { return Object.keys(ACTIONS).map((a) => requestId(a, id)); }

  // The tower request record each action produces (what the server journals; shown in tests and
  // used to recognise a row tower already has queued).
  function request(action, id, date) {
    const data = { task: id };
    if (action === 'defer') data.not_before = notBefore(date);
    return { id: requestId(action, id), data };
  }

  // What the tab POSTs to /api/rc/tower/act.
  function actBody(action, id, date) {
    const b = { op: ACTIONS[action].op, id };
    if (action === 'defer') b.not_before = notBefore(date);
    return b;
  }

  const DATE_RE = /^\d{4}-\d{2}-\d{2}$/;
  function notBefore(date) {
    if (!DATE_RE.test(String(date || '')) || isNaN(Date.parse(date + 'T08:00:00Z'))) throw new Error('pick a date (YYYY-MM-DD)');
    return date + 'T08:00:00Z';
  }

  // Tomorrow as YYYY-MM-DD in the phone's local calendar (the default of the defer picker).
  function tomorrow(now) {
    const d = new Date(now == null ? Date.now() : now);
    d.setDate(d.getDate() + 1);
    const p = (n) => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
  }

  // Open request ids in the model: overview requests still open (open_requests) and the
  // model's own request items that are not done.
  function openRequestIds(model) {
    const out = new Set();
    ((model && model.open_requests) || []).forEach((id) => out.add(id));
    ((model && model.sections) || []).forEach((sec) => (sec.items || []).forEach((it) => {
      if (it && it.kind === 'request' && it.status !== 'done') out.add(it.id);
    }));
    return out;
  }

  function isQueued(id, open, local) {
    return requestIds(id).some((r) => (open && open.has(r)) || (local && local.has(r)));
  }

  function ageText(row, now) {
    let d = row && row.age_days;
    if (d == null && row && row.created) {
      const t = Date.parse(row.created);
      if (!isNaN(t)) d = ((now == null ? Date.now() : now) - t) / 86400000;
    }
    if (d == null || isNaN(Number(d))) return '? d';
    return Math.max(0, Math.floor(Number(d))) + ' d';
  }

  function truncate(s, max) {
    const t = String(s || '').replace(/\s+/g, ' ').trim();
    return t.length > max ? t.slice(0, max - 1) + '…' : t;
  }

  // The display shape of one row: everything the DOM code prints.
  function rowView(row, now) {
    const deps = Array.isArray(row.deps_open) ? row.deps_open.filter(Boolean) : [];
    return {
      age: ageText(row, now),
      id: String(row.id),
      goal: row.goal || '',
      title: truncate(row.title, 90),
      waits: deps.length ? 'waits on ' + deps.join(', ') : '',
      notBefore: row.not_before ? 'not before ' + String(row.not_before).slice(0, 10) : '',
    };
  }

  // null when the overview has no backlog (an older tower): the tab shows no section.
  function section(model) {
    if (!model || !Array.isArray(model.backlog)) return null;
    const c = model.counts && model.counts.backlog;
    const n = c != null && !isNaN(Number(c)) ? Number(c) : model.backlog.length;
    return { title: 'BACKLOG', count: n, header: 'BACKLOG (' + n + ')', rows: model.backlog.filter((r) => r && r.id) };
  }

  const api = { slug, ACTIONS, requestId, requestIds, request, actBody, notBefore, tomorrow, openRequestIds, isQueued, ageText, rowView, section };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckBacklog = api;
})(typeof window !== 'undefined' ? window : globalThis);
