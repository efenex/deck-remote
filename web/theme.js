/* deck-remote — appearance options (PREVIEW).
 *
 * Four axes, each a data-* attribute on <html> that index.html's CSS keys off:
 *   data-theme   dark | opencode | light   (dark = today's Tokyo Night)
 *   data-density comfortable | compact
 *   data-shape   round | flat
 *   data-turns   rail | block              (block = opencode-style own-input tint)
 * The default of every axis is today's look and is expressed by the attribute
 * being absent (data-theme stays "dark", as shipped in index.html), so nothing
 * changes until the operator picks something. Choice persists in localStorage
 * under dr-theme; ?preview=<preset>|off sets it from a URL.
 *
 * Loaded in <head> (window.DeckTheme) and by the node tests (web/theme.test.js).
 */
(function (root) {
  'use strict';

  const KEY = 'dr-theme';
  const AXES = {
    theme: ['dark', 'opencode', 'light'],
    density: ['comfortable', 'compact'],
    shape: ['round', 'flat'],
    turns: ['rail', 'block'],
  };
  const DEFAULTS = { theme: 'dark', density: 'comfortable', shape: 'round', turns: 'rail' };
  const PRESETS = {
    classic: { label: 'Classic', ...DEFAULTS },
    opencode: { label: 'Opencode', theme: 'opencode', density: 'compact', shape: 'flat', turns: 'block' },
    dense: { label: 'Dense', theme: 'dark', density: 'compact', shape: 'flat', turns: 'block' },
    paper: { label: 'Paper', theme: 'light', density: 'comfortable', shape: 'round', turns: 'block' },
  };
  // <meta name="theme-color"> per colour (status bar / task switcher tint).
  const META_COLOR = { dark: '#1a1b26', opencode: '#0a0a0a', light: '#f5f5f2' };

  // Unknown keys dropped, unknown values reset to the default.
  function normalize(p) {
    const out = {};
    for (const k of Object.keys(AXES)) {
      const v = p && typeof p === 'object' ? p[k] : undefined;
      out[k] = AXES[k].includes(v) ? v : DEFAULTS[k];
    }
    return out;
  }

  function isDefault(p) {
    const n = normalize(p);
    return Object.keys(AXES).every((k) => n[k] === DEFAULTS[k]);
  }

  // The preset whose axes all match, or '' for a custom mix.
  function presetOf(p) {
    const n = normalize(p);
    for (const [name, pr] of Object.entries(PRESETS)) {
      if (Object.keys(AXES).every((k) => pr[k] === n[k])) return name;
    }
    return '';
  }

  function load(storage) {
    try { return normalize(JSON.parse(storage.getItem(KEY) || 'null')); } catch (_) { return normalize(null); }
  }

  // The default is stored as no key at all, so "reset" leaves no trace.
  function save(storage, p) {
    try {
      if (isDefault(p)) storage.removeItem(KEY);
      else storage.setItem(KEY, JSON.stringify(normalize(p)));
    } catch (_) {}
  }

  // ?preview=<preset> → that preset; ?preview=off|default|classic → defaults;
  // absent or unknown → null (leave the stored choice alone).
  function fromQuery(search) {
    let v;
    try { v = new URLSearchParams(search || '').get('preview'); } catch (_) { return null; }
    if (v == null) return null;
    v = v.trim().toLowerCase();
    if (v === 'off' || v === 'default') return normalize(null);
    return PRESETS[v] ? normalize(PRESETS[v]) : null;
  }

  // Attributes for <html>: data-theme always (as shipped), the others only
  // when they differ from the default.
  function attrs(p) {
    const n = normalize(p);
    const a = { 'data-theme': n.theme };
    for (const k of ['density', 'shape', 'turns']) a['data-' + k] = n[k] === DEFAULTS[k] ? null : n[k];
    return a;
  }

  function apply(docEl, p) {
    for (const [name, v] of Object.entries(attrs(p))) {
      if (v == null) docEl.removeAttribute(name); else docEl.setAttribute(name, v);
    }
  }

  const api = { KEY, AXES, DEFAULTS, PRESETS, normalize, isDefault, presetOf, load, save, fromQuery, attrs, apply };
  if (typeof module !== 'undefined' && module.exports) { module.exports = api; return; }
  root.DeckTheme = api;

  // ---------------- browser wiring ----------------
  const doc = root.document;
  let cur = load(root.localStorage);
  const q = fromQuery(root.location.search);
  if (q) {
    cur = q;
    save(root.localStorage, cur);
    try {
      const u = new URL(root.location.href);
      u.searchParams.delete('preview');
      root.history.replaceState(root.history.state, '', u.pathname + u.search + u.hash);
    } catch (_) {}
  }

  function paint() {
    apply(doc.documentElement, cur);
    const meta = doc.querySelector('meta[name="theme-color"]');
    if (meta) meta.setAttribute('content', META_COLOR[cur.theme]);
    const title = doc.querySelector('#screenDeck .appbar-title');
    let chip = doc.getElementById('themeChip');
    if (isDefault(cur)) { if (chip) chip.remove(); } else if (title) {
      if (!chip) { chip = doc.createElement('span'); chip.id = 'themeChip'; chip.className = 'theme-chip'; title.appendChild(chip); }
      chip.textContent = 'preview: ' + (PRESETS[presetOf(cur)] || { label: 'custom' }).label.toLowerCase();
    }
    syncControls();
  }

  const LABELS = {
    theme: { dark: 'Tokyo', opencode: 'Opencode', light: 'Paper' },
    density: { comfortable: 'Comfy', compact: 'Compact' },
    shape: { round: 'Round', flat: 'Flat' },
    turns: { rail: 'Rail', block: 'Block' },
  };
  const ROWS = [['preset', 'Preset'], ['theme', 'Colour'], ['density', 'Density'], ['shape', 'Corners'], ['turns', 'Your turns']];

  function syncControls() {
    const box = doc.getElementById('themeOpts');
    if (!box) return;
    const pr = presetOf(cur);
    box.querySelectorAll('.seg-opt').forEach((b) => {
      const on = b.dataset.axis === 'preset' ? b.dataset.v === pr : cur[b.dataset.axis] === b.dataset.v;
      b.classList.toggle('on', on);
    });
  }

  function build() {
    const anchor = doc.getElementById('escapeTerm');
    if (!anchor || doc.getElementById('themeOpts')) return;
    const head = doc.createElement('div');
    head.className = 'set-section';
    head.textContent = 'Appearance · PREVIEW';
    const list = doc.createElement('div');
    list.className = 'set-list theme-opts';
    list.id = 'themeOpts';
    for (const [axis, label] of ROWS) {
      const row = doc.createElement('div');
      row.className = 'set-row';
      const lbl = doc.createElement('span');
      lbl.className = 'lbl';
      lbl.textContent = label;
      const seg = doc.createElement('div');
      seg.className = 'seg';
      seg.setAttribute('role', 'radiogroup');
      const opts = axis === 'preset'
        ? Object.entries(PRESETS).map(([k, p]) => [k, p.label])
        : AXES[axis].map((v) => [v, LABELS[axis][v]]);
      for (const [v, text] of opts) {
        const b = doc.createElement('button');
        b.type = 'button';
        b.className = 'seg-opt';
        b.dataset.axis = axis;
        b.dataset.v = v;
        b.textContent = text;
        seg.appendChild(b);
      }
      row.append(lbl, seg);
      list.appendChild(row);
    }
    const note = doc.createElement('div');
    note.className = 'set-row';
    note.innerHTML = '<span class="lbl"><div class="rhint" style="font-size:12px;color:var(--muted)">Trial only, stored on this device. Classic = the current look. Link form: <code>?preview=opencode</code> (or <code>off</code>).</div></span>';
    list.appendChild(note);
    list.addEventListener('click', (e) => {
      const b = e.target.closest('.seg-opt');
      if (!b) return;
      cur = b.dataset.axis === 'preset' ? normalize(PRESETS[b.dataset.v]) : normalize({ ...cur, [b.dataset.axis]: b.dataset.v });
      save(root.localStorage, cur);
      paint();
    });
    anchor.parentNode.insertBefore(head, anchor);
    anchor.parentNode.insertBefore(list, anchor);
  }

  apply(doc.documentElement, cur); // before first paint: script runs in <head>
  const ready = () => { build(); paint(); };
  if (doc.readyState === 'loading') doc.addEventListener('DOMContentLoaded', ready); else ready();
})(typeof window !== 'undefined' ? window : globalThis);
