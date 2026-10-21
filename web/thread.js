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
    if (t.role === 'bash') return k + '\n' + (t.stdout || '') + '\n' + (t.stderr || '');
    // Image-only prompts have no text: the count keeps two of them distinct.
    return t.images ? k + '\n' + t.images + 'img' : k;
  }

  // The chip under/after a user prompt that carried images.
  function imagesLabel(n) {
    return '\uD83D\uDCF7 ' + n + (n === 1 ? ' image' : ' images');
  }

  function escHtml(s) {
    return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  // One compact block for an operator `!` shell line: the command, stdout below,
  // stderr in its own (red) element. The `$` prompt is the row's gutter, not
  // part of this block (a second one here showed `$ $ cmd`). Everything is escaped — transcript content
  // is untrusted. Returns '' when there is nothing to show.
  function bashBlockHTML(t) {
    const cmd = String(t.content == null ? '' : t.content).trim();
    const out = String(t.stdout == null ? '' : t.stdout);
    const err = String(t.stderr == null ? '' : t.stderr);
    if (!cmd && !out.trim() && !err.trim()) return '';
    let h = '';
    if (cmd) h += '<div class="bash-cmd">' + escHtml(cmd) + '</div>';
    if (out.trim()) h += '<pre class="bash-out">' + escHtml(out) + '</pre>';
    if (err.trim()) h += '<pre class="bash-err">' + escHtml(err) + '</pre>';
    return h;
  }

  function firstLine(text) {
    const lines = String(text == null ? '' : text).split('\n');
    for (const l of lines) { if (l.trim()) return l.trim(); }
    return '';
  }

  // One-line summary of a collapsed peer / slash row. A peer shows who sent it
  // (the envelope's display name, else the session title for its id, else a short
  // id) and the first line of the message; a slash row shows the command + args.
  // nameOf(id) -> a title or ''.
  function foldSummary(t, nameOf) {
    if (t.role === 'slash') return firstLine(t.content);
    const id = String(t.origin || '').replace(/^peer:/, '');
    const who = t.sender || (id && nameOf && nameOf(id)) || (id ? id.slice(0, 8) : 'peer');
    return who + ' \u00b7 ' + firstLine(t.content);
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

  // Which tail turns (oldest-first, already mapped to convo turns) are missing
  // from the thread? One-for-one multiset match on turnKey over every role, so a
  // genuine repeat still counts but a row seen on the last poll (bash rows
  // included) is not appended again.
  function newTailTurns(conv, tail) {
    const seen = new Map();
    conv.forEach((e) => {
      if (e.content == null) return;
      const k = turnKey(e);
      seen.set(k, (seen.get(k) || 0) + 1);
    });
    return tail.filter((t) => {
      const k = turnKey(t);
      const left = seen.get(k) || 0;
      if (left > 0) { seen.set(k, left - 1); return false; }
      return true;
    });
  }


  // ----- phone-message delivery status -----
  // A bubble the operator sent from the phone walks sent (server accepted) ->
  // queued (n) (waiting its turn in agent-deck's queue) -> delivering ->
  // delivered (in the session's transcript), or failed / unconfirmed.
  const IN_FLIGHT = { sent: 1, queued: 1, delivering: 1 };

  // Apply an `ask-state` SSE event to the matching 'me' bubble. The bubble is
  // matched by requestId; the first event of a send (state "sent") can beat the
  // POST response that tags it, so an untagged 'sent' bubble is claimed too.
  // Returns the bubble, or null when none matches.
  function applyAskState(conv, ev) {
    let e = null;
    for (let i = conv.length - 1; i >= 0; i--) {
      if (conv[i].role === 'me' && conv[i].requestId === ev.requestId) { e = conv[i]; break; }
    }
    if (!e) {
      for (let i = 0; i < conv.length; i++) {
        if (conv[i].role === 'me' && !conv[i].requestId && conv[i].status === 'sent') { e = conv[i]; break; }
      }
      if (e) e.requestId = ev.requestId;
    }
    if (!e) return null;
    // The transcript already proved delivery (promoteDelivered): agent-deck's
    // later, weaker verdict ("unconfirmed") must not take it back.
    if (e.status === 'delivered') return e;
    e.status = ev.state;
    e.position = ev.position || 0;
    e.error = ev.error || '';
    e.retryable = !!ev.retryable;
    e.queued = !!IN_FLIGHT[ev.state];
    return e;
  }

  // The session's transcript is the ground truth for "delivered": agent-deck's
  // own landed detection gives up after 2 minutes, so a message that waited out
  // a long turn can settle "unconfirmed" while its text is plainly in the
  // transcript. Promote each phone bubble that has a not-older matching 'me'
  // row in the tail (one-for-one, time-bounded like mergeFreshHistory so an old
  // identical "yes" proves nothing). Returns how many bubbles changed.
  function promoteDelivered(conv, tail) {
    const used = new Set();
    let n = 0;
    conv.forEach((e) => {
      if (e.role !== 'me' || e.hist || !e.status || e.status === 'delivered') return;
      const k = turnKey(e);
      for (let i = tail.length - 1; i >= 0; i--) {
        const f = tail[i];
        if (used.has(i) || f.role !== 'me' || turnKey(f) !== k) continue;
        if (f.ts && e.ts && f.ts < e.ts - SAME_TURN_SECS) continue;
        used.add(i);
        e.status = 'delivered'; e.queued = false; e.error = ''; e.retryable = false;
        if (f.origin) e.origin = f.origin;
        n++;
        return;
      }
    });
    return n;
  }

  function sendStatusLabel(e) {
    // A reply/slash-result clears `queued`: stale in-flight statuses go quiet.
    if (IN_FLIGHT[e.status] && !e.queued) return '';
    switch (e.status) {
      case 'queued': return e.position ? 'queued (' + e.position + ')' : 'queued';
      case 'delivering': return 'delivering…';
      case 'delivered': return e.origin === 'phone' ? 'you (phone) · delivered' : 'delivered';
      case 'unconfirmed': return 'unconfirmed — check the terminal before resending';
      case 'failed': return 'failed' + (e.error ? ': ' + e.error : '');
      case 'sent': return 'sent';
      default: return e.queued ? 'sent' : '';
    }
  }

  // Only a send agent-deck says typed nothing may be resent: an "unconfirmed"
  // one can already be sitting in the composer.
  function canRetry(e) { return e.status === 'failed' && e.retryable !== false; }

  // Live fields the server drops from a session row when they are falsy
  // (`omitempty`): a session that stopped working is simply sent without
  // `working`. Merging a fresh row over the previous one must reset them first,
  // or last night's `working:true` / "Channelling…" / needsAttention survive
  // every refresh until the app is restarted.
  const LIVE_DEFAULTS = {
    working: false, stalled: false, needsAttention: false,
    activity: '', currentTool: '', state: '', degradedReason: '', ctx: null,
  };
  // lastReply / lastActivity are deliberately not reset: a transient snapshot
  // failure server-side omits them, and blanking would fold the group and drop
  // the preview for a blip.

  // The previous row only keeps what the server never sends (client-side
  // annotations); everything the server owns comes from the fresh row.
  function mergeSessionRow(prev, fresh) {
    return Object.assign({}, prev || {}, LIVE_DEFAULTS, fresh);
  }

  // The Deck row's badge for a session that needs the operator: what is waiting, not just "needs you".
  function attentionChipLabel(s) {
    if (!s || !s.needsAttention) return '';
    if (s.attentionKind === 'question') {
      const n = Number(s.attentionN) || 0;
      return n > 1 ? n + ' questions' : 'question';
    }
    if (s.attentionKind === 'approval') return 'approval';
    return 'needs you';
  }

  // One line for an answer walk's progress event ({step, n, total}); '' for an unknown step.
  function answerProgressLabel(ev) {
    if (!ev) return '';
    const of = ev.total > 1 ? ' ' + ev.n + '/' + ev.total : '';
    switch (ev.step) {
      case 'checking': return 'Checking the menu…';
      case 'answering': return 'Answering' + of + '…';
      case 'verifying': return 'Verifying the answers…';
      case 'submitting': return 'Submitting…';
      default: return '';
    }
  }

  const api = { attentionChipLabel, answerProgressLabel, mergeSessionRow, firstLine, foldSummary, newTailTurns, turnKey, imagesLabel, bashBlockHTML, mergeFreshHistory, trimOverlap, applyAskState, promoteDelivered, sendStatusLabel, canRetry };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckThread = api;
})(typeof window !== 'undefined' ? window : globalThis);
