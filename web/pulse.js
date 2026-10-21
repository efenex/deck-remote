// Context / quota meters (the deck-remote counterpart of claude-pulse's statusline).
// Pure helpers: server rows in, HTML strings out. Loaded before app.js; the same
// file is required by node --test (pulse.test.js).
//
// Thresholds follow claude-pulse: usage bars turn yellow at 50 % and red at 80 %
// (bar_colour); the context bar uses its pressure warnings, 70 % and 90 %.
(function (root) {
  'use strict';

  const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const clamp = (n) => Math.max(0, Math.min(100, Number(n) || 0));

  // kind 'ctx' | 'quota' -> 'ok' | 'warn' | 'hot'
  function level(pct, kind) {
    const p = Number(pct) || 0;
    const [warn, hot] = kind === 'ctx' ? [70, 90] : [50, 80];
    return p >= hot ? 'hot' : p >= warn ? 'warn' : 'ok';
  }

  // 44033 -> "44k", 1000000 -> "1M", 1250000 -> "1.3M"
  function fmtTokens(n) {
    n = Number(n) || 0;
    if (n >= 1e6) { const m = n / 1e6; return (m >= 10 || Math.abs(m - Math.round(m)) < 0.05 ? Math.round(m) : m.toFixed(1)) + 'M'; }
    if (n >= 1e3) return Math.round(n / 1e3) + 'k';
    return String(n);
  }

  // Time until a reset (unix s): "42m", "1h02m", "3d4h"; '' when unknown or past.
  function fmtReset(resetsAt, nowMs) {
    const s = (Number(resetsAt) || 0) - Math.floor((nowMs == null ? Date.now() : nowMs) / 1000);
    if (!(Number(resetsAt) > 0) || s <= 0) return '';
    const m = Math.floor(s / 60), h = Math.floor(m / 60), d = Math.floor(h / 24);
    if (d >= 1) return d + 'd' + (h % 24) + 'h';
    if (h >= 1) return h + 'h' + String(m % 60).padStart(2, '0') + 'm';
    return Math.max(1, m) + 'm';
  }

  // "12m old" for a record older than staleMin minutes; '' otherwise.
  function fmtAge(asOf, nowMs, staleMin) {
    const min = Math.floor(((nowMs == null ? Date.now() : nowMs) / 1000 - (Number(asOf) || 0)) / 60);
    if (!(Number(asOf) > 0) || min < (staleMin == null ? 10 : staleMin)) return '';
    return min >= 120 ? Math.floor(min / 60) + 'h old' : min + 'm old';
  }

  function bar(pct, kind, cls) {
    const p = clamp(pct);
    return '<span class="pbar ' + (cls || '') + ' lv-' + level(p, kind) + '" role="img" aria-label="' + Math.round(p) + ' percent"><i style="width:' + p + '%"></i></span>';
  }

  // Compact context meter for a session row: "▮▮▯▯ 4%". '' without data.
  function ctxMini(ctx) {
    if (!ctx || ctx.pct == null) return '';
    const p = clamp(ctx.pct);
    const tip = Math.round(p) + '% of context' + (ctx.tokens ? ' · ' + fmtTokens(ctx.tokens) + (ctx.window ? ' / ' + fmtTokens(ctx.window) : '') + ' tokens' : '') + (ctx.model ? ' · ' + ctx.model : '');
    return '<span class="ctx-mini lv-' + level(p, 'ctx') + '" title="' + esc(tip) + '">' + bar(p, 'ctx', 'mini') + '<b>' + Math.round(p) + '%</b></span>';
  }

  // Sheet header line: bar, "4% · 44k / 1M", model.
  function ctxFull(ctx) {
    if (!ctx || ctx.pct == null) return '';
    const p = clamp(ctx.pct);
    const tok = ctx.tokens ? fmtTokens(ctx.tokens) + (ctx.window ? ' / ' + fmtTokens(ctx.window) : '') : '';
    return '<div class="ctx-full lv-' + level(p, 'ctx') + '">' + bar(p, 'ctx') +
      '<span class="ctx-num"><b>' + Math.round(p) + '%</b> context' + (tok ? ' · ' + esc(tok) : '') + '</span>' +
      (ctx.model ? '<span class="ctx-model">' + esc(ctx.model) + '</span>' : '') + '</div>';
  }

  function win(label, w, nowMs) {
    if (!w) return '<div class="q-win none"><span class="q-label">' + label + '</span><span class="q-val">—</span></div>';
    const p = clamp(w.pct);
    const r = fmtReset(w.resetsAt, nowMs);
    return '<div class="q-win lv-' + level(p, 'quota') + '"><span class="q-label">' + label + '</span>' + bar(p, 'quota') +
      '<span class="q-val"><b>' + Math.round(p) + '%</b>' + (r ? ' <span class="q-reset">↻ ' + r + '</span>' : '') + '</span></div>';
  }

  // One account's 5 h / 7 d usage; `groups` names the agent-deck groups it was read from.
  function quotaAccount(q, nowMs) {
    const age = fmtAge(q.asOf, nowMs);
    const stale = fmtAge(q.asOf, nowMs, 60) ? ' stale' : ''; // an hour or more: dim it, the age tag stays
    return '<div class="q-acct' + stale + '"><div class="q-groups">' + esc((q.groups || []).join(' · ')) + (age ? ' <span class="q-stale">' + age + '</span>' : '') + '</div>' +
      '<div class="q-wins">' + win('5h', q.fiveHour, nowMs) + win('7d', q.sevenDay, nowMs) + '</div></div>';
  }

  // The deck's quota strip: one block per account, never mixed. '' without data.
  function quotaStrip(quota, nowMs) {
    if (!Array.isArray(quota) || !quota.length) return '';
    return quota.map((q) => quotaAccount(q, nowMs)).join('');
  }

  // The quota entry that covers a session's group, if any.
  function quotaFor(quota, group) {
    return (Array.isArray(quota) ? quota : []).find((q) => (q.groups || []).indexOf(group) >= 0) || null;
  }

  // "5h 12% · 7d 20%" for the sheet header.
  function quotaInline(q, nowMs) {
    if (!q) return '';
    const part = (label, w) => w ? '<span class="lv-' + level(w.pct, 'quota') + '">' + label + ' <b>' + Math.round(clamp(w.pct)) + '%</b>' + (fmtReset(w.resetsAt, nowMs) ? ' ↻' + fmtReset(w.resetsAt, nowMs) : '') + '</span>' : '';
    const out = [part('5h', q.fiveHour), part('7d', q.sevenDay)].filter(Boolean);
    return out.length ? '<div class="q-inline">' + out.join('') + '</div>' : '';
  }

  const api = { level, fmtTokens, fmtReset, fmtAge, bar, ctxMini, ctxFull, quotaStrip, quotaAccount, quotaFor, quotaInline };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckPulse = api;
})(typeof window !== 'undefined' ? window : globalThis);
