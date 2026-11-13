/* Read-only child summaries. No delivery, permission, or navigation actions. */
(function (root) {
  'use strict';
  const fresh = (s, now = Date.now()) => s && s.availability === 'available' && s.complete &&
    Number.isFinite(Date.parse(s.freshUntil)) && now < Date.parse(s.freshUntil);
  function badge(s, now = Date.now()) {
    if (!s) return '';
    if (!fresh(s, now)) return s.running > 0 ? `${s.running} last seen active · status stale` : 'Subagent status unavailable';
    if (s.running > 0) return `${s.running} subagent${s.running === 1 ? '' : 's'} running`;
    if (s.unknown > 0) return `${s.unknown} subagent${s.unknown === 1 ? '' : 's'} · status unknown`;
    return ''; // verified zero is retained in data; unsupported has no data
  }
  function rowText(child, now = Date.now()) {
    const at = Date.parse(child.evidenceAt);
    const age = Number.isFinite(at) && at > 0 ? Math.max(0, Math.floor((now - at) / 1000)) : null;
    const isFresh = child.freshness === 'fresh' && age !== null && age < 30;
    const status = child.status === 'running' && !isFresh ? 'unknown' : child.status;
    return [child.label || 'Agent', status || 'unknown', child.lastAction && child.lastAction.name,
      isFresh ? 'fresh evidence' : 'status stale', age === null ? '' : `${age}s since evidence`,
      !child.linked ? 'Unlinked child · parent call unavailable' : '',
      child.availability !== 'available' ? 'source unavailable' : ''].filter(Boolean).join(' · ');
  }
  function mount(doc) {
    const panel = doc.createElement('details'); panel.className = 'subagent-panel'; panel.id = 'subagentPanel';
    const summary = doc.createElement('summary'); summary.textContent = 'Subagents · read-only'; panel.append(summary);
    const note = doc.createElement('div'); note.className = 'subagent-note'; note.textContent = 'Loading child summaries…'; panel.append(note);
    const rows = doc.createElement('div'); rows.className = 'subagent-rows'; panel.append(rows);
    return panel;
  }
  function render(panel, data, now = Date.now()) {
    const rows = panel.querySelector('.subagent-rows');
    const note = panel.querySelector('.subagent-note');
    panel.querySelector('summary').textContent = 'Subagents · read-only' + (data ? ` · ${data.children.length}` : '');
    note.textContent = !data ? 'Child status unavailable' :
      [data.availability !== 'available' ? 'Child source unavailable' : '',
       !data.complete || data.truncated ? 'Partial discovery · counts may be incomplete' : '',
       data.nextCursor ? `First ${data.children.length} children shown` : '',
       !data.children.length && data.complete ? 'No direct subagents found' : ''].filter(Boolean).join(' · ');
    const old = new Map([...rows.children].map(row => [row.dataset.childId, row]));
    const keep = new Set();
    for (const child of data ? data.children : []) {
      if (keep.has(child.id)) continue;
      keep.add(child.id);
      let row = old.get(child.id);
      if (!row) { row = panel.ownerDocument.createElement('div'); row.className = 'subagent-row'; row.dataset.childId = child.id; rows.append(row); }
      const text = rowText(child, now);
      if (row.textContent !== text) row.textContent = text; // never HTML, even for malicious labels
    }
    for (const [id, row] of old) if (!keep.has(id)) row.remove();
    // Existing nodes, details.open, and list scroll position survive refresh.
  }
  const api = { fresh, badge, rowText, mount, render };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckSubagents = api;
})(typeof globalThis !== 'undefined' ? globalThis : this);
