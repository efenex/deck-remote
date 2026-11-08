// Tower tab reply drafts (G15): typed answers must survive every overview refresh.
// Pure helpers, no DOM of their own (elements are duck-typed). Loaded before app.js; the same
// file is required by node --test (towerdraft.test.js).
//
// renderTower rebuilds the whole list, so each refresh made a fresh <textarea> per ask and the
// operator's half-typed reply vanished. The rules, all of them here or wired from app.js:
//   1. a draft is kept per ask id (store: memory + localStorage, so a reload / PWA resume keeps it),
//      updated on every input, cleared only when its reply was sent OK (or the operator discards it);
//   2. a refresh never tears down a focused, non-empty reply box (shouldDefer): it waits for blur;
//   3. when the ask closes while a draft exists the draft is shown in an "ask closed" box (orphans);
//   4. a render that does run restores value, focus and caret (capture / restore).
(function (root) {
  'use strict';

  const KEY = 'dr-tower-drafts';
  const TTL_MS = 7 * 86400000; // an abandoned draft is forgotten after a week
  const MAX = 50;              // drafts kept; the oldest go first

  // createStore(storage[, {key, now}]): ask id -> text. `storage` is localStorage-like and may be
  // missing or throw (private mode, quota): the drafts then live in memory only.
  function createStore(storage, opts) {
    const o = opts || {};
    const key = o.key || KEY;
    const now = o.now || Date.now;
    const m = new Map(); // id -> { t: text, at: ms }

    function save() {
      if (!storage) return;
      try {
        if (!m.size) storage.removeItem(key);
        else storage.setItem(key, JSON.stringify(Object.fromEntries(m)));
      } catch (_) { /* memory copy still holds */ }
    }
    function prune() {
      const cut = now() - TTL_MS;
      for (const [id, d] of m) if (!(d.at > cut)) m.delete(id);
      if (m.size > MAX) {
        [...m.entries()].sort((a, b) => a[1].at - b[1].at).slice(0, m.size - MAX).forEach(([id]) => m.delete(id));
      }
    }
    try {
      const raw = storage ? JSON.parse(storage.getItem(key) || '{}') : {};
      Object.keys(raw || {}).forEach((id) => {
        const d = raw[id];
        if (d && typeof d.t === 'string' && d.t.trim() && typeof d.at === 'number') m.set(id, { t: d.t, at: d.at });
      });
      prune();
    } catch (_) { /* corrupt entry: start empty */ }

    return {
      get(id) { const d = m.get(String(id)); return d ? d.t : ''; },
      // an empty (whitespace-only) text is "no draft"
      set(id, text) {
        const k = String(id);
        if (!String(text == null ? '' : text).trim()) { if (m.delete(k)) save(); return; }
        m.set(k, { t: String(text), at: now() });
        prune();
        save();
      },
      clear(id) { if (m.delete(String(id))) save(); },
      ids() { return [...m.keys()]; },
    };
  }

  // Ask ids the data still lists, folded ones included (a folded ask renders no reply box but is
  // not closed). model: every kind==='ask' item of every section; legacy overview: waiting plus
  // waiting_folded. Both are read when both are given, so a source switch never looks like a close.
  function presentAskIds(model, overview) {
    const ids = new Set();
    ((model && model.sections) || []).forEach((s) => (s.items || []).forEach((it) => { if (it && it.kind === 'ask' && it.id != null) ids.add(String(it.id)); }));
    const o = overview || {};
    (o.waiting || []).concat(o.waiting_folded || []).forEach((w) => { if (w && w.id != null) ids.add(String(w.id)); });
    return ids;
  }

  // Drafts whose ask is no longer listed: [{id, text}], newest data first by insertion order.
  function orphans(store, present) {
    return store.ids().filter((id) => !present.has(id)).map((id) => ({ id, text: store.get(id) }));
  }

  // Is `active` (document.activeElement) a Tower reply box holding text? Then the list must not be rebuilt.
  function shouldDefer(active) {
    return !!(active && active.tagName === 'TEXTAREA' && active.dataset && active.dataset.ask && String(active.value || '').trim());
  }

  // capture(active) -> a snapshot of the focused reply box (any content), restore(list, snap) after a rebuild.
  function capture(active) {
    if (!active || active.tagName !== 'TEXTAREA' || !active.dataset || !active.dataset.ask) return null;
    return { id: active.dataset.ask, start: active.selectionStart, end: active.selectionEnd, dir: active.selectionDirection };
  }
  function restore(list, snap) {
    if (!snap || !list) return false;
    const ta = [...list.querySelectorAll('textarea')].find((t) => t.dataset && t.dataset.ask === snap.id);
    if (!ta) return false;
    try {
      ta.focus({ preventScroll: true });
      if (typeof snap.start === 'number') ta.setSelectionRange(snap.start, snap.end, snap.dir || 'none');
    } catch (_) { /* detached or unfocusable: the draft value is still there */ }
    return true;
  }

  const api = { KEY, TTL_MS, MAX, createStore, presentAskIds, orphans, shouldDefer, capture, restore };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckDrafts = api;
})(typeof window !== 'undefined' ? window : globalThis);
