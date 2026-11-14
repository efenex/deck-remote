/* deck-remote — pure thread-merge helpers.
 *
 * Loaded before app.js (window.DeckThread) and by the node tests
 * (web/thread.test.js). No DOM, no state: inputs in, arrays out.
 */
(function (root) {
  'use strict';

  // agent-deck appends generated attachment references to the caption. Parse
  // only that trailing transport suffix, never paths mentioned inside prose.
  function photoParts(content) {
    const text = String(content == null ? '' : content).trim();
    const suffix = /(?:\s+@\/(?:[^\s]+\/)?\.agentdeck-images\/\d+-\d+-[^\s]+\.(?:jpg|jpeg|png))+$/i.exec(text);
    return suffix ? { caption: text.slice(0, suffix.index).trim(), count: suffix[0].trim().split(/\s+/).length } : null;
  }
  function photoCount(content) { const p = photoParts(content); return p ? p.count : 0; }

  // Only a local photo echo may match by caption + attachment count. History
  // rows retain their full references in turnKey: two different uploads with
  // the same caption must remain distinct on subsequent polls and paging.
  function sameEcho(e, f) {
    if (turnKey(e) === turnKey(f)) return true;
    if (e.hist || e.role !== 'me' || !e.status || !e.images || f.role !== 'me' || (e.kind || '') !== (f.kind || '')) return false;
    const p = photoParts(f.content);
    return !!p && p.count === e.images && (!f.images || f.images === p.count) &&
      p.caption === String(e.content || '').trim();
  }

  // Identity of a turn for dedup: role, kind and trimmed content. Not ts —
  // live SSE rows carry ms-precision Date.now() stamps, the transcript whole
  // seconds.
  function turnKey(t) {
    const k = t.role + '\n' + (t.kind || '') + '\n' + String(t.content == null ? '' : t.content).trim();
    // Bash-mode rows share a command across runs: their output tells them apart.
    if (t.role === 'bash') return k + '\n' + (t.stdout || '') + '\n' + (t.stderr || '');
    // Image-only prompts have no text: the count keeps two of them distinct.
    const images = t.images || (t.role === 'me' ? photoCount(t.content) : 0);
    return images ? k + '\n' + images + 'img' : k;
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
    const claimed = new Map(current.filter(e => e.landedRowId).map(e => [e.landedRowId, e]));
    const first = fresh.find((e) => e.ts);
    const kept = current.filter((e) => {
      if (e.hist && !e.sendId) return false;
      // A delivered echo can have missed tail reconciliation (sheet closed),
      // or predate the hist promotion fix. Once outside this history window it
      // belongs on an older page, not after the newest reply. Keep unresolved
      // sends regardless of age so queued/failed messages remain actionable.
      if (e.role === 'me' && e.status === 'delivered' && first && e.ts &&
          e.ts < first.ts - SAME_TURN_SECS &&
          (!e.landedRowId || !fresh.some(f => f.id === e.landedRowId))) return false;
      if (e.role !== 'me' && e.role !== 'reply') return true;
      for (let i = fresh.length - 1; i >= 0; i--) {
        const f = fresh[i];
        if (claimed.has(f.id) && claimed.get(f.id) !== e) continue;
        if (used.has(i) || (e.landedRowId ? f.id !== e.landedRowId : !sameEcho(e, f))) continue;
        if (!e.landedRowId && f.ts && e.ts && f.ts < e.ts - SAME_TURN_SECS) continue;
        used.add(i);
        if (e.sendId) {
          Object.assign(f, e, { hist: true, ts: f.ts, id: f.id, content: f.content,
            status: e.status === 'delivered' ? 'delivered' : 'seen', queued: false,
            reason: e.status === 'delivered' ? 'Seen in the target transcript' : 'Content seen; delivery attribution is unverified',
            redeliveryReason: '', error: '', statusTs: f.ts || e.statusTs });
        }
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
  const IN_FLIGHT = { sent: 1, queued: 1, delivering: 1, pending: 1 };

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
    // Positive agent-deck proof is monotonic. A later landed receipt may add
    // the row ID, but a weaker status must not take delivery back.
    if (e.status === 'delivered' && ev.state !== 'delivered') return e;
    // An older poll or the initial acceptance event can arrive after a newer
    // receipt. Never let it erase delivery diagnostics or regress the state.
    if (e.sendId && !ev.sendId && ev.state === 'sent') return e;
    const recordTs = e.recordStatusTs ?? e.statusTs;
    if (!ev.operationId && ev.state !== 'delivered' && ev.statusTs && recordTs && ev.statusTs < recordTs) return e;
    if (ev.sendId) e.sendId = ev.sendId;
    if (ev.statusTs) {
      // Operation time is not the queue record's revision. A fresh status
      // read may reveal a terminal snapshot older than the tap's decision.
      if (!ev.operationId) e.recordStatusTs = ev.statusTs;
      else if (e.recordStatusTs === undefined) e.recordStatusTs = e.statusTs || 0;
      e.statusTs = ev.statusTs;
    }
    if (ev.reason !== undefined) e.reason = ev.reason;
    if (ev.targetStatus !== undefined) e.targetStatus = ev.targetStatus;
    if (ev.attempts !== undefined) e.attempts = ev.attempts;
    if (ev.redeliveryReason !== undefined) e.redeliveryReason = ev.redeliveryReason;
    if (ev.redeliveryOutcome) e.redeliveryOutcome = ev.redeliveryOutcome;
    if (ev.landedRowId) e.landedRowId = ev.landedRowId;
    if (ev.images) e.images = ev.images;
    e.status = ev.state;
    if (ev.state === 'delivered') e.redeliveryReason = '';
    e.position = ev.position || 0;
    e.error = ev.error || '';
    e.retryable = !!ev.retryable;
    e.queued = !!IN_FLIGHT[ev.state];
    return e;
  }

  // Prefer the owner's landed row ID. Content-only matches are one-for-one,
  // time-bounded and labelled seen (unverified), since a human may have typed
  // the same text. Adopt history ownership to avoid re-appending old echoes.
  // Returns how many bubbles changed, including already-delivered SSE echoes.
  function promoteDelivered(conv, tail) {
    const used = new Set();
    const photos = [];
    const claimed = new Map(conv.filter(e => e.landedRowId).map(e => [e.landedRowId, e]));
    let n = 0;
    conv.forEach((e) => {
      if (e.role !== 'me' || (!e.hist && !e.status)) return;
      for (let i = 0; i < tail.length; i++) {
        const f = tail[i];
        if (used.has(i) || f.role !== 'me' || (claimed.has(f.id) && claimed.get(f.id) !== e)) continue;
        if (e.landedRowId ? f.id !== e.landedRowId : !sameEcho(e, f)) continue;
        // Reserve rows already reconciled on an earlier poll, so the same
        // transcript "yes" cannot deliver another queued "yes" next time.
        if (!e.landedRowId && e.hist && e.ts !== f.ts) continue;
        if (!e.landedRowId && f.ts && e.ts && f.ts < e.ts - SAME_TURN_SECS) continue;
        used.add(i);
        if (e.hist && (!e.landedRowId || e.id === f.id)) return;
        if (e.images) {
          // Keep the canonical references for later polls, without losing the
          // image chip when this transcript represents attachments as text only.
          e.content = f.content;
          e.images = f.images || photoCount(f.content) || e.images;
          photos.push(e);
        }
        e.status = e.landedRowId || e.status === 'delivered' ? 'delivered' : 'seen'; e.queued = false; e.error = ''; e.retryable = false;
        e.reason = e.status === 'delivered' ? 'Seen in the target transcript' : 'Content seen; delivery attribution is unverified'; e.redeliveryReason = '';
        if (f.ts) e.statusTs = f.ts;
        e.hist = true;
        if (f.id) e.id = f.id;
        if (f.ts) e.ts = f.ts;
        if (f.origin) e.origin = f.origin;
        n++;
        return;
      }
    });
    // A pre-fix echo may already sit after newer history. Move just the
    // reconciled photos back before later transcript rows; leave pending sends
    // and untimestamped notes alone rather than sorting the whole conversation.
    photos.forEach((e) => {
      const from = conv.indexOf(e);
      const to = conv.findIndex((f, i) => i < from && f.hist && f.ts && e.ts && f.ts > e.ts);
      if (to >= 0) { conv.splice(from, 1); conv.splice(to, 0, e); }
    });
    return n;
  }

  function sendStatusLabel(e) {
    // A reply/slash-result clears `queued`: stale in-flight statuses go quiet.
    if (IN_FLIGHT[e.status] && !e.queued) return '';
    switch (e.status) {
      case 'queued': return e.position ? 'queued (' + e.position + ')' : 'queued';
      case 'delivering': return 'delivering…';
      case 'delivered': return e.origin === 'phone' ? 'you (phone) · landed' : 'landed';
      case 'lost':
      case 'blocked_unknown': return 'may have landed; not re-sent · check again';
      case 'unsupported': return 'unverifiable';
      case 'seen': return 'seen (unverified)';
      case 'pending': return 'in progress';
      case 'unconfirmed': return 'unconfirmed — check the terminal before resending';
      case 'failed': return 'failed' + (e.error ? ': ' + e.error : '');
      case 'refused': return 're-delivery unavailable';
      case 'sent': return 'sent';
      default: return e.queued ? 'sent' : '';
    }
  }

  // This action checks/releases the ORIGINAL durable ID, never copies the
  // text into /ask. agent-deck reconciles uncertain and already-landed sends.
  function canRetry(e) {
    return !!e.sendId && !!e.requestId && !e.redelivering &&
      ['queued', 'delivering', 'sent', 'failed', 'lost', 'unconfirmed', 'blocked_unknown', 'unsupported', 'pending', 'seen', 'refused'].includes(e.status);
  }

  // Only receipt IDs are persisted, never message text. An unfinished operation
  // survives a reload; completing it removes the key so the next tap is new.
  const REDELIVERY_PREFIX = 'deck.redelivery.';
  function operationSlot(profile, sessionId, sendId) {
    return REDELIVERY_PREFIX + JSON.stringify([profile, sessionId, sendId]);
  }
  function beginRedelivery(storage, profile, sessionId, entry, uuid) {
    const slot = operationSlot(profile, sessionId, entry.sendId);
    const old = JSON.parse(storage.getItem(slot) || 'null');
    if (old) {
      if (old.requestId !== entry.requestId || !old.operationId) throw new Error('Original check receipt does not match');
      return old.operationId;
    }
    const op = { profile, sessionId, sendId: entry.sendId, requestId: entry.requestId, operationId: uuid(), ts: entry.ts };
    storage.setItem(slot, JSON.stringify(op)); // failure must prevent the POST
    return op.operationId;
  }
  function finishRedelivery(storage, profile, sessionId, entry, operationId) {
    const slot = operationSlot(profile, sessionId, entry.sendId);
    const op = JSON.parse(storage.getItem(slot) || 'null');
    if (op && op.operationId === operationId) storage.removeItem(slot);
  }
  function pendingRedeliveries(storage, profile, sessionId) {
    const pending = [];
    for (let i = 0; i < storage.length; i++) {
      const key = storage.key(i);
      if (!key.startsWith(REDELIVERY_PREFIX)) continue;
      const op = JSON.parse(storage.getItem(key));
      if (op.profile === profile && op.sessionId === sessionId) pending.push({
        role: 'me', content: 'Original message · delivery check pending',
        requestId: op.requestId, sendId: op.sendId, ts: op.ts, redeliveryOperationId: op.operationId,
        status: 'unconfirmed', reason: 'Previous check has no confirmed response; retry to recover it',
      });
    }
    return pending;
  }
  function retryLabel(e) {
    if (e.redeliveryOperationId) return 'Retry check';
    return ['lost', 'blocked_unknown', 'unsupported', 'seen', 'unconfirmed'].includes(e.status) ? 'Check again' : 'Re-deliver';
  }

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

  // What the card says when an answer walk did not finish. The host reports how many answers are
  // already in the menu ({done,total}); a sending tap again finishes the rest. A request that got no
  // reply at all may still be landing on the host, so it is not called "Not sent".
  function answerFailureLabel(err) {
    const e = err || {};
    const d = e.data || {};
    // A review that disagrees with the phone's answers refuses the same way every time: say so
    // instead of inviting another tap.
    if (d.total > 0 && d.done > 0 && /review shows/.test(e.message || '')) {
      return d.done + ' of ' + d.total + ' answers are entered, but the terminal does not match your choices, so nothing was submitted. ' + e.message;
    }
    if (d.total > 0 && d.done > 0) {
      return d.done >= d.total
        ? 'All ' + d.total + ' answers are entered but not submitted yet; tap Submit answers to finish.'
        : d.done + ' of ' + d.total + ' answers sent; tap Submit answers to finish.';
    }
    if (!e.status && /did not respond/.test(e.message || '')) {
      return 'No reply from the host yet: your answers may still be going in. Re-checking the menu; if it is partly answered, tap Submit answers to finish.';
    }
    if (e.status === 409 && /already being sent/.test(e.message || '')) {
      return 'Still sending the earlier tap; the host is working through your answers.';
    }
    return 'Not sent: ' + (e.message || 'unknown error');
  }

  const api = { beginRedelivery, finishRedelivery, pendingRedeliveries, retryLabel, attentionChipLabel, answerProgressLabel, answerFailureLabel, mergeSessionRow, firstLine, foldSummary, newTailTurns, turnKey, photoCount, imagesLabel, bashBlockHTML, mergeFreshHistory, trimOverlap, applyAskState, promoteDelivered, sendStatusLabel, canRetry };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckThread = api;
})(typeof window !== 'undefined' ? window : globalThis);
