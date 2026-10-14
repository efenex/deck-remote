/* deck-remote — live PWA frontend
 *
 * Vanilla JS, no build step, no external deps. Drives the dark Tokyo Night UI
 * from the approved mockup against deck-remote's real APIs.
 *
 * Detail-first model: the deck leads with each session's real last reply.
 * agent-deck's `status` field is intentionally NOT rendered — it's unreliable
 * in this environment. Attention panels are gated on a live rollout/dialog
 * (GET /api/rc/attention), never on status.
 *
 * Contract (same-origin, bearer-token auth):
 *   GET  /api/rc/sessions    -> {sessions:[{id,title,path,group,tool,lastReply,working,activity,currentTool,...}]}
 *   GET  /api/rc/activity?id -> {working:bool, activity?:string, currentTool?:string} (live probe)
 *   GET  /api/rc/reply?id    -> {claude_session_id,content,role,timestamp}
 *   GET  /api/rc/attention?id -> {pending,kind,id,text?,questions?,actions?}
 *   POST /api/rc/attention/respond {sessionId,attentionId,action,answers}
 *   POST /api/rc/ask         {sessionId,text,delivery} -> 202 {requestId,sessionId,status}
 *   POST /api/rc/slash       {sessionId,text} -> same
 *   POST /api/rc/interrupt   {sessionId} -> {sessionId,stopped,state}
 *   GET  /api/rc/events      (SSE) -> ask-state | reply | slash-result | attention-result
 *   GET  /api/rc/push/config    -> {enabled,publicKey?}
 *   POST /api/rc/push/subscribe   = PushSubscription JSON
 *   POST /api/rc/push/presence    {focused:bool}
 *   /ws/session/<id>?token=    terminal WebSocket (consumed by /terminal.html, the embedded xterm.js page)
 *
 * SSE/WebSocket cannot set headers, so the token is appended as ?token=.
 */
