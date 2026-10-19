/* Server/web version check: pure logic for the "rebuild + restart needed" banner. */
(function (root) {
  'use strict';

  function shaMatch(a, b) {
    if (!a || !b || a === 'unknown' || b === 'unknown') return true; // can't tell: don't alarm
    return a.startsWith(b) || b.startsWith(a);
  }

  function ago(ms) {
    const m = Math.round(ms / 60000);
    if (m < 1) return 'just now';
    if (m < 90) return m + ' min ago';
    if (m < 2880) return (m / 60).toFixed(1) + ' h ago';
    return (m / 1440).toFixed(1) + ' d ago';
  }

  // GET /api/rc/version -> null (nothing to say) or {text, level: 'warn'|'info', key}.
  // key identifies the situation so a dismissed banner stays dismissed until it changes.
  function banner(v, nowMs) {
    if (!v || typeof v !== 'object') return null;
    const parts = [];
    let key = '';
    if (!shaMatch(v.server_sha, v.web_sha)) {
      let built = '';
      const t = Date.parse(v.built_at || '');
      if (!isNaN(t)) built = ' (server built ' + ago((nowMs == null ? Date.now() : nowMs) - t) + ')';
      parts.push('server ' + v.server_sha + ' ≠ web ' + v.web_sha + ': rebuild + restart needed' + built);
      key += v.server_sha + '/' + v.web_sha;
    }
    if (v.web_dirty) {
      parts.push('web has uncommitted changes');
      key += '/dirty';
    }
    if (!parts.length) return null;
    return { text: parts.join(' · '), level: shaMatch(v.server_sha, v.web_sha) ? 'info' : 'warn', key };
  }

  const api = { banner, shaMatch };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckVersion = api;
})(typeof window !== 'undefined' ? window : globalThis);
