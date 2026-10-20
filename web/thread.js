/* deck-remote — pure thread-merge helpers.
 *
 * Loaded before app.js (window.DeckThread) and by the node tests
 * (web/thread.test.js). No DOM, no state: inputs in, arrays out.
 */
(function (root) {
  'use strict';

  // Identity of a turn for dedup: role, kind and trimmed content. Not ts —
  // live SSE rows carry ms-precision Date.now() stamps, the transcript whole
  // seconds.
  function turnKey(t) {
    const k = t.role + '\n' + (t.kind || '') + '\n' + String(t.content == null ? '' : t.content).trim();
    // Bash-mode rows share a command across runs: their output tells them apart.
    return t.role === 'bash' ? k + '\n' + (t.stdout || '') + '\n' + (t.stderr || '') : k;
  }

  function escHtml(s) {
    return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  // One compact block for an operator `!` shell line: "$ cmd", stdout below,
  // stderr in its own (red) element. Everything is escaped — transcript content
  // is untrusted. Returns '' when there is nothing to show.
  function bashBlockHTML(t) {
    const cmd = String(t.content == null ? '' : t.content).trim();
    const out = String(t.stdout == null ? '' : t.stdout);
    const err = String(t.stderr == null ? '' : t.stderr);
    if (!cmd && !out.trim() && !err.trim()) return '';
    let h = '';
    if (cmd) h += '<div class="bash-cmd"><span class="bash-prompt">$ </span>' + escHtml(cmd) + '</div>';
    if (out.trim()) h += '<pre class="bash-out">' + escHtml(out) + '</pre>';
    if (err.trim()) h += '<pre class="bash-err">' + escHtml(err) + '</pre>';
    return h;
  }

  // A transcript row is the same message as a live row if its key matches and
  // it is not older than the live row by more than this (the live stamp is
  // taken when the PWA sends or receives; the transcript's can lag or lead).
  const SAME_TURN_SECS = 120;

  // Re-seed a thread from a fresh history page. Rows that came from history
  // (hist: true) are replaced by the fresh copy; live rows (sent from here,
  // SSE replies, notes, errors) are kept after it unless the transcript has
  // caught up with them. Matching is one-for-one and time-bounded, so an old
  // "yes" in the transcript does not swallow a new "yes" still in flight.
  function mergeFreshHistory(fresh, current) {
    const used = new Set();
    const kept = current.filter((e) => {
      if (e.hist) return false;
      if (e.role !== 'me' && e.role !== 'reply') return true;
      const k = turnKey(e);
      for (let i = fresh.length - 1; i >= 0; i--) {
        const f = fresh[i];
        if (used.has(i) || turnKey(f) !== k) continue;
        if (f.ts && e.ts && f.ts < e.ts - SAME_TURN_SECS) continue;
        used.add(i);
        return false;
      }
      return true;
    });
    return fresh.concat(kept);
  }

  // Trim an older history page where it overlaps the thread's head. Paging is
  // by offset from the newest message, so turns appended since the last page
  // shift the window and its newest rows repeat the oldest ones shown. Only
  // the boundary is compared, so identical messages elsewhere are kept.
  function trimOverlap(older, head) {
    const max = Math.min(older.length, head.length);
    for (let k = max; k > 0; k--) {
      let match = true;
      for (let i = 0; i < k; i++) {
        if (turnKey(older[older.length - k + i]) !== turnKey(head[i])) { match = false; break; }
      }
      if (match) return older.slice(0, older.length - k);
    }
    return older;
  }

  const api = { turnKey, bashBlockHTML, mergeFreshHistory, trimOverlap };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckThread = api;
})(typeof window !== 'undefined' ? window : globalThis);