(() => {
  'use strict';

  // ---------------------------------------------------------------------------
  // Token + auth
  // ---------------------------------------------------------------------------
  const TOKEN_KEY = 'deck_remote_token';
  const PREFS_KEY = 'deck_remote_prefs';
  const COLLAPSE_KEY = 'deck_remote_group_collapse'; // {groupName: bool} manual overrides
  const PROFILE_KEY = 'deck_remote_profile'; // selected agent-deck profile (empty = daemon default)

  // Multi-profile state. The CLI-first /api/rc/* surface is per-call profile-
  // switchable; the proxied terminal + Web Push stay bound to PROXY_PROFILE
  // (one agent-deck web instance). See /api/rc/profiles + main.go.
  let CURRENT_PROFILE = localStorage.getItem(PROFILE_KEY) || '';
  let PROXY_PROFILE = '';
  let PROFILES = [];

  function readToken() {
    const url = new URL(location.href);
    const fromUrl = url.searchParams.get('token');
    if (fromUrl) {
      localStorage.setItem(TOKEN_KEY, fromUrl);
      // Strip the token from the address bar so it isn't left in history.
      url.searchParams.delete('token');
      history.replaceState(null, '', url.pathname + url.search + url.hash);
      return fromUrl;
    }
    return localStorage.getItem(TOKEN_KEY) || '';
  }

  let TOKEN = readToken();

  function authHeaders(extra) {
    return Object.assign({ Authorization: 'Bearer ' + TOKEN }, extra || {});
  }

  // tokenURL appends the bearer as a query param (for SSE / WS which can't set headers).
  function tokenURL(path) {
    const u = new URL(path, location.origin);
    u.searchParams.set('token', TOKEN);
    return u.toString();
  }

  // withProfileParam scopes a /api/rc/* GET path to the selected profile, except
  // for profile-independent endpoints (profiles list, push, events stream).
  function withProfileParam(path) {
    if (!CURRENT_PROFILE) return path;
    if (!path.startsWith('/api/rc/')) return path;
    if (path.startsWith('/api/rc/profiles') || path.startsWith('/api/rc/push') || path.startsWith('/api/rc/events')) return path;
    return path + (path.includes('?') ? '&' : '?') + 'profile=' + encodeURIComponent(CURRENT_PROFILE);
  }

  async function api(method, path, body) {
    const opts = { method, headers: authHeaders() };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(withProfileParam(path), opts);
    if (res.status === 401) {
      forgetToken('Token rejected — re-enter it.');
      throw new Error('unauthorized');
    }
    let data = null;
    const text = await res.text();
    if (text) { try { data = JSON.parse(text); } catch (_) { data = { raw: text }; } }
    if (!res.ok) {
      const msg = (data && (data.error || data.raw)) || ('HTTP ' + res.status);
      const e = new Error(msg);
      e.status = res.status;
      e.data = data;
      throw e;
    }
    return data;
  }

  // ---------------------------------------------------------------------------
  // Small DOM helpers
  // ---------------------------------------------------------------------------
  const $ = (sel, root) => (root || document).querySelector(sel);
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }
  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }
  function fmtTime(ts) {
    let d;
    if (typeof ts === 'number') d = new Date(ts * 1000);
    else if (ts) d = new Date(ts);
    else d = new Date();
    if (isNaN(d.getTime())) d = new Date();
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  }

  // ---------------------------------------------------------------------------
  // Preferences (notify toggles)
  // ---------------------------------------------------------------------------
  function loadPrefs() {
    let p = {};
    try { p = JSON.parse(localStorage.getItem(PREFS_KEY) || '{}'); } catch (_) {}
    // Default to "only notify when a session needs you": approve (waiting on a
    // permission prompt) + stall (a frozen/hung spinner) + error ON; finished
    // (every completed turn) and idle OFF — turn-completion pings are opt-in.
    // sortGroupsByActivity ON by default (most-recent/working groups float up).
    return Object.assign({ approve: true, finished: false, stall: true, error: true, idle: false, quietOn: false, quietStart: '22:00', quietEnd: '07:00', sortGroupsByActivity: true }, p);
  }
  function savePrefs(p) { localStorage.setItem(PREFS_KEY, JSON.stringify(p)); }
  let prefs = loadPrefs();

  // Per-group manual collapse overrides. A value here OVERRIDES the stale default
  // (true=collapsed, false=expanded). Absent => follow the stale default.
  function loadCollapse() {
    let m = {};
    try { m = JSON.parse(localStorage.getItem(COLLAPSE_KEY) || '{}'); } catch (_) {}
    return (m && typeof m === 'object') ? m : {};
  }
  function saveCollapse(m) { localStorage.setItem(COLLAPSE_KEY, JSON.stringify(m)); }
  let collapseOverrides = loadCollapse();

  // ---------------------------------------------------------------------------
  // App state
  // ---------------------------------------------------------------------------
  const state = {
    sessions: new Map(),      // id -> session object
    convos: new Map(),        // id -> array of turn entries {role, content, ts, requestId, kind}
    pending: new Map(),       // id -> {requestId, ctx} active turn
    history: new Map(),       // id -> {loaded, hasMore, loading} scroll-back paging state
    pushConfig: null,         // {enabled, publicKey}
    openSheetId: null,        // session id whose sheet is open
    activityTimer: null,      // interval id for the open sheet's live-activity poll
    convoScrollHandler: null, // scroll listener attached to the open sheet's #convo
    swipeHandlers: null,      // {sheet,onStart,onMove,onEnd} swipe-dismiss listeners
    lastWorking: new Map(),   // id -> last-seen working flag from the activity poll
    reconciling: new Set(),   // ids with an in-flight transcript-tail reconcile
  };

  // How often the open detail sheet probes /api/rc/activity for live state.
  const ACTIVITY_MS = 3000;
  // How many transcript-tail messages to fetch when reconciling a finished turn
  // into the open thread (catches replies driven from the agent-deck TUI, not
  // just PWA-originated /api/rc/ask turns).
  const RECONCILE_TAIL = 12;
  // How many history messages to load per page (open-load and each scroll-up).
  const HISTORY_PAGE = 30;
  // Pixels from the top of the convo that trigger an older-window fetch.
  const SCROLL_TOP_THRESHOLD = 80;

  // ---------------------------------------------------------------------------
  // Token screen
  // ---------------------------------------------------------------------------
  const tokenScreen = $('#tokenScreen');
  const appEl = $('#app');

  function showTokenScreen(errMsg) {
    tokenScreen.hidden = false;
    appEl.hidden = true;
    $('#tokenErr').textContent = errMsg || '';
    $('#tokenInput').value = '';
  }
  function forgetToken(msg) {
    localStorage.removeItem(TOKEN_KEY);
    TOKEN = '';
    teardownStreams();
    showTokenScreen(msg);
  }
  $('#tokenSave').addEventListener('click', () => {
    const v = $('#tokenInput').value.trim();
    if (!v) { $('#tokenErr').textContent = 'Enter a token.'; return; }
    localStorage.setItem(TOKEN_KEY, v);
    TOKEN = v;
    tokenScreen.hidden = true;
    appEl.hidden = false;
    boot();
  });
  $('#tokenInput').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') $('#tokenSave').click();
  });

  // ---------------------------------------------------------------------------
  // Tab navigation
  // ---------------------------------------------------------------------------
  function showTab(tab) {
    $('#screenDeck').hidden = tab !== 'deck';
    $('#screenInbox').hidden = tab !== 'inbox';
    $('#screenSettings').hidden = tab !== 'settings';
    $$('.tab').forEach((t) => t.classList.toggle('active', t.dataset.tab === tab));
    if (tab === 'settings') { refreshPushUI(); refreshDevicesUI(); }
    if (tab === 'inbox') { renderInbox(); loadInbox(); }
  }
  $$('.tab').forEach((t) => t.addEventListener('click', () => showTab(t.dataset.tab)));
  $('#goRefresh').addEventListener('click', async () => {
    const b = $('#goRefresh');
    b.classList.add('busy');
    try { await loadSessions(false); } finally { b.classList.remove('busy'); }
  });

  // ---------------------------------------------------------------------------
  // Inbox: every notification the server recorded, with its full text (the OS
  // notification is a truncated preview). A tapped notification lands on its
  // entry here. Read state is per device: the newest timestamp seen.
  // ---------------------------------------------------------------------------
  const INBOX_SEEN_KEY = 'dr-inbox-seen-at';
  const INBOX_KIND = { approval: '🔐 Approval', question: '❓ Question', reply: '✅ Reply', stall: '🧊 Stalled', test: '🧪 Test' };
  const inbox = { items: [], loaded: false, focusId: null, openIds: new Set() };

  function inboxSeenAt() { return Number(localStorage.getItem(INBOX_SEEN_KEY) || 0); }
  function renderInboxBadge() {
    const b = $('#inboxBadge');
    if (!b) return;
    const seen = inboxSeenAt();
    const n = inbox.items.filter((x) => x.at > seen).length;
    b.hidden = n === 0;
    b.textContent = n > 99 ? '99+' : String(n);
  }
  function markInboxSeen() {
    const newest = inbox.items.length ? inbox.items[0].at : 0;
    if (newest > inboxSeenAt()) localStorage.setItem(INBOX_SEEN_KEY, String(newest));
    renderInboxBadge();
  }
  async function loadInbox() {
    try {
      const r = await api('GET', '/api/rc/notifications?limit=200');
      inbox.items = (r && r.notifications) || [];
      inbox.loaded = true;
    } catch (_) { /* keep what we have; api() already surfaced auth problems */ }
    renderInboxBadge();
    if (!$('#screenInbox').hidden) renderInbox();
  }
  function addInboxItem(n) {
    if (!n || !n.id || inbox.items.some((x) => x.id === n.id)) return;
    inbox.items.unshift(n);
    if (inbox.items.length > 200) inbox.items.length = 200;
    renderInboxBadge();
    if (!$('#screenInbox').hidden) renderInbox();
  }
  function fmtWhen(ms) {
    const d = new Date(ms);
    const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    return d.toDateString() === new Date().toDateString()
      ? time
      : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + time;
  }
  function renderInbox() {
    const list = $('#inboxList');
    if (!list) return;
    if (!inbox.items.length) {
      list.innerHTML = '<div class="inbox-empty">' + (inbox.loaded ? 'No notifications yet.' : 'Loading…') + '</div>';
      return;
    }
    const seen = inboxSeenAt();
    const focus = inbox.focusId;
    if (focus) inbox.openIds.add(focus);
    list.innerHTML = '';
    let focusCard = null;
    for (const n of inbox.items) {
      const cls = 'card notif' + (n.at > seen ? ' unread' : '') + (inbox.openIds.has(n.id) ? ' open' : '') + (focus === n.id ? ' flash' : '');
      const card = el('div', cls);
      card.dataset.id = n.id;
      const known = !n.sessionId || !state.sessions.size || state.sessions.has(n.sessionId);
      card.innerHTML =
        '<div class="card-top"><div class="card-title">' + esc(n.title || 'deck-remote') + '</div>' +
        '<div class="card-meta">' + esc(fmtWhen(n.at)) + '</div></div>' +
        '<div class="card-meta notif-kind">' + esc(INBOX_KIND[n.kind] || n.kind || '') + '</div>' +
        '<div class="notif-body">' + renderMarkdown(n.body || '') + '</div>' +
        (n.sessionId
          ? '<div class="notif-actions"><button class="notif-open" data-open="' + esc(n.sessionId) + '"' + (known ? '' : ' disabled') + '>' +
            (known ? 'Open session' : 'Session no longer listed') + '</button></div>'
          : '');
      card.addEventListener('click', (e) => {
        const btn = e.target.closest('[data-open]');
        if (btn) { e.stopPropagation(); openSession(btn.dataset.open); return; }
        if (inbox.openIds.has(n.id)) inbox.openIds.delete(n.id); else inbox.openIds.add(n.id);
        card.classList.toggle('open');
      });
      if (focus === n.id) focusCard = card;
      list.appendChild(card);
    }
    if (focusCard) {
      focusCard.scrollIntoView({ block: 'center' });
      inbox.focusId = null;
    } else if (focus && inbox.loaded) {
      inbox.focusId = null; // not in the recorded window (older than the last 200)
    }
    markInboxSeen();
  }
  // A tapped notification: show its inbox entry, expanded and highlighted.
  function openNotification(id) {
    inbox.focusId = id || null;
    showTab('inbox');
  }
  // Open a session's sheet, loading the session list first when the app was
  // cold-started from a notification (openSheet is a no-op for unknown ids).
  async function openSession(id) {
    if (!id) return;
    if (!state.sessions.has(id)) {
      try { await loadSessions(false); } catch (_) {}
    }
    if (!state.sessions.has(id)) {
      toast('Session not found', 'It may have been removed, or it belongs to another profile.');
      return;
    }
    showTab('deck');
    if (state.openSheetId !== id) openSheet(id);
  }
  $('#inboxRefresh').addEventListener('click', () => loadInbox());

  // ---------------------------------------------------------------------------
  // Toast (foreground notify + errors)
  // ---------------------------------------------------------------------------
  let toastTimer = null;
  function toast(title, body, onTap) {
    const old = $('.toast');
    if (old) old.remove();
    const t = el('div', 'toast');
    t.innerHTML =
      '<div class="pt-head"><span class="pt-ico">d</span> DECK-REMOTE · now</div>' +
      '<div class="pt-title">' + esc(title) + '</div>' +
      (body ? '<div class="pt-body">' + esc(body) + '</div>' : '');
    if (onTap) t.addEventListener('click', () => { t.remove(); onTap(); });
    document.body.appendChild(t);
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.remove(), 5000);
  }

  // ---------------------------------------------------------------------------
  // Dashboard render
  // ---------------------------------------------------------------------------
  function groupKey(s) {
    // Prefer explicit group; fall back to the leading path segment as a tree.
    if (s.group) return s.group;
    if (s.path) {
      const parts = s.path.replace(/^~\/?/, '').split('/').filter(Boolean);
      if (parts.length >= 2) return parts.slice(0, -1).join(' / ');
      if (parts.length === 1) return parts[0];
    }
    return 'ungrouped';
  }

  function replyLine(s) {
    // Hero detail = the session's real last reply (one-line preview).
    // Prefer a fresher reply we've already seen this session, else the
    // backend-provided lastReply. May be empty for non-Claude / unavailable.
    const pend = state.pending.get(s.id);
    if (pend) return pend.ctx ? 'working… ' + pend.ctx : 'working…';
    const conv = state.convos.get(s.id);
    if (conv && conv.length) {
      for (let i = conv.length - 1; i >= 0; i--) {
        if (conv[i].role === 'reply' && conv[i].content) return oneLine(conv[i].content, 200);
      }
    }
    return oneLine(s.lastReply || '', 200);
  }

  function oneLine(s, max) {
    const t = String(s || '').replace(/\s+/g, ' ').trim();
    const m = max || 64;
    return t.length > m ? t.slice(0, m - 1) + '…' : t;
  }

  // Defensive client guard: the server sanitizes reply content (cleanReplyContent),
  // but if a raw/structured payload (a serialized tool_use block or a literal JSON
  // array of content blocks) ever slips through, never render it verbatim. Only an
  // Anthropic content-block payload (typed blocks) is reduced to its text (or ''
  // when text-less); plain text, malformed JSON, and genuine JSON the model itself
  // produced ([1,2,3], {"foo":"bar"}) are returned unchanged so a real answer is
  // never blanked.
  function looksLikeContentBlocks(v) {
    if (Array.isArray(v)) {
      return v.length > 0 && v.every((el) => el && typeof el === 'object' && !Array.isArray(el) && 'type' in el);
    }
    return !!v && typeof v === 'object' && 'type' in v;
  }
  function cleanReplyContent(s) {
    const str = String(s || '');
    const t = str.trim();
    if (!t || (t[0] !== '{' && t[0] !== '[')) return str;
    let v;
    try { v = JSON.parse(t); } catch (e) { return str; } // malformed: leave as-is
    if (!looksLikeContentBlocks(v)) return str; // genuine JSON answer — keep verbatim
    const blocks = Array.isArray(v) ? v : [v];
    const out = [];
    for (const b of blocks) {
      if (b && typeof b === 'object' && b.type === 'text' && b.text) out.push(b.text);
    }
    return out.join('\n').trim();
  }

  const STALE_S = 86400; // 24h — older groups default to collapsed.

  // Relative-age label from a unix-seconds timestamp. "now" when working,
  // "—" when never (0/absent), else compact "5m" / "3h" / "7d ago".
  function relAge(lastActivity, working) {
    if (working) return 'now';
    const t = Number(lastActivity) || 0;
    if (!t) return '—';
    const ageS = Date.now() / 1000 - t;
    if (ageS < 60) return 'now';
    if (ageS < 3600) return Math.floor(ageS / 60) + 'm';
    if (ageS < 86400) return Math.floor(ageS / 3600) + 'h';
    return Math.floor(ageS / 86400) + 'd ago';
  }

  // Needs-attention sessions outrank working sessions, then recency.
  function groupRecency(list) {
    let anyWorking = false;
    let needsAttention = false;
    let max = 0;
    for (const s of list) {
      if (s.working) anyWorking = true;
      if (s.needsAttention) needsAttention = true;
      const t = Number(s.lastActivity) || 0;
      if (t > max) max = t;
    }
    return { working: anyWorking, needsAttention, recency: needsAttention ? Infinity : (anyWorking ? Number.MAX_VALUE : max), last: max };
  }

  // Effective collapsed state: a working group is never collapsed; a manual
  // override wins; otherwise default-collapse only when stale (>24h).
  function isGroupCollapsed(name, info) {
    if (info.working || info.needsAttention) return false;
    if (Object.prototype.hasOwnProperty.call(collapseOverrides, name)) {
      return !!collapseOverrides[name];
    }
    return info.last > 0 ? (Date.now() / 1000 - info.last) > STALE_S : true;
  }

  // A currentTool starting with "Task(" means a subagent — relabel it for humans.
  function toolLabel(tool) {
    const t = String(tool || '').trim();
    if (!t) return '';
    if (/^Task\(/.test(t)) return 'subagent: ' + t.replace(/^Task\(\s*/, '').replace(/\)\s*$/, '');
    return t;
  }

  // The deck leads with state, not the tree: Needs you → Working → Recent
  // (active in the last RECENT_S) → the rest in their folded tree groups.
  const RECENT_S = 86400;

  function deckSection(deck, title, list, cls) {
    if (!list.length) return;
    const sec = el('section', 'deck-section' + (cls ? ' ' + cls : ''));
    const head = el('div', 'section-head');
    head.innerHTML = '<span class="section-title">' + esc(title) + '</span><span class="count">' + list.length + '</span>';
    sec.appendChild(head);
    list.forEach((s) => sec.appendChild(cardEl(s)));
    deck.appendChild(sec);
  }

  function renderDeck() {
    const deck = $('#deck');
    const all = Array.from(state.sessions.values());

    if (!all.length) {
      deck.innerHTML = '';
      const ph = el('div', 'placeholder');
      ph.innerHTML = '<div class="big">🌙</div>No sessions yet.<br>Start one in agent-deck and it shows up here.';
      deck.appendChild(ph);
      return;
    }

    deck.innerHTML = '';
    const byTitle = (a, b) => (a.title || a.id).localeCompare(b.title || b.id);
    const byRecent = (a, b) => (Number(b.lastActivity) || 0) - (Number(a.lastActivity) || 0) || byTitle(a, b);
    const cut = Date.now() / 1000 - RECENT_S;
    const needs = all.filter((s) => s.needsAttention).sort(byRecent);
    const working = all.filter((s) => !s.needsAttention && s.working).sort(byTitle);
    const recent = all.filter((s) => !s.needsAttention && !s.working && (Number(s.lastActivity) || 0) > cut).sort(byRecent);
    const placed = new Set(needs.concat(working, recent).map((s) => s.id));
    const older = all.filter((s) => !placed.has(s.id));

    deckSection(deck, 'Needs you', needs, 'sec-needs');
    deckSection(deck, 'Working', working, 'sec-work');
    deckSection(deck, 'Recent', recent);
    if (!older.length) return;
    if (placed.size) {
      const head = el('div', 'section-head older-head');
      head.innerHTML = '<span class="section-title">Older</span><span class="count">' + older.length + '</span>';
      deck.appendChild(head);
    }

    // Older: by tree/group, folded by default (stale), as before.
    const groups = new Map();
    older.forEach((s) => {
      const k = groupKey(s);
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(s);
    });

    // Precompute per-group recency so we can sort and label headers.
    const meta = new Map(); // name -> {info, list}
    groups.forEach((list, k) => {
      const sorted = list.slice().sort(byTitle);
      meta.set(k, { info: groupRecency(sorted), list: sorted });
    });

    // Ordering: by activity (most-recent first, "never" last) when the pref is
    // ON; else alphabetical. Ties break alphabetically.
    let keys = Array.from(groups.keys());
    if (prefs.sortGroupsByActivity) {
      keys.sort((a, b) => {
        const rb = meta.get(b).info.recency, ra = meta.get(a).info.recency;
        if (rb !== ra) return rb - ra;
        return a.localeCompare(b);
      });
    } else {
      keys.sort((a, b) => a.localeCompare(b));
    }

    keys.forEach((k) => {
      const { info, list } = meta.get(k);
      const collapsed = isGroupCollapsed(k, info);

      const grp = el('div', 'group' + (collapsed ? ' collapsed' : ''));
      const head = el('div', 'group-head');
      head.setAttribute('role', 'button');
      head.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      const age = relAge(info.last, info.working);
      head.innerHTML =
        '<span class="chev">' + (collapsed ? '▸' : '▾') + '</span>' +
        '<span class="tree">' + esc(k) + '</span>' +
        '<span class="age">' + esc(age) + '</span><span class="count">' + list.length + '</span>';
      head.addEventListener('click', () => toggleGroup(k, info));
      grp.appendChild(head);
      if (!collapsed) list.forEach((s) => grp.appendChild(cardEl(s)));
      deck.appendChild(grp);
    });
  }

  // Tap a header to expand/collapse. Records a manual override (overriding the
  // stale default). A working group can't be collapsed.
  function toggleGroup(name, info) {
    const nowCollapsed = isGroupCollapsed(name, info);
    if (!nowCollapsed && (info.working || info.needsAttention)) return;
    collapseOverrides[name] = !nowCollapsed;
    saveCollapse(collapseOverrides);
    renderDeck();
  }

  // Per-session "last looked at" (unix s), for the new-reply dot. A session
  // seen for the first time counts as read up to its current activity.
  const SEEN_KEY = 'deckRemote.lastSeen';
  const lastSeen = (() => { try { return JSON.parse(localStorage.getItem(SEEN_KEY)) || {}; } catch (_) { return {}; } })();
  function saveSeen() { try { localStorage.setItem(SEEN_KEY, JSON.stringify(lastSeen)); } catch (_) {} }
  function markSeen(id) { lastSeen[id] = Date.now() / 1000; saveSeen(); }
  function hasNewReply(s) {
    const t = Number(s.lastActivity) || 0;
    if (!t || s.working) return false;
    if (!(s.id in lastSeen)) { lastSeen[s.id] = t; saveSeen(); return false; }
    return t > lastSeen[s.id];
  }

  // One state per card, one hue per state (left rail): needs you (orange),
  // stalled (yellow), working (blue), new reply (green dot). Idle: no colour.
  function cardState(s) {
    if (s.needsAttention) return 'st-needs';
    if (s.working && s.stalled) return 'st-stall';
    if (s.working) return 'st-work';
    return '';
  }

  function cardEl(s) {
    const st = cardState(s);
    const card = el('div', 'card' + (st ? ' ' + st : ''));
    card.dataset.id = s.id;

    // Title row: title + state (needs-you chip or new-reply dot).
    const top = el('div', 'card-top');
    top.appendChild(el('div', 'card-title', s.title || s.id));
    if (s.needsAttention) top.appendChild(el('span', 'attention-chip', 'needs you'));
    else if (hasNewReply(s)) {
      const dot = el('span', 'new-dot');
      dot.title = 'New reply';
      top.appendChild(dot);
    }
    card.appendChild(top);

    // One meta line: harness · group · age.
    const meta = el('div', 'card-meta');
    const age = relAge(s.lastActivity, s.working);
    meta.textContent = [s.tool || 'agent', groupKey(s), age === '—' ? '' : age].filter(Boolean).join(' · ');
    card.appendChild(meta);

    // Live activity takes over the hero detail while the agent is working;
    // otherwise we fall back to the dim one-line last-reply preview.
    if (s.working) {
      const act = el('div', 'card-activity' + (s.stalled ? ' stalled' : ''));
      // Stalled: the spinner is frozen (no progress across polls). Keep showing
      // it's "working" but mark it so a frozen agent doesn't read as live.
      const spin = s.stalled ? '<span class="stall-dot">⚠</span>' : '<span class="spin"></span>';
      const label = s.stalled ? (esc(s.activity || 'Working…') + ' <span class="stall-tag">stalled</span>')
                              : esc(s.activity || 'Working…');
      act.innerHTML = spin + '<span class="act-text">' + label + '</span>';
      card.appendChild(act);
      const toolText = toolLabel(s.currentTool);
      if (toolText) card.appendChild(el('div', 'card-tool', oneLine(toolText, 72)));
    } else {
      const reply = replyLine(s);
      const r = el('div', 'card-reply' + (reply ? '' : ' empty'), reply || 'No reply yet');
      card.appendChild(r);
    }

    card.addEventListener('click', () => openSheet(s.id));
    return card;
  }

  // ---------------------------------------------------------------------------
  // Action sheet (detail / approve)
  // ---------------------------------------------------------------------------
  function closeSheet() {
    stopActivityPoll(); // stop the live-activity poll so it doesn't leak
    if (state.openSheetId) markSeen(state.openSheetId); // replies read while open
    // Detach the convo scroll listener and clear any in-flight paging flag so a
    // fetch settling after close doesn't touch a stale DOM.
    const convoEl = $('#convo');
    if (convoEl && state.convoScrollHandler) convoEl.removeEventListener('scroll', state.convoScrollHandler);
    state.convoScrollHandler = null;
    // Detach the swipe-dismiss listeners so they can't fire on a stale sheet.
    teardownSwipe();
    const hs = state.openSheetId && state.history.get(state.openSheetId);
    if (hs) hs.loading = false;
    // Drop live-sync bookkeeping for the closing sheet so a reconcile settling
    // after close can't touch a stale thread, and the next open starts clean.
    if (state.openSheetId) {
      state.lastWorking.delete(state.openSheetId);
      state.reconciling.delete(state.openSheetId);
    }
    const scrim = $('#scrim');
    const sheet = $('#sheet');
    if (scrim) scrim.remove();
    if (sheet) sheet.remove();
    state.openSheetId = null;
    // remove session deep-link param
    const url = new URL(location.href);
    if (url.searchParams.has('session')) {
      url.searchParams.delete('session');
      history.replaceState(null, '', url.pathname + url.search + url.hash);
    }
  }

  // Detail-first: the sheet ALWAYS opens in detail mode. The Approve panel is
  // an embedded section, shown only when /api/rc/permission reports a real
  // pending dialog — never inferred from status.
  // iOS doesn't shrink the layout viewport for the on-screen keyboard: the
  // full-height sheet stayed put and the page scrolled the composer into
  // view, pushing the header off screen. Size the open sheet to the visual
  // viewport instead, so the header stays and the composer sits on the
  // keyboard.
  function applyViewport() {
    const vv = window.visualViewport;
    if (!vv) return;
    const kb = vv.height < window.innerHeight - 120;
    const was = document.documentElement.classList.contains('kb');
    document.documentElement.classList.toggle('kb', kb);
    const sheet = $('#sheet');
    if (!sheet) return;
    sheet.style.top = kb ? vv.offsetTop + 'px' : '';
    sheet.style.height = kb ? vv.height + 'px' : '';
    sheet.style.bottom = kb ? 'auto' : '';
    if (kb !== was) {
      const convo = $('#convo');
      if (convo) convo.scrollTop = convo.scrollHeight;
    }
  }
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', applyViewport);
    window.visualViewport.addEventListener('scroll', applyViewport);
  }

  function openSheet(id) {
    const s = state.sessions.get(id);
    if (!s) return;

    if ($('#sheet')) closeSheet();
    state.openSheetId = id;
    markSeen(id);

    const scrim = el('div', 'scrim');
    scrim.id = 'scrim';
    scrim.addEventListener('click', closeSheet);
    document.body.appendChild(scrim);

    const sheet = el('div', 'sheet');
    sheet.id = 'sheet';
    document.body.appendChild(sheet);

    sheet.innerHTML = sheetHeadHTML(s);
    buildDetailSheet(sheet, s);
    applyViewport();
  }

  function sheetHeadHTML(s) {
    // Open the embedded xterm.js terminal page in the same window: an installed
    // iOS PWA opens target=_blank in a separate browser context whose storage
    // lacks the token. Same-origin, so terminal.html reads the token from
    // localStorage; it is deliberately not put in the URL.
    const termURL = '/terminal.html?id=' + encodeURIComponent(s.id);
    // The terminal is reverse-proxied to ONE agent-deck web bound to PROXY_PROFILE,
    // so it can't follow the profile selector. Disable + annotate it honestly when
    // the selected profile differs (rather than opening a terminal for a session
    // the proxy can't see).
    const termOff = CURRENT_PROFILE && PROXY_PROFILE && CURRENT_PROFILE !== PROXY_PROFILE;
    const termLink = termOff
      ? '<a class="sheet-term disabled" title="Terminal only available for the \'' + esc(PROXY_PROFILE) + '\' profile" aria-disabled="true"><svg class="ico" viewBox="0 0 24 24"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="M7 10l3 2-3 2M12 15h5"/></svg></a>'
      : '<a class="sheet-term" title="Open full web terminal" aria-label="Terminal" href="' + termURL + '"><svg class="ico" viewBox="0 0 24 24"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="M7 10l3 2-3 2M12 15h5"/></svg></a>';
    // grabber + header are wrapped in .sheet-handle: this whole region (which
    // sits ABOVE the scrolling convo and never scrolls) is the swipe-down
    // dismiss target, keeping it cleanly separate from the convo's scroll-up
    // history paging.
    return (
      '<div class="sheet-handle" id="sheetHandle">' +
      '<div class="grabber" id="sheetGrab" title="Back to sessions"></div>' +
      '<div class="sheet-head"><div class="sheet-title-row">' +
        '<button class="icon-btn sheet-back" id="sheetBack" title="Back to sessions"><span class="gi">‹</span></button>' +
        '<div style="flex:1;min-width:0">' +
          '<div class="sheet-title">' + esc(s.title || s.id) + '</div>' +
          '<div class="sheet-sub"><span class="harness"><span class="gi">✦</span>' + esc(s.tool || 'agent') + '</span>' +
            '<span>' + esc(groupKey(s)) + '</span></div>' +
        '</div>' +
        '<button class="icon-btn sheet-check" id="sheetCheckPerm" title="Check for a pending request" aria-label="Check for a pending request"><svg class="ico" viewBox="0 0 24 24"><path d="M12 3l7 3v5c0 4.5-3 8-7 10-4-2-7-5.5-7-10V6z"/><path d="M9 12l2 2 4-4"/></svg></button>' +
        termLink +
      '</div></div>' +
      '</div>'
    );
  }

  // ----- detail sheet (ask + last reply + async pending + gated approve) -----
  function buildDetailSheet(sheet, s) {
    // DOM order top→bottom: handle (already in sheet.innerHTML) → convo (flex:1)
    // → approval dock → status strip (live activity + Stop) →
    // composer-wrap (slash popup + composer, pinned to the very bottom).
    const convo = el('div', 'convo');
    convo.id = 'convo';
    sheet.appendChild(convo);

    // Scroll-up paging: when the user nears the top and older history exists,
    // fetch the next older window and prepend it (preserving scroll position).
    const onScroll = () => {
      if (convo.scrollTop <= SCROLL_TOP_THRESHOLD) loadOlderHistory(s.id);
    };
    state.convoScrollHandler = onScroll;
    convo.addEventListener('scroll', onScroll, { passive: true });
    convo.addEventListener('click', onCodeAction);

    // "↓ N new": shown when messages arrive while scrolled up to read.
    const newPill = el('button', 'new-pill');
    newPill.id = 'newPill';
    newPill.type = 'button';
    newPill.hidden = true;
    newPill.addEventListener('click', () => { convo.scrollTop = convo.scrollHeight; hideNewPill(); });
    sheet.appendChild(newPill);
    convo.addEventListener('scroll', () => {
      if (convo.scrollHeight - convo.scrollTop - convo.clientHeight < 80) hideNewPill();
    }, { passive: true });

    // Approval / question dock: between the thread and the composer, so a
    // pending request is on screen when the sheet opens (the thread opens
    // scrolled to the bottom). Populated only when a real pending dialog is
    // detected; scrolls internally up to 62% of the sheet.
    const approveMount = el('div', 'approve-dock');
    approveMount.id = 'approveMount';
    sheet.appendChild(approveMount);

    // Back affordances (standalone PWA has no back gesture, and the full-screen
    // sheet covers the scrim — so an explicit control is required).
    const backBtn = $('#sheetBack', sheet);
    if (backBtn) backBtn.addEventListener('click', closeSheet);
    const grab = $('#sheetGrab', sheet);
    if (grab) grab.addEventListener('click', closeSheet);

    // Wire the header "Check for approval" affordance.
    const checkBtn = $('#sheetCheckPerm', sheet);
    if (checkBtn) checkBtn.addEventListener('click', () => checkAttention(s, true));

    // One status strip directly above the composer (Claude Code style):
    // spinner (or ⚠ when stalled), activity, tool line, and Hold-to-stop when
    // the harness supports interruption. Only .la-main is re-rendered, so a
    // hold in progress on Stop isn't torn out of the DOM by the 3s refresh.
    // Hidden while idle.
    const live = el('div', 'live-activity idle');
    live.id = 'liveActivity';
    live.appendChild(el('div', 'la-main'));
    if (s.capabilities && s.capabilities.interrupt) {
      const stop = el('button', 'hold-stop', 'Stop');
      stop.title = 'Hold to stop the current turn';
      wireHoldToInterrupt(stop, s);
      live.appendChild(stop);
    }
    sheet.appendChild(live);

    // Composer + its slash-suggestion popup (relative anchor for the popup).
    const composerWrap = el('div', 'composer-wrap');

    // Queue/Steer: only meaningful while a turn runs, so the row is shown by
    // renderLiveActivity while working and resets to Queue when hidden.
    let delivery = 'auto';
    if (s.capabilities && s.capabilities.steer) {
      const deliveryRow = el('div', 'delivery-row');
      deliveryRow.id = 'deliveryRow';
      deliveryRow.hidden = true;
      deliveryRow.innerHTML = '<span class="dl-label">While busy</span>' +
        '<div class="seg" role="radiogroup">' +
          '<button type="button" class="seg-opt on" data-v="auto" role="radio" aria-checked="true">Queue</button>' +
          '<button type="button" class="seg-opt" data-v="steer" role="radio" aria-checked="false">Steer</button>' +
        '</div>';
      const pick = (v) => {
        delivery = v;
        $$('.seg-opt', deliveryRow).forEach((b) => {
          const on = b.dataset.v === v;
          b.classList.toggle('on', on);
          b.setAttribute('aria-checked', on ? 'true' : 'false');
        });
      };
      $$('.seg-opt', deliveryRow).forEach((b) => b.addEventListener('click', () => pick(b.dataset.v)));
      deliveryRow.resetDelivery = () => pick('auto');
      composerWrap.appendChild(deliveryRow);
    }

    // Dynamic slash popup: hidden unless the input value starts with '/'.
    const slashPop = el('div', 'slash-pop');
    slashPop.id = 'slashPop';
    slashPop.hidden = true;
    composerWrap.appendChild(slashPop);

    const composer = el('div', 'composer');
    const ta = el('textarea');
    ta.id = 'composerInput';
    ta.rows = 1;
    ta.placeholder = 'Message… (/ for commands)';
    const structuredAsk = !s.capabilities || !!s.capabilities.ask;
    if (!structuredAsk) {
      ta.disabled = true;
      ta.placeholder = s.degradedReason || 'Structured controls unavailable — open terminal';
    }
    ta.addEventListener('input', () => {
      ta.style.height = 'auto';
      ta.style.height = Math.min(ta.scrollHeight, 110) + 'px';
      send.disabled = !ta.value.trim();
      updateSlashPop(s.id, ta);
    });
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); send.click(); }
    });
    // Hide the popup on blur (deferred so a tap on a suggestion still fires).
    ta.addEventListener('blur', () => setTimeout(() => hideSlashPop(), 120));
    const send = el('button', 'send-btn', '↑');
    send.disabled = true;
    send.addEventListener('click', () => {
      const text = ta.value.trim();
      if (!text) return;
      ta.value = '';
      ta.style.height = 'auto';
      send.disabled = true;
      hideSlashPop();
      // A leading-slash message is a slash command; otherwise a normal ask.
      if (text[0] === '/') sendSlash(s.id, text, delivery);
      else sendAsk(s.id, text, delivery);
    });
    // Visible entry to the slash commands (the list used to be discoverable
    // only by typing '/').
    const slashBtn = el('button', 'slash-btn', '/');
    slashBtn.type = 'button';
    slashBtn.title = 'Commands';
    slashBtn.disabled = ta.disabled;
    slashBtn.addEventListener('mousedown', (e) => e.preventDefault()); // keep focus
    slashBtn.addEventListener('click', () => {
      if (ta.disabled) return;
      if (ta.value[0] !== '/') ta.value = '/' + ta.value;
      ta.focus();
      send.disabled = !ta.value.trim();
      updateSlashPop(s.id, ta);
    });
    composer.appendChild(slashBtn);
    composer.appendChild(ta);
    composer.appendChild(send);
    composerWrap.appendChild(composer);
    sheet.appendChild(composerWrap);

    // Swipe-to-dismiss gestures (swipe-down on the handle, edge-swipe-right).
    wireSwipeDismiss(sheet);

    // Seed the live area from the list snapshot, then poll for fresh state.
    renderLiveActivity(s.id, { working: s.working, activity: s.activity, currentTool: s.currentTool, stalled: s.stalled });
    startActivityPoll(s.id);

    renderConvo(s.id);
    loadHistory(s.id);
    checkAttention(s, false); // questions and approvals are live, guarded forms

    // If the agent is already idle on open, reconcile the transcript tail once
    // shortly after history lands — covers a reply that finished just BEFORE we
    // opened (after this card's last list refresh) so it never produced an SSE
    // event we'd see. Usually a no-op (dedup against the freshly-loaded history).
    if (!s.working) {
      setTimeout(() => {
        if (state.openSheetId === s.id) reconcileTranscriptTail(s.id);
      }, 600);
    }
  }

  // ----- dynamic slash-command popup -----
  // The known slash commands (label + short hint). Tapping or sending one
  // routes through the existing /api/rc/slash path (sendSlash).
  const FALLBACK_SLASH_COMMANDS = [
    { cmd: '/compact', desc: 'condense the conversation' },
    { cmd: '/context', desc: 'show context usage' },
    { cmd: '/clear', desc: 'clear the conversation' },
    { cmd: '/diff', desc: 'show working-tree diff' },
  ];

  function hideSlashPop() {
    const pop = $('#slashPop');
    if (pop) { pop.hidden = true; pop.innerHTML = ''; }
  }

  // Show a suggestion popup when the input starts with '/', filtered by the
  // typed prefix. Tapping a suggestion sends it; hides otherwise.
  function updateSlashPop(id, ta) {
    const pop = $('#slashPop');
    if (!pop) return;
    const v = ta.value;
    // Only a single leading-slash token (no space yet) triggers suggestions.
    if (v[0] !== '/' || /\s/.test(v)) { hideSlashPop(); return; }
    const q = v.toLowerCase();
    const session = state.sessions.get(id);
    const advertised = session && session.capabilities && session.capabilities.slashCommands;
    const commands = Array.isArray(advertised) && advertised.length
      ? advertised.map((c) => ({ cmd: c.command, desc: c.description }))
      : FALLBACK_SLASH_COMMANDS;
    const matches = commands.filter((c) => c.cmd.toLowerCase().indexOf(q) === 0);
    if (!matches.length) { hideSlashPop(); return; }
    pop.innerHTML = '';
    matches.forEach((c) => {
      const opt = el('div', 'slash-opt');
      opt.innerHTML = '<span class="cmd">' + esc(c.cmd) + '</span><span class="desc">' + esc(c.desc) + '</span>';
      // mousedown (not click) fires before the textarea's blur, so the tap
      // isn't lost to the deferred blur-hide.
      opt.addEventListener('mousedown', (e) => {
        e.preventDefault();
        selectSlash(id, ta, c.cmd);
      });
      pop.appendChild(opt);
    });
    pop.hidden = false;
  }

  // A picked slash command: clear the input, hide the popup, and send it.
  function selectSlash(id, ta, cmd) {
    ta.value = '';
    ta.style.height = 'auto';
    const send = ta.parentElement && ta.parentElement.querySelector('.send-btn');
    if (send) send.disabled = true;
    hideSlashPop();
    sendSlash(id, cmd, 'auto');
  }

  // ----- swipe-to-dismiss gestures -----
  // Two iOS-standard gestures dismiss the sheet, complementing the ‹ back button:
  //   1) swipe DOWN starting on the header/grab-handle region (#sheetHandle) —
  //      bound to the handle, NOT the convo, because the convo's top edge is used
  //      for scroll-up history paging (a down-drag there must keep scrolling, not
  //      close). The two gestures can't collide: the handle never scrolls.
  //   2) swipe RIGHT from the very left screen edge (touchstart clientX < ~30px).
  // A cheap translate follows the finger; release past ~60px closes, else snaps
  // back. Listeners live on `sheet` and are torn down in closeSheet().
  const SWIPE_DISMISS = 60;   // px past which a release dismisses
  const EDGE_ZONE = 30;       // px from the left edge that arms edge-back
  const SWIPE_SLOP = 10;      // px before we commit to a drag axis

  function wireSwipeDismiss(sheet) {
    let mode = null;          // null | 'down' | 'edge' | 'reject'
    let startX = 0, startY = 0, dx = 0, dy = 0;

    function onStart(e) {
      if (e.touches.length !== 1) { mode = 'reject'; return; }
      const t = e.touches[0];
      startX = t.clientX; startY = t.clientY; dx = 0; dy = 0;
      // Edge-back arms anywhere on the sheet when the touch begins at the left
      // screen edge. The down-swipe arms only on the header/handle region.
      const onHandle = !!(e.target.closest && e.target.closest('#sheetHandle'));
      if (startX < EDGE_ZONE) mode = 'edge';
      else if (onHandle) mode = 'down';
      else mode = null; // undecided; resolve direction on first move
    }

    function onMove(e) {
      if (mode === 'reject') return;
      const t = e.touches[0];
      dx = t.clientX - startX;
      dy = t.clientY - startY;
      if (mode === null) {
        // Only a clearly-downward drag that STARTED on the handle could be here
        // (non-handle starts away from the edge are left to normal scrolling).
        const onHandle = !!(e.target.closest && e.target.closest('#sheetHandle'));
        if (Math.abs(dy) < SWIPE_SLOP && Math.abs(dx) < SWIPE_SLOP) return;
        if (onHandle && dy > Math.abs(dx)) mode = 'down';
        else { mode = 'reject'; return; }
      }
      if (mode === 'down') {
        if (dy <= 0) { setSheetDrag(sheet, 0); return; } // ignore upward
        e.preventDefault();
        setSheetDrag(sheet, dy);
      } else if (mode === 'edge') {
        // Commit to horizontal only; a mostly-vertical move releases the gesture.
        if (Math.abs(dy) > Math.abs(dx) && Math.abs(dy) > SWIPE_SLOP) { mode = 'reject'; clearSheetDrag(sheet); return; }
        if (dx <= 0) { setSheetDrag(sheet, 0); return; }
        e.preventDefault();
        setSheetDrag(sheet, 0, dx);
      }
    }

    function onEnd() {
      if (mode === 'down' && dy > SWIPE_DISMISS) { closeSheet(); return; }
      if (mode === 'edge' && dx > SWIPE_DISMISS) { closeSheet(); return; }
      // Not past threshold: snap back to rest.
      if (mode === 'down' || mode === 'edge') snapSheetBack(sheet);
      mode = null;
    }

    sheet.addEventListener('touchstart', onStart, { passive: true });
    sheet.addEventListener('touchmove', onMove, { passive: false });
    sheet.addEventListener('touchend', onEnd, { passive: true });
    sheet.addEventListener('touchcancel', onEnd, { passive: true });
    // Stash for teardown in closeSheet.
    state.swipeHandlers = { sheet, onStart, onMove, onEnd };
  }

  function setSheetDrag(sheet, ty, tx) {
    sheet.classList.add('dragging');
    sheet.classList.remove('snapback');
    sheet.style.transform = 'translate(' + (tx || 0) + 'px,' + (ty || 0) + 'px)';
  }
  function clearSheetDrag(sheet) {
    sheet.classList.remove('dragging');
    sheet.style.transform = '';
  }
  function snapSheetBack(sheet) {
    sheet.classList.remove('dragging');
    sheet.classList.add('snapback');
    sheet.style.transform = '';
    setTimeout(() => sheet.classList.remove('snapback'), 220);
  }

  function teardownSwipe() {
    const h = state.swipeHandlers;
    if (!h || !h.sheet) { state.swipeHandlers = null; return; }
    h.sheet.removeEventListener('touchstart', h.onStart);
    h.sheet.removeEventListener('touchmove', h.onMove);
    h.sheet.removeEventListener('touchend', h.onEnd);
    h.sheet.removeEventListener('touchcancel', h.onEnd);
    state.swipeHandlers = null;
  }

  // ----- live activity (detail sheet) -----
  // Render the prominent "what it's doing now" block. When working, show the
  // thinking/processing line + currentTool (Task(…) → subagent). When idle,
  // show a subtle waiting note; the full last reply still renders in the convo.
  function renderLiveActivity(id, a) {
    if (state.openSheetId !== id) return;
    const live = $('#liveActivity');
    if (!live) return;
    const main = $('.la-main', live);
    const working = !!(a && a.working);
    const dr = $('#deliveryRow');
    if (dr) {
      if (dr.hidden === working && !working && dr.resetDelivery) dr.resetDelivery();
      dr.hidden = !working;
    }
    if (!working) {
      live.className = 'live-activity idle';
      // Re-arm Stop for the next turn (it stays disabled after firing).
      const stop = $('.hold-stop', live);
      if (stop) { stop.disabled = false; stop.classList.remove('armed'); stop.textContent = 'Stop'; }
      return;
    }
    const stalled = !!a.stalled;
    live.className = 'live-activity working' + (stalled ? ' stalled' : '');
    const toolText = toolLabel(a.currentTool);
    // Stalled: spinner frozen across polls. Still "working", but flagged so a
    // hung agent doesn't masquerade as live.
    const lead = stalled ? '<span class="stall-dot">⚠</span>' : '<span class="spin"></span>';
    const tag = stalled ? ' <span class="stall-tag">stalled</span>' : '';
    main.innerHTML =
      '<div class="la-row">' + lead +
        '<span class="la-text">' + esc(a.activity || 'Working…') + tag + '</span></div>' +
      (toolText ? '<div class="la-tool">' + esc(oneLine(toolText, 90)) + '</div>' : '');
  }

  function wireHoldToInterrupt(button, s) {
    const HOLD_MS = 900;
    let timer = null, fired = false;
    const begin = (e) => {
      e.preventDefault(); fired = false; button.classList.add('armed'); button.textContent = 'Hold…';
      timer = setTimeout(async () => {
        fired = true; button.disabled = true; button.textContent = 'Stopping…';
        try {
          const r = await api('POST', '/api/rc/interrupt', { sessionId: s.id, profile: CURRENT_PROFILE });
          button.textContent = r && r.stopped ? 'Stopped' : 'Stop sent';
        } catch (err) {
          button.disabled = false; button.classList.remove('armed'); button.textContent = 'Stop';
          toast(s.title || 'Session', 'Interrupt failed: ' + err.message);
        }
      }, HOLD_MS);
    };
    const cancel = () => {
      if (fired) return;
      clearTimeout(timer); button.classList.remove('armed'); button.textContent = 'Stop';
    };
    button.addEventListener('mousedown', begin);
    button.addEventListener('touchstart', begin, { passive: false });
    ['mouseup', 'mouseleave', 'touchend', 'touchcancel'].forEach((ev) => button.addEventListener(ev, cancel));
  }

  function startActivityPoll(id) {
    stopActivityPoll();
    state.lastWorking.set(id, undefined); // unknown until the first probe lands
    async function probe() {
      if (state.openSheetId !== id) { stopActivityPoll(); return; }
      try {
        const a = await api('GET', '/api/rc/activity?id=' + encodeURIComponent(id));
        if (state.openSheetId !== id) return;
        renderLiveActivity(id, a || { working: false });
        // Keep the list snapshot in sync so the card reflects the same state.
        const s = state.sessions.get(id);
        if (s) {
          s.working = !!(a && a.working);
          s.activity = (a && a.activity) || '';
          s.currentTool = (a && a.currentTool) || '';
          s.stalled = !!(a && a.stalled);
        }
        // Live thread sync (catch-all for turns NOT originated by this PWA — e.g.
        // prompts entered in the agent-deck TUI, or back-to-back turns where the
        // working flag never visibly toggles between two 3s polls). Reconcile on
        // EVERY probe: reconcileTranscriptTail coalesces overlapping calls and
        // dedups by content, so it's a cheap no-op when nothing new landed, and the
        // thread stays within one poll (~3s) of the transcript. (A transition-only
        // trigger missed turns that completed during a continuous "working" run.)
        state.lastWorking.set(id, !!(a && a.working));
        reconcileTranscriptTail(id);
      } catch (_) {
        // Graceful: on error just fall back to idle, don't spam.
        if (state.openSheetId === id) renderLiveActivity(id, { working: false });
      }
    }
    probe();
    state.activityTimer = setInterval(probe, ACTIVITY_MS);
  }

  function stopActivityPoll() {
    if (state.activityTimer) { clearInterval(state.activityTimer); state.activityTimer = null; }
  }

  function ensureConvo(id) {
    if (!state.convos.has(id)) state.convos.set(id, []);
    return state.convos.get(id);
  }

  async function loadReply(id) {
    try {
      const r = await api('GET', '/api/rc/reply?id=' + encodeURIComponent(id));
      const content = r ? cleanReplyContent(r.content) : '';
      if (content) {
        const conv = ensureConvo(id);
        // Only seed if we don't already have this as the latest reply.
        const hasReply = conv.some((e) => e.role === 'reply' && e.content === content);
        if (!hasReply) {
          conv.push({ role: 'reply', content, ts: r.timestamp });
          renderConvo(id);
        }
      }
    } catch (e) {
      // Non-fatal: show a soft note in the convo.
      if (state.openSheetId === id) {
        const conv = ensureConvo(id);
        if (!conv.length) {
          conv.push({ role: 'note', content: 'No reply yet (' + e.message + ').' });
          renderConvo(id);
        }
      }
    }
  }

  // Map a history entry (role "user"|"reply"|"commentary", unix-seconds ts)
  // onto a convo turn. Commentary is reply-shaped but remains a distinct row.
  function historyToTurn(m) {
    const role = m.role === 'user' ? 'me' : 'reply';
    const content = role === 'reply' ? cleanReplyContent(m.content) : (m.content || '');
    // hist: from the transcript, so a fresh history page may replace it.
    return { role, content, ts: m.ts, kind: m.role === 'commentary' ? 'commentary' : '', hist: true };
  }

  // Open-load: fetch the most recent history window and seed the convo with it.
  // Falls back to the legacy last-reply behavior when there's no transcript
  // (non-Claude sessions return {messages:[], hasMore:false}).
  async function loadHistory(id) {
    state.history.set(id, { loaded: 0, hasMore: false, loading: true });
    let r;
    try {
      r = await api('GET', '/api/rc/history?id=' + encodeURIComponent(id) +
        '&limit=' + HISTORY_PAGE + '&offset=0');
    } catch (e) {
      // Graceful: don't wipe the thread. Fall back to the last-reply behavior.
      const hs = state.history.get(id);
      if (hs) hs.loading = false;
      if (state.openSheetId === id) loadReply(id);
      return;
    }
    if (state.openSheetId !== id) return; // sheet closed while in flight
    const msgs = (r && r.messages) || [];
    if (msgs.length) {
      // Seed the convo from history (oldest-first), preserving any in-flight
      // pending turn (that's tracked separately in state.pending, so we only
      // need to avoid clobbering it — which we don't, convos != pending).
      // Replace the rows that came from history and keep live ones (queued
      // 'me' rows, SSE replies, errors) the transcript hasn't caught up with.
      // Re-appending the whole old thread duplicated it on every reopen.
      const conv = ensureConvo(id);
      const merged = DeckThread.mergeFreshHistory(msgs.map(historyToTurn), conv);
      conv.length = 0;
      merged.forEach((e) => conv.push(e));
      state.history.set(id, { loaded: msgs.length, hasMore: !!(r && r.hasMore), loading: false });
      renderConvo(id);
      renderDeck();
    } else {
      // No transcript (non-Claude or empty): legacy fallback + subtle note.
      state.history.set(id, { loaded: 0, hasMore: false, loading: false });
      const s = state.sessions.get(id);
      if (s && s.tool && s.tool !== 'claude') {
        const conv = ensureConvo(id);
        if (!conv.some((e) => e.role === 'note' && e.kind === 'no-history')) {
          conv.unshift({ role: 'note', kind: 'no-history', content: 'Full history: open terminal.' });
        }
      }
      renderConvo(id); // drop the loading state; shows the empty-thread note
      loadReply(id);
    }
  }

  // Scroll-up paging: fetch the next older window and prepend it, preserving the
  // visual scroll position. Guarded against concurrent/duplicate fetches.
  async function loadOlderHistory(id) {
    const hs = state.history.get(id);
    if (!hs || !hs.hasMore || hs.loading) return;
    hs.loading = true;
    renderConvo(id); // surface the "Loading…" affordance at the top

    const convo = $('#convo');
    const beforeH = convo ? convo.scrollHeight : 0;
    const beforeT = convo ? convo.scrollTop : 0;

    let r;
    try {
      r = await api('GET', '/api/rc/history?id=' + encodeURIComponent(id) +
        '&limit=' + HISTORY_PAGE + '&offset=' + hs.loaded);
    } catch (e) {
      const cur = state.history.get(id);
      if (cur) { cur.loading = false; renderConvo(id); }
      return;
    }
    if (state.openSheetId !== id) return; // sheet closed while in flight
    const msgs = (r && r.messages) || [];
    const conv = ensureConvo(id);
    // Prepend older messages ahead of the existing thread, after any leading
    // note row so the "Full history" hint stays at the very top.
    let insertAt = 0;
    while (insertAt < conv.length && conv[insertAt].role === 'note') insertAt++;
    // The offset counts back from the newest message, so turns appended since
    // the last page make this one overlap the thread's head: trim it.
    const turns = DeckThread.trimOverlap(msgs.map(historyToTurn), conv.slice(insertAt));
    conv.splice(insertAt, 0, ...turns);
    state.history.set(id, {
      loaded: hs.loaded + msgs.length,
      hasMore: !!(r && r.hasMore),
      loading: false,
    });
    renderConvo(id, { keepScroll: true });

    // Preserve scroll position: the view should stay anchored on the same
    // content, so add the height that was inserted above the viewport.
    if (convo) {
      const afterH = convo.scrollHeight;
      convo.scrollTop = beforeT + (afterH - beforeH);
    }
  }

  // Live thread sync: pull the recent transcript tail and append any messages
  // that aren't already in the open thread. This is the catch-all for turns that
  // weren't originated by this PWA (e.g. a prompt typed in the agent-deck TUI),
  // which never produce an SSE `reply` event here. The SSE fast-path is kept;
  // dedup-by-content below prevents collisions when both fire.
  //
  // Dedup: map each tail message's role (user→'me', reply→'reply') and skip it
  // if an existing convo entry of the SAME mapped role already has the same
  // trimmed content. We deliberately do NOT key on `ts` — live SSE entries use
  // millisecond Date.now()/1000 while transcript ts is whole seconds.
  async function reconcileTranscriptTail(id) {
    if (state.openSheetId !== id) return;
    if (state.reconciling.has(id)) return; // coalesce overlapping reconciles
    state.reconciling.add(id);

    // Decide auto-scroll BEFORE we mutate/render: only stick to the bottom if the
    // user is already near it. If they've scrolled up to read, don't yank them.
    const convo = $('#convo');
    const nearBottom = convo
      ? (convo.scrollHeight - convo.scrollTop - convo.clientHeight) < 80
      : true;

    let r;
    try {
      r = await api('GET', '/api/rc/history?id=' + encodeURIComponent(id) +
        '&limit=' + RECONCILE_TAIL + '&offset=0');
    } catch (_) {
      state.reconciling.delete(id);
      return; // graceful: a poll-driven reconcile failing is a no-op
    } finally {
      state.reconciling.delete(id);
    }
    if (state.openSheetId !== id) return; // sheet closed while in flight

    const msgs = (r && r.messages) || []; // oldest-first
    if (!msgs.length) return;
    const conv = ensureConvo(id);

    // Build a per-role multiset of trimmed contents already present so repeated
    // identical messages still dedup one-for-one (don't drop a genuine repeat).
    const seen = new Map(); // key `${role}\n${kind}\n${trimmedContent}` -> remaining count
    conv.forEach((e) => {
      if (e.role !== 'me' && e.role !== 'reply') return;
      if (e.content == null) return;
      const key = e.role + '\n' + (e.kind || '') + '\n' + String(e.content).trim();
      seen.set(key, (seen.get(key) || 0) + 1);
    });

    let appended = 0;
    msgs.forEach((m) => {
      const turn = historyToTurn(m); // {role:'me'|'reply', content, ts}
      const key = turn.role + '\n' + (turn.kind || '') + '\n' + String(turn.content || '').trim();
      const left = seen.get(key) || 0;
      if (left > 0) { seen.set(key, left - 1); return; } // already in thread
      conv.push(turn);
      appended++;
    });

    if (!appended) return; // common case: nothing new — cheap no-op
    // Keep the scroll-back offset aligned with what the thread holds.
    const hsNow = state.history.get(id);
    if (hsNow) hsNow.loaded += appended;

    if (nearBottom) {
      // User is at the live edge: render normally so it sticks to the bottom.
      renderConvo(id);
    } else {
      // User scrolled up to read: append without yanking the view. renderConvo
      // rebuilds the DOM (resetting scrollTop), so restore their position — the
      // new messages are below the fold and don't shift content above it.
      const beforeT = convo ? convo.scrollTop : 0;
      renderConvo(id, { keepScroll: true });
      if (convo) convo.scrollTop = beforeT;
      showNewPill(appended);
    }
    renderDeck(); // refresh the card's last-reply preview
  }

  function showNewPill(n) {
    const pill = $('#newPill');
    if (!pill) return;
    pill.dataset.count = String((Number(pill.dataset.count) || 0) + n);
    pill.textContent = '↓ ' + pill.dataset.count + ' new';
    pill.hidden = false;
  }
  function hideNewPill() {
    const pill = $('#newPill');
    if (pill) { pill.hidden = true; pill.dataset.count = '0'; }
  }

  // A timestamp closes a run of turns: shown when the next row is from the
  // other side, more than 5 minutes later, or there is no next row.
  function closesRun(e, next) {
    if (!next) return true;
    const side = (r) => (r === 'me' ? 'me' : 'agent');
    if (side(e.role) !== side(next.role)) return true;
    return !!(e.ts && next.ts && next.ts - e.ts > 300);
  }

  function renderConvo(id, opts) {
    if (state.openSheetId !== id) return;
    const convo = $('#convo');
    if (!convo) return;
    const conv = ensureConvo(id);

    convo.innerHTML = '';

    // Scroll-back affordance at the very top: a "Loading…" spinner while a page
    // is in flight, hidden entirely once there's no older history.
    const hs = state.history.get(id);
    if (hs && (hs.hasMore || hs.loading)) {
      const more = el('div', 'history-more' + (hs.loading ? ' loading' : ''));
      more.textContent = hs.loading ? 'Loading older…' : 'Scroll up for older messages';
      convo.appendChild(more);
    }

    conv.forEach((e, i) => {
      if (e.role === 'me') {
        // Full-width user row: dim '›' marker, plain text body.
        const row = el('div', 'msg me' + (e.queued ? ' queued' : ''));
        row.appendChild(el('span', 'gutter', '›'));
        const body = el('div', 'body');
        body.textContent = e.content;
        row.appendChild(body);
        convo.appendChild(row);
        if (e.queued || closesRun(e, conv[i + 1])) convo.appendChild(el('div', 'ts', fmtTime(e.ts) + (e.queued ? ' · sent' : '')));
      } else if (e.role === 'reply') {
        // Merge consecutive 'reply' entries (one Claude turn can yield several
        // assistant text entries) into a single row. Codex commentary remains
        // discrete so intermediary progress is durably readable message by
        // message instead of being folded into the eventual final answer.
        if (!e.kind && i > 0 && conv[i - 1].role === 'reply' && !conv[i - 1].kind) return;
        let content = e.content;
        let last = e;
        let lastIdx = i;
        for (let j = i + 1; !e.kind && j < conv.length && conv[j].role === 'reply' && !conv[j].kind; j++) {
          content += '\n\n' + conv[j].content;
          last = conv[j];
          lastIdx = j;
        }
        // Full-width assistant row: accent left border + '‹' marker.
        const row = el('div', 'msg reply');
        row.appendChild(el('span', 'gutter', '‹'));
        const body = el('div', 'body');
        body.innerHTML = renderMarkdown(content);
        row.appendChild(body);
        convo.appendChild(row);
        if (closesRun(last, conv[lastIdx + 1])) convo.appendChild(el('div', 'ts', fmtTime(last.ts)));
      } else if (e.role === 'error') {
        const row = el('div', 'msg err');
        row.appendChild(el('span', 'gutter', '⛔'));
        const body = el('div', 'body');
        body.textContent = e.content;
        row.appendChild(body);
        convo.appendChild(row);
        convo.appendChild(el('div', 'ts', fmtTime(e.ts)));
      } else if (e.role === 'approved') {
        const b = el('div', 'approved-strip' + (e.bad ? ' bad' : ''));
        b.innerHTML = e.html;
        convo.appendChild(b);
      } else if (e.role === 'note') {
        const b = el('div', 'pending');
        b.innerHTML = '<span>' + esc(e.content) + '</span>';
        convo.appendChild(b);
      } else if (e.role === 'slash-out') {
        // Collapsible captured output for a printing slash command. Open by
        // default so the freshly-requested output is visible without a tap.
        const d = el('details', 'slash-out');
        d.open = true;
        const sum = el('summary', null, '✓ ' + (e.command || 'command') + ' output');
        d.appendChild(sum);
        const pre = el('pre', 'slash-out-text');
        pre.textContent = e.content;
        d.appendChild(pre);
        convo.appendChild(d);
        convo.appendChild(el('div', 'ts', fmtTime(e.ts)));
      }
    });

    const pend = state.pending.get(id);
    if (!conv.length && !pend && hs && !hs.loading) {
      convo.appendChild(el('div', 'thread-empty', 'No messages yet. Send one below.'));
    }

    // Active pending row.
    if (pend) {
      const p = el('div', 'pending');
      const session = state.sessions.get(id);
      const agent = session && session.tool === 'codex' ? 'Codex' : 'Claude';
      p.innerHTML = '<span class="tdots"><i></i><i></i><i></i></span>' +
        '<span>' + agent + ' is working… <span class="ctx">' + esc(pend.ctx || 'thinking') + '</span></span>';
      convo.appendChild(p);
    }

    // Older-paging re-renders restore scroll position themselves (the caller
    // anchors on inserted height); everything else sticks to the bottom.
    if (!(opts && opts.keepScroll)) { convo.scrollTop = convo.scrollHeight; hideNewPill(); }
  }

  // Escape-first markdown (web/markdown.js).
  function renderMarkdown(src) { return DeckMarkdown.render(src); }

  // Code-block toolbar (Wrap / Copy / Show all), delegated from the thread.
  function onCodeAction(e) {
    const btn = e.target.closest && e.target.closest('[data-act]');
    const block = btn && btn.closest('.code');
    if (!block) return;
    const act = btn.dataset.act;
    if (act === 'wrap') {
      // The label says what a tap does: wrapped blocks offer Unwrap.
      block.classList.toggle('wrap');
      btn.textContent = block.classList.contains('wrap') ? 'Unwrap' : 'Wrap';
    } else if (act === 'more') {
      block.classList.remove('collapsed');
      btn.remove();
    } else if (act === 'copy') {
      const code = $('code', block);
      const done = (ok) => { btn.textContent = ok ? 'Copied' : 'Copy failed'; setTimeout(() => { btn.textContent = 'Copy'; }, 1500); };
      if (navigator.clipboard && code) navigator.clipboard.writeText(code.textContent).then(() => done(true), () => done(false));
      else done(false);
    }
  }

  // ----- ask / slash -----
  async function sendAsk(id, text, delivery) { await doSend(id, text, false, delivery); }
  async function sendSlash(id, text, delivery) { await doSend(id, text, true, delivery); }

  async function doSend(id, text, slash, delivery) {
    const conv = ensureConvo(id);
    conv.push({ role: 'me', content: text, ts: Date.now() / 1000, queued: true });
    // Non-blocking pending state shown immediately.
    state.pending.set(id, { ctx: slash ? text : 'thinking' });
    renderConvo(id);
    renderDeck();

    try {
      const path = slash ? '/api/rc/slash' : '/api/rc/ask';
      const r = await api('POST', path, { sessionId: id, text, profile: CURRENT_PROFILE, delivery: delivery || 'auto' });
      if (r && r.requestId) {
        // tag the pending row so the matching reply event resolves it
        const pend = state.pending.get(id) || {};
        pend.requestId = r.requestId;
        state.pending.set(id, pend);
      }
    } catch (e) {
      state.pending.delete(id);
      conv.push({ role: 'error', content: 'Send failed: ' + e.message, ts: Date.now() / 1000 });
      renderConvo(id);
      renderDeck();
    }
  }

  // ----- attention: guarded approvals + request_user_input forms -----
  async function checkAttention(s, userInitiated) {
    const mount = $('#approveMount');
    if (!mount || state.openSheetId !== s.id) return;

    const btn = $('#sheetCheckPerm');
    if (btn) btn.classList.add('busy');
    try {
      const r = await api('GET', '/api/rc/attention?id=' + encodeURIComponent(s.id));
      if (state.openSheetId !== s.id) return;
      if (r && r.pending) {
        s.needsAttention = true;
        if (r.kind === 'question') renderQuestionPanel(s, r);
        else renderApprovePanel(s, r);
      } else if (r && r.unavailable) {
        mount.innerHTML = '';
        if (userInitiated) mount.appendChild(approveNote('Couldn\'t verify this session\'s live attention state.'));
      } else {
        s.needsAttention = false;
        mount.innerHTML = '';
        if (userInitiated) mount.appendChild(approveNote('No pending approval or question.'));
      }
      renderDeck();
    } catch (e) {
      if (state.openSheetId !== s.id) return;
      mount.innerHTML = '';
      if (userInitiated) mount.appendChild(approveNote('Couldn\'t check attention: ' + e.message));
    } finally {
      if (btn) btn.classList.remove('busy');
      syncComposerLock();
    }
  }

  function approveNote(text) {
    const n = el('div', 'approve-note');
    n.textContent = text;
    return n;
  }

  // While a request is docked the composer is locked: typed text would land in
  // the live dialog, not the composer.
  function syncComposerLock() {
    const ta = $('#composerInput');
    const mount = $('#approveMount');
    if (!ta) return;
    const locked = !!(mount && mount.querySelector('.approve-card'));
    const send = ta.parentElement && ta.parentElement.querySelector('.send-btn');
    if (send) send.disabled = locked || !ta.value.trim();
    const slash = ta.parentElement && ta.parentElement.querySelector('.slash-btn');
    if (locked && !ta.dataset.lockPlaceholder) {
      ta.dataset.lockPlaceholder = ta.placeholder;
      ta.dataset.lockDisabled = ta.disabled ? '1' : '';
      ta.placeholder = 'Answer the request above first';
      ta.disabled = true;
    } else if (!locked && ta.dataset.lockPlaceholder) {
      ta.placeholder = ta.dataset.lockPlaceholder;
      ta.disabled = ta.dataset.lockDisabled === '1';
      delete ta.dataset.lockPlaceholder;
      delete ta.dataset.lockDisabled;
    }
    if (slash) slash.disabled = ta.disabled;
  }

  // "Bash command" / "git push origin main": the dialog's first two lines, as
  // a one-glance summary above the verbatim text.
  function permissionSummary(text) {
    const lines = String(text || '').split('\n')
      .map((l) => l.replace(/^[\s│╭╮╰╯─]+|[\s│╭╮╰╯─]+$/g, ''))
      .filter(Boolean);
    return { kind: lines[0] || 'Permission request', what: lines[1] || '' };
  }

  // Render the Approve dock with the ACTUAL permission text the server read.
  function renderApprovePanel(s, info) {
    const mount = $('#approveMount');
    if (!mount) return;
    const sum = permissionSummary(info.text);
    const wrap = el('div', 'approve-card');
    wrap.innerHTML =
      '<div class="approve-head"><span class="dot"></span><div class="approve-sum">' +
        '<div class="approve-kind">' + esc(sum.kind) + '</div>' +
        (sum.what ? '<div class="approve-cmd">' + esc(sum.what) + '</div>' : '') +
      '</div></div>' +
      '<div class="approve-body"><pre class="perm-text" id="permText"></pre></div>' +
      '<div class="approval-actions" id="approvalActions"></div>' +
      '<div class="approve-foot" id="approveFoot">The live dialog is re-checked before any key is sent.</div>';
    mount.innerHTML = '';
    mount.appendChild(wrap);
    // Set the real text as textContent so line breaks/monospace are preserved
    // verbatim and nothing is interpreted as HTML.
    $('#permText', wrap).textContent = info.text || '(no request text returned)';
    const actions = $('#approvalActions', wrap);
    const labels = { 'approve-once': 'Hold to approve', 'approve-prefix': 'Hold: approve and don\'t ask again', decline: 'Decline' };
    const button = (action) => {
      const b = el('button', 'attention-action ' + (action === 'decline' ? 'decline' : 'approve'));
      b.textContent = labels[action] || action;
      if (action === 'decline') b.addEventListener('click', () => respondAttention(s, info, action, []));
      else wireHoldAction(b, () => respondAttention(s, info, action, []));
      return b;
    };
    // Main row: Decline left, Hold-to-approve right. Anything else (approve
    // prefix) sits behind "More options" so it can't be hit by accident.
    const acts = info.actions || [];
    const main = el('div', 'approval-main');
    ['decline', 'approve-once'].forEach((a) => { if (acts.includes(a)) main.appendChild(button(a)); });
    if (main.children.length) actions.appendChild(main);
    const rest = acts.filter((a) => a !== 'decline' && a !== 'approve-once');
    if (rest.length) {
      const more = el('details', 'approval-more');
      more.appendChild(el('summary', null, 'More options'));
      rest.forEach((a) => more.appendChild(button(a)));
      actions.appendChild(more);
    }
    if (!acts.length) actions.appendChild(approveNote('This live dialog is not unambiguous enough for remote approval. Use the terminal.'));
    syncComposerLock();
  }

  function renderQuestionPanel(s, info) {
    const mount = $('#approveMount');
    if (!mount) return;
    const wrap = el('div', 'approve-card question-card');
    wrap.innerHTML = '<div class="approve-head"><span class="dot"></span> Codex needs input</div><div class="approve-body"><form id="questionForm"></form><div class="approve-foot" id="approveFoot">Answers are sent only while this exact request is still live.</div></div>';
    mount.innerHTML = '';
    mount.appendChild(wrap);
    syncComposerLock();
    const form = $('#questionForm', wrap);
    (info.questions || []).forEach((q) => {
      const field = el('fieldset', 'question-field');
      const legend = el('legend', null, q.header || 'Question');
      field.appendChild(legend);
      field.appendChild(el('div', 'question-text', q.question || ''));
      (q.options || []).forEach((opt, i) => {
        const label = el('label', 'question-option');
        const input = document.createElement('input');
        input.type = 'radio'; input.name = q.id; input.value = opt.label; input.required = true;
        label.appendChild(input);
        const copy = el('span');
        copy.innerHTML = '<b>' + esc(opt.label) + '</b>' + (opt.description ? '<small>' + esc(opt.description) + '</small>' : '');
        label.appendChild(copy); field.appendChild(label);
      });
      const custom = el('label', 'question-option');
      const radio = document.createElement('input');
      radio.type = 'radio'; radio.name = q.id; radio.value = '__other__'; radio.required = true;
      const text = document.createElement('input');
      text.type = 'text'; text.className = 'question-freeform'; text.placeholder = 'Other answer';
      text.addEventListener('focus', () => { radio.checked = true; });
      custom.appendChild(radio); custom.appendChild(text); field.appendChild(custom);
      form.appendChild(field);
    });
    const submit = el('button', 'question-submit', 'Submit answers');
    submit.type = 'submit'; form.appendChild(submit);
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      const answers = [];
      for (const q of (info.questions || [])) {
        const checked = form.querySelector('input[type=radio][name="' + CSS.escape(q.id) + '"]:checked');
        if (!checked) return;
        let answer = checked.value;
        if (answer === '__other__') {
          answer = checked.parentElement.querySelector('.question-freeform').value.trim();
          if (!answer) return;
        }
        answers.push({ id: q.id, answer });
      }
      submit.disabled = true;
      respondAttention(s, info, 'answer', answers);
    });
  }

  function wireHoldAction(button, action) {
    const HOLD_MS = 800;
    let timer = null, fired = false;
    const begin = (e) => {
      e.preventDefault(); fired = false; button.classList.add('armed');
      timer = setTimeout(() => { fired = true; button.disabled = true; action(); }, HOLD_MS);
    };
    const cancel = () => { if (!fired) { clearTimeout(timer); button.classList.remove('armed'); } };
    button.addEventListener('mousedown', begin);
    button.addEventListener('touchstart', begin, { passive: false });
    ['mouseup', 'mouseleave', 'touchend', 'touchcancel'].forEach((ev) => button.addEventListener(ev, cancel));
  }

  async function respondAttention(s, info, action, answers) {
    try {
      const r = await api('POST', '/api/rc/attention/respond', {
        sessionId: s.id, profile: CURRENT_PROFILE, attentionId: info.id, action, answers: answers || [],
      });
      if (r && r.responded) {
        s.needsAttention = false;
        const mount = $('#approveMount'); if (mount) mount.innerHTML = '';
        syncComposerLock();
        ensureConvo(s.id).push({ role: 'note', content: '✓ response sent' + (r.cleared ? '' : ' · confirming…'), ts: Date.now() / 1000 });
        renderConvo(s.id); renderDeck();
      }
    } catch (e) {
      const foot = $('#approveFoot');
      if (foot) { foot.textContent = 'Response failed: ' + e.message; foot.style.color = 'var(--red)'; }
      checkAttention(s, false);
    }
  }

  // ---------------------------------------------------------------------------
  // SSE streams
  // ---------------------------------------------------------------------------
  let evRC = null;

  function teardownStreams() {
    if (evRC) { evRC.close(); evRC = null; }
  }

  // Live: a green dot by the title. Anything else also shows the detail strip.
  function setConn(text, cls) {
    const led = $('#titleLed');
    if (led) { led.className = 'title-led' + (cls ? ' ' + cls : ''); led.title = text; }
    const strip = $('#connStrip');
    if (!strip) return;
    strip.className = 'conn' + (cls ? ' ' + cls : '');
    strip.hidden = cls === 'live';
    $('#connText').textContent = text;
  }


  function startStreams() {
    teardownStreams();

    // deck-remote's own events: ask-state / reply / approve-result.
    evRC = new EventSource(tokenURL('/api/rc/events'));
    evRC.onopen = () => setConn('Live', 'live');
    evRC.onerror = () => setConn('Reconnecting…', 'bad'); // EventSource auto-reconnects
    evRC.onmessage = (m) => {
      let ev; try { ev = JSON.parse(m.data); } catch (_) { return; }
      handleRCEvent(ev);
    };
  }

  function handleRCEvent(ev) {
    if (!ev || !ev.type) return;
    if (ev.type === 'notification') { addInboxItem(ev.notification); return; }
    const id = ev.sessionId;
    if (ev.type === 'ask-state' && (ev.state === 'sent' || ev.state === 'steered')) {
      // already reflected locally on send; ensure pending exists
      if (id && !state.pending.has(id)) state.pending.set(id, { ctx: 'thinking', requestId: ev.requestId });
      renderConvo(id);
      if (ev.state === 'steered' && id && state.openSheetId === id) toast('Steered', 'Injected into the current Codex turn.');
    } else if (ev.type === 'ask-state' && ev.state === 'queued') {
      // Busy-session delivery: the message was injected into Claude's live composer
      // and queued natively (runs after the current turn). No "reply" event follows
      // this requestId — the reply arrives via the transcript reconcile — so clear
      // the per-message pending to avoid a perpetual "thinking" spinner.
      state.pending.delete(id);
      renderConvo(id);
      renderDeck();
      if (state.openSheetId === id) toast('Queued', 'The agent will run it after the current turn.');
    } else if (ev.type === 'ask-state' && ev.state === 'stopping') {
      if (id && state.openSheetId === id) toast('Stopping', 'Interrupt sent; waiting for rollout confirmation.');
    } else if (ev.type === 'reply') {
      const conv = ensureConvo(id);
      const pend = state.pending.get(id);
      state.pending.delete(id);
      // mark prior queued user bubble as no longer queued
      conv.forEach((e) => { if (e.role === 'me') e.queued = false; });
      if (ev.error) {
        conv.push({ role: 'error', content: ev.error, ts: ev.ts });
        notifyForeground(id, 'error', 'Turn failed', ev.error);
      } else {
        const content = cleanReplyContent(ev.content);
        conv.push({ role: 'reply', content, ts: ev.ts });
        notifyForeground(id, 'finished', titleFor(id) + ' replied', oneLine(content, 90));
      }
      renderConvo(id);
      renderDeck();
    } else if (ev.type === 'slash-result') {
      // Slash commands produce no reply — show a subtle confirmation, not a bubble.
      const conv = ensureConvo(id);
      state.pending.delete(id);
      conv.forEach((e) => { if (e.role === 'me') e.queued = false; });
      if (ev.error) {
        conv.push({ role: 'error', content: (ev.command || 'command') + ' failed: ' + ev.error, ts: ev.ts });
      } else if (ev.output) {
        // Printing slash (/context, /cost, …): show the captured pane output in
        // a collapsible block alongside the sent confirmation.
        conv.push({ role: 'slash-out', command: ev.command || 'command', content: ev.output, ts: ev.ts });
      } else {
        conv.push({ role: 'note', content: '✓ ' + (ev.command || 'command') + ' sent', ts: ev.ts });
      }
      renderConvo(id);
      renderDeck();
    } else if (ev.type === 'approve-result') {
      if (id && state.openSheetId === id) {
        const s = state.sessions.get(id); if (s) checkAttention(s, false);
      }
    } else if (ev.type === 'attention-result') {
      if (id && state.openSheetId === id && ev.responded) {
        const s = state.sessions.get(id);
        if (s) { s.needsAttention = false; checkAttention(s, false); }
      }
    }
  }

  function titleFor(id) {
    const s = state.sessions.get(id);
    return (s && s.title) || 'Session';
  }

  // Foreground notify: when the app is visible we show an in-app toast instead of
  // a (suppressed) system push. Honors the per-type prefs.
  function notifyForeground(id, kind, title, body) {
    if (!prefs[kind]) return;
    if (document.visibilityState !== 'visible') return; // system push handles backgrounded
    toast(title, body, () => { showTab('deck'); openSheet(id); });
  }

  // ---------------------------------------------------------------------------
  // Presence (CRITICAL: server suppresses push when focus is unknown)
  // ---------------------------------------------------------------------------
  async function postPresence(focused) {
    try {
      await fetch('/api/rc/push/presence', {
        method: 'POST',
        headers: authHeaders({ 'Content-Type': 'application/json' }),
        body: JSON.stringify({ focused }),
        keepalive: true, // allow delivery during page-hide
      });
    } catch (_) { /* best-effort */ }
  }

  function wirePresence() {
    document.addEventListener('visibilitychange', () => {
      postPresence(document.visibilityState === 'visible');
    });
    window.addEventListener('blur', () => postPresence(false));
    window.addEventListener('focus', () => postPresence(true));
    // Post focused:false right before backgrounding so push works.
    window.addEventListener('pagehide', () => postPresence(false));
    // Initial state.
    postPresence(document.visibilityState === 'visible');
  }

  // ---------------------------------------------------------------------------
  // Push opt-in / settings
  // ---------------------------------------------------------------------------
  function urlBase64ToUint8Array(base64String) {
    const padding = '='.repeat((4 - (base64String.length % 4)) % 4);
    const base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
    const raw = atob(base64);
    const arr = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) arr[i] = raw.charCodeAt(i);
    return arr;
  }

  function isStandalone() {
    return window.matchMedia('(display-mode: standalone)').matches || window.navigator.standalone === true;
  }
  function pushSupported() {
    return 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
  }

  async function registerSW() {
    if (!('serviceWorker' in navigator)) return null;
    try {
      return await navigator.serviceWorker.register('/sw.js', { scope: '/' });
    } catch (e) {
      return null;
    }
  }

  async function refreshPushUI() {
    // setup checklist
    mark($('#reqInstall'), isStandalone());
    mark($('#reqSupport'), pushSupported());
    mark($('#reqPerm'), 'Notification' in window && Notification.permission === 'granted');

    // Pre-opt-in warning: the two preconditions that silently break push.
    const warn = $('#pushWarn');
    if (warn) {
      const isIOS = /iphone|ipad|ipod/i.test(navigator.userAgent);
      if (isIOS && !isStandalone()) {
        warn.hidden = false;
        warn.textContent = "Open deck-remote from its Home-Screen icon — Safari/Brave tabs can't receive notifications.";
      } else if ('Notification' in window && Notification.permission === 'denied') {
        warn.hidden = false;
        warn.textContent = 'Notifications are blocked for this app. Enable them in iOS Settings ▸ deck-remote ▸ Notifications.';
      } else {
        warn.hidden = true;
      }
    }

    // Quiet-hours row visibility + values follow the prefs.
    const qr = $('#quietRow');
    if (qr) qr.hidden = !prefs.quietOn;
    const qs = $('#quietStart'), qe = $('#quietEnd');
    if (qs && prefs.quietStart) qs.value = prefs.quietStart;
    if (qe && prefs.quietEnd) qe.value = prefs.quietEnd;

    // fetch config (graceful enabled:false)
    if (!state.pushConfig) {
      try { state.pushConfig = await api('GET', '/api/rc/push/config'); }
      catch (e) { state.pushConfig = { enabled: false, _err: e.message }; }
    }
    const cfg = state.pushConfig;
    const btn = $('#btnEnablePush');
    const sub = $('#subState');
    const subText = $('#subStateText');

    let existing = null;
    if (pushSupported()) {
      try {
        const reg = await navigator.serviceWorker.getRegistration();
        if (reg) existing = await reg.pushManager.getSubscription();
      } catch (_) {}
    }

    // Re-sync client prefs to the server whenever we're already subscribed. The
    // persisted server prefs file predates any newly-added pref (e.g. `stall`),
    // which JSON-decodes to false; pushing the current client prefs on load makes
    // new defaults take effect without requiring a manual toggle.
    if (existing) pushPrefsToServer();

    // iOS exposes PushManager ONLY in an installed (home-screen) PWA, so on iOS
    // a Safari/Brave TAB always fails pushSupported(). Check install state FIRST
    // and guide to Add-to-Home-Screen rather than a misleading "not supported".
    if (/iphone|ipad|ipod/i.test(navigator.userAgent) && !isStandalone()) {
      btn.disabled = true; btn.textContent = 'Add to Home Screen first';
      setSub(sub, subText, '', 'iOS only allows notifications from the installed app: tap <b>Share → Add to Home Screen</b>, open deck-remote from the new icon, then return here.');
    } else if (!pushSupported()) {
      btn.disabled = true; btn.textContent = 'Push not supported here';
      setSub(sub, subText, 'off', 'Push <b>unavailable</b> on this browser. Use a recent iOS/Android browser, installed to the Home Screen.');
    } else if (!cfg || !cfg.enabled) {
      btn.disabled = true; btn.textContent = 'Push disabled on server';
      setSub(sub, subText, 'off', 'The server is running <b>without --push</b>. Restart deck-remote with push to enable.');
    } else if (existing) {
      btn.disabled = false; btn.textContent = 'Notifications enabled ✓';
      setSub(sub, subText, 'ok', 'Push <b>enabled</b> on this device.');
    } else {
      btn.disabled = false; btn.textContent = 'Enable notifications';
      setSub(sub, subText, '', 'Push <b>not yet enabled</b> on this device.');
    }
  }

  function mark(reqEl, done) {
    if (!reqEl) return;
    reqEl.className = 'req ' + (done ? 'done' : 'todo');
    const rm = $('.rmark', reqEl);
    if (rm) rm.textContent = done ? '✓' : '';
  }
  function setSub(sub, subText, cls, html) {
    sub.className = 'sub-state' + (cls ? ' ' + cls : '');
    subText.innerHTML = html;
  }

  // Push the server-honored prefs (per-event approve/finished + quiet-hours) to
  // the daemon. Times are sent as minutes-from-midnight (local). Best-effort.
  async function pushPrefsToServer() {
    const [sh, sm] = (prefs.quietStart || '22:00').split(':').map(Number);
    const [eh, em] = (prefs.quietEnd || '07:00').split(':').map(Number);
    try {
      await api('POST', '/api/rc/push/prefs', {
        approve: !!prefs.approve,
        finished: !!prefs.finished,
        stall: !!prefs.stall,
        quietOn: !!prefs.quietOn,
        quietStart: sh * 60 + sm,
        quietEnd: eh * 60 + em,
      });
    } catch (_) {}
  }

  async function enablePush() {
    if (!pushSupported()) return;
    const cfg = state.pushConfig;
    if (!cfg || !cfg.enabled || !(cfg.vapidPublicKey || cfg.publicKey)) {
      toast('Push unavailable', 'The server is not running with push enabled.');
      return;
    }
    const btn = $('#btnEnablePush');
    btn.disabled = true; btn.textContent = 'Requesting…';
    try {
      const perm = await Notification.requestPermission();
      if (perm !== 'granted') {
        toast('Permission denied', 'Enable notifications for this app in OS settings.');
        await refreshPushUI();
        return;
      }
      const reg = (await navigator.serviceWorker.getRegistration()) || (await registerSW());
      const sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array((cfg.vapidPublicKey || cfg.publicKey)),
      });
      await api('POST', '/api/rc/push/subscribe', sub.toJSON());
      await pushPrefsToServer();
      toast('Notifications on', 'You\'ll get pushed when a session needs you.');
    } catch (e) {
      toast('Could not enable push', e.message);
    } finally {
      await refreshPushUI();
    }
  }

  // settings toggles
  $$('.toggle[data-pref]').forEach((tg) => {
    tg.classList.toggle('on', !!prefs[tg.dataset.pref]);
    // The whole row is the tap target, not just the 46px knob.
    const row = tg.closest('.set-row');
    if (row) {
      row.classList.add('tappable');
      row.addEventListener('click', (e) => { if (!tg.contains(e.target)) tg.click(); });
    }
    tg.addEventListener('click', () => {
      const key = tg.dataset.pref;
      prefs[key] = !prefs[key];
      tg.classList.toggle('on', prefs[key]);
      savePrefs(prefs);
      if (key === 'quietOn') { const qr = $('#quietRow'); if (qr) qr.hidden = !prefs.quietOn; }
      // Sync the server-honored prefs (cheap; harmless before any subscription).
      if (key === 'approve' || key === 'finished' || key === 'stall' || key === 'quietOn') pushPrefsToServer();
    });
  });
  // Quiet-hours time inputs.
  ['#quietStart', '#quietEnd'].forEach((sel) => {
    const el = $(sel);
    if (!el) return;
    el.addEventListener('change', () => {
      if (sel === '#quietStart') prefs.quietStart = el.value;
      else prefs.quietEnd = el.value;
      savePrefs(prefs);
      pushPrefsToServer();
    });
  });
  $('#btnEnablePush').addEventListener('click', enablePush);
  $('#btnTestPush').addEventListener('click', async () => {
    const b = $('#btnTestPush');
    b.disabled = true;
    const old = b.textContent;
    b.textContent = 'Sending…';
    try {
      const r = await api('POST', '/api/rc/push/test');
      const n = (r && r.sent) || 0;
      const results = (r && r.results) || [];
      const oks = results.filter((x) => x.status >= 200 && x.status < 300).length;
      const fails = results.filter((x) => !(x.status >= 200 && x.status < 300));
      if (n === 0) toast('No subscriptions', 'Enable notifications on this device first.');
      else if (fails.length === 0) toast('Test push sent', oks + '/' + n + ' delivered. Check your lock screen.');
      else toast('Push had errors', fails.map((f) => (f.status || 'ERR') + (f.error ? (' ' + f.error) : '')).join(' · '));
    } catch (e) {
      toast('Test push failed', e.message);
    } finally {
      b.disabled = false;
      b.textContent = old;
    }
  });
  $('#forgetToken').addEventListener('click', () => {
    if (confirm('Forget the token on this device?')) forgetToken('Token cleared.');
  });

  // ---------------------------------------------------------------------------
  // Per-device tokens (additive). whoami identifies the active token; the admin
  // (shared-token) UI mints/lists/revokes device tokens. Device tokens never see
  // the admin section (the endpoints 403 anyway — UI just hides it).
  // ---------------------------------------------------------------------------
  let DEVICE_ADMIN = false;
  async function refreshDevicesUI() {
    const hint = $('#whoamiHint');
    try {
      const who = await api('GET', '/api/rc/devices/whoami');
      DEVICE_ADMIN = !!(who && who.admin);
      if (hint) {
        hint.textContent = who && who.kind === 'device'
          ? ('Device token: ' + (who.name || who.id))
          : 'Shared (admin) token';
      }
    } catch (_) {
      DEVICE_ADMIN = false;
      if (hint) hint.textContent = '';
    }
    const admin = $('#devicesAdmin');
    if (admin) admin.hidden = !DEVICE_ADMIN;
    if (DEVICE_ADMIN) loadDeviceList();
  }

  async function loadDeviceList() {
    const list = $('#deviceList');
    if (!list) return;
    list.innerHTML = '';
    let devices = [];
    try { const r = await api('GET', '/api/rc/devices'); devices = (r && r.devices) || []; }
    catch (_) { return; }
    if (!devices.length) {
      const row = el('div', 'set-row');
      row.innerHTML = '<span class="gi">—</span><span class="lbl" style="color:var(--muted)">No device tokens yet</span>';
      list.appendChild(row);
      return;
    }
    devices.forEach((d) => {
      const row = el('div', 'set-row');
      const when = d.created ? new Date(d.created * 1000).toLocaleDateString() : '';
      row.innerHTML = '<span class="gi">📲</span><span class="lbl">' + esc(d.name || d.id) +
        '<div class="rhint" style="font-size:12px;color:var(--muted);margin-top:2px">added ' + esc(when) + '</div></span>';
      const revoke = el('span', 'lbl', 'Revoke');
      revoke.style.color = 'var(--red)';
      revoke.style.flex = '0 0 auto';
      revoke.style.cursor = 'pointer';
      revoke.addEventListener('click', async () => {
        if (!confirm('Revoke "' + (d.name || d.id) + '"? That device must re-enroll.')) return;
        try { await api('POST', '/api/rc/devices/revoke', { id: d.id }); toast('Revoked', d.name || d.id); loadDeviceList(); }
        catch (e) { toast('Revoke failed', e.message); }
      });
      row.appendChild(revoke);
      list.appendChild(row);
    });
  }

  const mintBtn = $('#mintDevice');
  if (mintBtn) mintBtn.addEventListener('click', async () => {
    const name = (prompt('Name for the new device (e.g. ruben-iphone):') || '').trim();
    if (!name) return;
    try {
      const r = await api('POST', '/api/rc/devices/mint', { name });
      const out = $('#mintResult');
      if (out) {
        out.hidden = false;
        out.innerHTML = 'Token for <b>' + esc(r.name) + '</b> (shown once):<br>' +
          '<code>' + esc(r.token) + '</code><br>' +
          'Open this on the device:<br><code>' + esc(r.phoneUrl) + '</code>';
      }
      loadDeviceList();
    } catch (e) { toast('Mint failed', e.message); }
  });

  // Escape hatch → embedded xterm.js terminal page. We deep-link to the open
  // session if any, else the most recent session; with no sessions at all we
  // fall back to the app root. terminal.html reads the token from localStorage
  // (same origin), so it stays out of the URL.
  $('#escapeTerm').addEventListener('click', (e) => {
    e.preventDefault();
    // The terminal is bound to PROXY_PROFILE (single proxied agent-deck web), so
    // it can't open a session that lives in a different selected profile.
    if (CURRENT_PROFILE && PROXY_PROFILE && CURRENT_PROFILE !== PROXY_PROFILE) {
      toast('Terminal', 'Only available for the \'' + PROXY_PROFILE + '\' profile.');
      return;
    }
    const id = state.openSheetId || (Array.from(state.sessions.keys())[0]);
    const url = id ? '/terminal.html?id=' + encodeURIComponent(id) : tokenURL('/');
    location.href = url; // same window: see sheetHeadHTML
  });

  // ---------------------------------------------------------------------------
  // SW message: deep-link from a tapped notification
  // ---------------------------------------------------------------------------
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.addEventListener('message', (e) => {
      if (e.data && e.data.type === 'open-notification' && e.data.id) {
        openNotification(e.data.id);
      } else if (e.data && e.data.type === 'open-session' && e.data.sessionId) {
        openSession(e.data.sessionId);
      }
    });
  }

  // ---------------------------------------------------------------------------
  // Boot
  // ---------------------------------------------------------------------------
  async function loadSessions(initial) {
    try {
      const r = await api('GET', '/api/rc/sessions');
      const list = (r && r.sessions) || [];
      // Merge: keep ids stable so an open sheet isn't disrupted; replace fields
      // (incl. refreshed lastReply) and drop sessions no longer present.
      const seen = new Set();
      list.forEach((s) => {
        seen.add(s.id);
        const prev = state.sessions.get(s.id);
        state.sessions.set(s.id, Object.assign({}, prev || {}, s));
      });
      Array.from(state.sessions.keys()).forEach((id) => { if (!seen.has(id)) state.sessions.delete(id); });
      setConn('Live', 'live');
    } catch (e) {
      setConn('Can\'t reach server: ' + e.message, 'bad');
    }
    renderDeck();
    if (initial) deepLinkFromURL();
  }

  function deepLinkFromURL() {
    const id = new URL(location.href).searchParams.get('session');
    if (id && state.sessions.has(id) && state.openSheetId !== id) openSheet(id);
  }

  // Periodically refresh the list so last-reply previews stay current, and also
  // refresh when the tab regains focus. (No status — "live" = fresh previews.)
  const REFRESH_MS = 20000;
  let refreshTimer = null;
  function startRefresh() {
    if (refreshTimer) clearInterval(refreshTimer);
    refreshTimer = setInterval(() => {
      if (document.visibilityState === 'visible') loadSessions(false);
    }, REFRESH_MS);
  }

  // ---------------------------------------------------------------------------
  // Multi-profile selector
  // ---------------------------------------------------------------------------
  // loadProfiles fetches the available profiles, the daemon default, and the
  // proxy-bound profile, then renders the header selector (hidden when <=1).
  async function loadProfiles() {
    try {
      const r = await api('GET', '/api/rc/profiles');
      PROFILES = (r && r.profiles) || [];
      PROXY_PROFILE = (r && r.proxyProfile) || '';
      // Default the selection to the daemon's current profile on first run, and
      // reset if a stored profile no longer exists.
      const names = PROFILES.map((p) => p.name);
      if (!CURRENT_PROFILE || !names.includes(CURRENT_PROFILE)) {
        CURRENT_PROFILE = (r && r.current) || '';
        localStorage.setItem(PROFILE_KEY, CURRENT_PROFILE);
      }
    } catch (_) {
      PROFILES = []; // endpoint unavailable: behave as single-profile
    }
    renderProfileSelector();
  }

  function renderProfileSelector() {
    const sel = $('#profileSel');
    if (!sel) return;
    if (PROFILES.length <= 1) { sel.hidden = true; sel.innerHTML = ''; return; }
    sel.hidden = false;
    sel.innerHTML = PROFILES.map((p) =>
      '<option value="' + esc(p.name) + '"' + (p.name === CURRENT_PROFILE ? ' selected' : '') + '>' +
      esc(p.name) + (p.isDefault ? ' (default)' : '') + '</option>'
    ).join('');
  }

  function onProfileChange(name) {
    if (name === CURRENT_PROFILE) return;
    CURRENT_PROFILE = name;
    localStorage.setItem(PROFILE_KEY, name);
    if ($('#sheet')) closeSheet(); // the open session belongs to the old profile
    state.sessions.clear();
    state.convos.clear();
    state.pending.clear();
    state.history.clear();
    renderDeck();
    loadSessions(false);
  }

  (function wireProfileSelector() {
    const sel = $('#profileSel');
    if (sel) sel.addEventListener('change', (e) => onProfileChange(e.target.value));
  })();

  async function boot() {
    showTab('deck');
    registerSW();
    wirePresence();
    startStreams();
    await loadProfiles();
    await loadSessions(true);
    startRefresh();
    loadInbox();
    // Deep links from a tapped notification on a cold start (the service worker
    // opens /?notif=<id>, or /?session=<id> for pushes from older servers).
    const params = new URL(location.href).searchParams;
    if (params.has('notif')) {
      const u = new URL(location.href);
      u.searchParams.delete('notif');
      history.replaceState(null, '', u.pathname + u.search + u.hash);
      openNotification(params.get('notif'));
    } else if (params.has('session')) {
      openSession(params.get('session'));
    }
    // Re-pull on resume. iOS standalone PWAs resume from bfcache without a reload
    // and don't reliably fire visibilitychange, so listen for pageshow too. On
    // resume, refresh the deck (fresh activity/status is scraped server-side) and,
    // if a sheet is open, immediately reconcile its transcript tail — otherwise the
    // view is stuck on whatever it had when the app was last foregrounded.
    function onResume() {
      if (document.visibilityState !== 'visible') return;
      loadSessions(false);
      loadInbox(); // pushes that arrived while backgrounded (no SSE then)
      if (state.openSheetId) reconcileTranscriptTail(state.openSheetId);
    }
    document.addEventListener('visibilitychange', onResume);
    window.addEventListener('pageshow', onResume);
    refreshPushUI();
    pushPrefsToServer(); // sync server-honored prefs once on launch
  }

  // Entry point.
  if (!TOKEN) {
    showTokenScreen();
  } else {
    appEl.hidden = false;
    boot();
  }
})();
