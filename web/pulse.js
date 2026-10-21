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

  // When a window resets, in the viewer's time: "06:00" when within a day, "Tue 06:00"
  // beyond. tz (IANA name) is for tests; the app passes none (device time zone).
  function fmtResetAt(resetsAt, nowMs, tz) {
    const t = Number(resetsAt) || 0;
    if (!(t > 0)) return '';
    const now = (nowMs == null ? Date.now() : nowMs) / 1000;
    const o = { hour: '2-digit', minute: '2-digit', hour12: false };
    if (tz) o.timeZone = tz;
    if (t - now > 20 * 3600) o.weekday = 'short';
    return new Intl.DateTimeFormat('en-GB', o).format(new Date(t * 1000)).replace(',', '');
  }

  // One line of the account quota: "Week ▮▮▯ 28% resets Tue 06:00". A window with no
  // record at all is a dash; one that has rolled over since its last record is 0 %.
  function line(label, w, nowMs, tz) {
    if (!w) return '<div class="q-line none"><span class="q-label">' + label + '</span><span class="q-val">—</span></div>';
    const p = w.expired ? 0 : clamp(w.pct);
    const at = w.expired ? '' : fmtResetAt(w.resetsAt, nowMs, tz);
    return '<div class="q-line lv-' + level(p, 'quota') + '"><span class="q-label">' + label + '</span>' + bar(p, 'quota') +
      '<span class="q-val"><b>' + Math.round(p) + '%</b></span>' +
      '<span class="q-reset">' + (w.expired ? 'just reset' : at ? 'resets ' + at : '') + '</span></div>';
  }

  // The account's weekly + 5 h usage; "as of Nm" once the freshest record is >10 min old,
  // "+N other" when fresh records on a different weekly window exist (another account).
  function quotaAccount(q, nowMs, tz) {
    const age = fmtAge(q.asOf, nowMs);
    const stale = fmtAge(q.asOf, nowMs, 60) ? ' stale' : ''; // an hour or more: dim it, the age tag stays
    const tags = (age ? '<span class="q-stale">as of ' + age.replace(' old', '') + '</span>' : '') +
      (q.others > 0 ? '<span class="q-other">+' + (Number(q.others) | 0) + ' other account' + (q.others > 1 ? 's' : '') + '</span>' : '');
    return '<div class="q-acct' + stale + '">' + line('Week', q.sevenDay, nowMs, tz) + line('5h', q.fiveHour, nowMs, tz) +
      (tags ? '<div class="q-tags">' + tags + '</div>' : '') + '</div>';
  }

  // The deck's quota strip: the account line. '' without data.
  function quotaStrip(quota, nowMs, tz) {
    if (!Array.isArray(quota) || !quota.length) return '';
    return quotaAccount(quota[0], nowMs, tz);
  }

  // Rate limits are per account, not per group: the one account line covers every session.
  function quotaFor(quota) {
    return (Array.isArray(quota) && quota[0]) || null;
  }

  // "Week 28% · 5h 0%" for the sheet header.
  function quotaInline(q) {
    if (!q) return '';
    const part = (label, w) => w ? '<span class="lv-' + level(w.expired ? 0 : w.pct, 'quota') + '">' + label + ' <b>' + Math.round(w.expired ? 0 : clamp(w.pct)) + '%</b></span>' : '';
    const out = [part('Week', q.sevenDay), part('5h', q.fiveHour)].filter(Boolean);
    return out.length ? '<div class="q-inline">' + out.join('') + '</div>' : '';
  }

  const api = { level, fmtTokens, fmtReset, fmtResetAt, fmtAge, bar, ctxMini, ctxFull, quotaStrip, quotaAccount, quotaFor, quotaInline };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckPulse = api;
})(typeof window !== 'undefined' ? window : globalThis);
