/* deck-remote — live PWA frontend
 *
 * Vanilla JS, no build step, no external deps. Drives the dark Tokyo Night UI
 * from the approved mockup against deck-remote's real APIs.
 *
 * Detail-first model: the deck leads with each session's real last reply.
 * agent-deck's `status` field is intentionally NOT rendered — it's unreliable
 * in this environment. The Approve panel is gated on a REAL detected dialog
 * (GET /api/rc/permission), never on status.
 *
 * Contract (same-origin, bearer-token auth):
 *   GET  /api/rc/sessions    -> {sessions:[{id,title,path,group,tool,lastReply,working,activity,currentTool,...}]}
 *   GET  /api/rc/activity?id -> {working:bool, activity?:string, currentTool?:string} (live probe)
 *   GET  /api/rc/reply?id    -> {claude_session_id,content,role,timestamp}
 *   GET  /api/rc/permission?id -> {pending:bool, text?:string, unavailable?:bool}
 *   POST /api/rc/ask         {sessionId,text} -> 202 {requestId,sessionId,status}
 *   POST /api/rc/slash       {sessionId,text} -> same
 *   POST /api/rc/approve     {sessionId} -> {sessionId,approved,cleared?,reason?}
 *   GET  /api/rc/events      (SSE) -> ask-state | reply | slash-result | approve-result
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

  async function api(method, path, body) {
    const opts = { method, headers: authHeaders() };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
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
    // decision #4: idle OFF by default; approve/finished/error ON.
    // sortGroupsByActivity ON by default (most-recent/working groups float up).
    return Object.assign({ approve: true, finished: true, error: true, idle: false, sortGroupsByActivity: true }, p);
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
  };

  // How often the open detail sheet probes /api/rc/activity for live state.
  const ACTIVITY_MS = 3000;
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
    $('#screenSettings').hidden = tab !== 'settings';
    $$('.tab').forEach((t) => t.classList.toggle('active', t.dataset.tab === tab));
    if (tab === 'settings') refreshPushUI();
  }
  $$('.tab').forEach((t) => t.addEventListener('click', () => showTab(t.dataset.tab)));
  $('#goSettings').addEventListener('click', () => showTab('settings'));
  $('#goRefresh').addEventListener('click', async () => {
    const b = $('#goRefresh');
    b.classList.add('busy');
    try { await loadSessions(false); } finally { b.classList.remove('busy'); }
  });

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
        if (conv[i].role === 'reply' && conv[i].content) return oneLine(conv[i].content);
      }
    }
    return oneLine(s.lastReply || '');
  }

  function oneLine(s, max) {
    const t = String(s || '').replace(/\s+/g, ' ').trim();
    const m = max || 64;
    return t.length > m ? t.slice(0, m - 1) + '…' : t;
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

  // Recency for a group's session list: working sessions rank first (Infinity);
  // otherwise the MAX lastActivity (unix seconds) across the list, 0 if none.
  function groupRecency(list) {
    let anyWorking = false;
    let max = 0;
    for (const s of list) {
      if (s.working) anyWorking = true;
      const t = Number(s.lastActivity) || 0;
      if (t > max) max = t;
    }
    return { working: anyWorking, recency: anyWorking ? Infinity : max, last: max };
  }

  // Effective collapsed state: a working group is never collapsed; a manual
  // override wins; otherwise default-collapse only when stale (>24h).
  function isGroupCollapsed(name, info) {
    if (info.working) return false;
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

    // Group by tree/group. Within-group session order stays stable-alpha.
    const groups = new Map();
    all.forEach((s) => {
      const k = groupKey(s);
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(s);
    });

    // Precompute per-group recency so we can sort and label headers.
    const meta = new Map(); // name -> {info, list}
    groups.forEach((list, k) => {
      const sorted = list.slice().sort((a, b) => (a.title || a.id).localeCompare(b.title || b.id));
      meta.set(k, { info: groupRecency(sorted), list: sorted });
    });

    // Ordering: by activity (working/most-recent first, "never" last) when the
    // pref is ON; else the legacy alphabetical order. Ties break alphabetically.
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
      const treeHTML = esc(k).replace(/ \/ /g, ' / ');
      const age = relAge(info.last, info.working);
      // Collapsed headers carry a summary (count + age); expanded show a dim age.
      const summary = collapsed
        ? '<span class="summary">' + list.length + ' · ' + esc(age) + '</span>'
        : '<span class="age">' + esc(age) + '</span><span class="count">' + list.length + '</span>';
      head.innerHTML =
        '<span class="chev">' + (collapsed ? '▸' : '▾') + '</span>' +
        '<span class="tree">' + treeHTML + '</span> ' + summary;
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
    if (!nowCollapsed && info.working) return; // never collapse a working group
    collapseOverrides[name] = !nowCollapsed;
    saveCollapse(collapseOverrides);
    renderDeck();
  }

  function cardEl(s) {
    const card = el('div', 'card');
    card.dataset.id = s.id;

    // Title row: title + tool chip.
    const top = el('div', 'card-top');
    top.appendChild(el('div', 'card-title', s.title || s.id));
    const harness = el('span', 'harness');
    harness.innerHTML = '<span class="gi">✦</span>' + esc(s.tool || 'agent');
    top.appendChild(harness);
    card.appendChild(top);

    // Group/tree crumb.
    const meta = el('div', 'card-meta');
    meta.appendChild(el('span', 'path', groupKey(s)));
    card.appendChild(meta);

    // Live activity takes over the hero detail while the agent is working;
    // otherwise we fall back to the dim one-line last-reply preview.
    if (s.working) {
      const act = el('div', 'card-activity');
      act.innerHTML = '<span class="spin"></span><span class="act-text">' +
        esc(s.activity || 'Working…') + '</span>';
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
    // Detach the convo scroll listener and clear any in-flight paging flag so a
    // fetch settling after close doesn't touch a stale DOM.
    const convoEl = $('#convo');
    if (convoEl && state.convoScrollHandler) convoEl.removeEventListener('scroll', state.convoScrollHandler);
    state.convoScrollHandler = null;
    const hs = state.openSheetId && state.history.get(state.openSheetId);
    if (hs) hs.loading = false;
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
  function openSheet(id) {
    const s = state.sessions.get(id);
    if (!s) return;

    if ($('#sheet')) closeSheet();
    state.openSheetId = id;

    const scrim = el('div', 'scrim');
    scrim.id = 'scrim';
    scrim.addEventListener('click', closeSheet);
    document.body.appendChild(scrim);

    const sheet = el('div', 'sheet');
    sheet.id = 'sheet';
    document.body.appendChild(sheet);

    sheet.innerHTML = sheetHeadHTML(s);
    buildDetailSheet(sheet, s);
  }

  function sheetHeadHTML(s) {
    // Open the embedded xterm.js terminal page. Same-origin, so the token is
    // read from localStorage by terminal.html — we deliberately do NOT put it
    // in the URL (avoids leaking it into history / target=_blank referrers).
    const termURL = '/terminal.html?id=' + encodeURIComponent(s.id);
    return (
      '<div class="grabber"></div>' +
      '<div class="sheet-head"><div class="sheet-title-row">' +
        '<div style="flex:1;min-width:0">' +
          '<div class="sheet-title">' + esc(s.title || s.id) + '</div>' +
          '<div class="sheet-sub"><span class="harness"><span class="gi">✦</span>' + esc(s.tool || 'agent') + '</span>' +
            '<span>' + esc(groupKey(s)) + '</span></div>' +
        '</div>' +
        '<button class="icon-btn sheet-check" id="sheetCheckPerm" title="Check for approval"><span class="gi">🔐</span></button>' +
        '<a class="sheet-term" title="Open full web terminal" target="_blank" rel="noopener" href="' + termURL + '">⧉</a>' +
      '</div></div>'
    );
  }

  // ----- detail sheet (ask + last reply + async pending + gated approve) -----
  function buildDetailSheet(sheet, s) {
    // Prominent LIVE "what it's doing now" area, kept fresh by a ~3s probe of
    // /api/rc/activity while the sheet is open. Starts from the list snapshot so
    // there's no flash of "idle" before the first poll lands.
    const live = el('div', 'live-activity');
    live.id = 'liveActivity';
    sheet.appendChild(live);

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

    // Approve section mounts inside the scrolling convo; populated only when a
    // real pending dialog is detected. Starts empty.
    const approveMount = el('div');
    approveMount.id = 'approveMount';
    convo.appendChild(approveMount);

    // Wire the header "Check for approval" affordance.
    const checkBtn = $('#sheetCheckPerm', sheet);
    if (checkBtn) checkBtn.addEventListener('click', () => checkPermission(s, true));

    const workingBanner = el('div', 'working-banner');
    workingBanner.id = 'workingBanner';
    workingBanner.hidden = true;
    workingBanner.innerHTML =
      '<span class="spin"></span><span>Turn in progress — you\'ll get a push when it\'s done. You can keep typing.</span>';
    sheet.appendChild(workingBanner);

    const rail = el('div', 'slash-rail');
    const slashes = ['/compact', '/context', '/clear', '/diff'];
    slashes.forEach((cmd) => {
      const b = el('button', 'slash', cmd);
      b.addEventListener('click', () => sendSlash(s.id, cmd));
      rail.appendChild(b);
    });
    const custom = el('button', 'slash muted', '＋ command');
    custom.addEventListener('click', () => {
      const cmd = prompt('Slash command (leading / optional):');
      if (cmd && cmd.trim()) sendSlash(s.id, cmd.trim());
    });
    rail.appendChild(custom);
    sheet.appendChild(rail);

    const composer = el('div', 'composer');
    const ta = el('textarea');
    ta.rows = 1;
    ta.placeholder = 'Message ' + (s.title || 'this session') + '…';
    ta.addEventListener('input', () => {
      ta.style.height = 'auto';
      ta.style.height = Math.min(ta.scrollHeight, 110) + 'px';
      send.disabled = !ta.value.trim();
    });
    ta.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); send.click(); }
    });
    const send = el('button', 'send-btn', '↑');
    send.disabled = true;
    send.addEventListener('click', () => {
      const text = ta.value.trim();
      if (!text) return;
      ta.value = '';
      ta.style.height = 'auto';
      send.disabled = true;
      sendAsk(s.id, text);
    });
    composer.appendChild(ta);
    composer.appendChild(send);
    sheet.appendChild(composer);

    // Seed the live area from the list snapshot, then poll for fresh state.
    renderLiveActivity(s.id, { working: s.working, activity: s.activity, currentTool: s.currentTool });
    startActivityPoll(s.id);

    renderConvo(s.id);
    loadHistory(s.id);
    checkPermission(s, false); // auto-check for a real pending dialog on open
  }

  // ----- live activity (detail sheet) -----
  // Render the prominent "what it's doing now" block. When working, show the
  // thinking/processing line + currentTool (Task(…) → subagent). When idle,
  // show a subtle waiting note; the full last reply still renders in the convo.
  function renderLiveActivity(id, a) {
    if (state.openSheetId !== id) return;
    const live = $('#liveActivity');
    if (!live) return;
    if (a && a.working) {
      live.className = 'live-activity working';
      const toolText = toolLabel(a.currentTool);
      live.innerHTML =
        '<div class="la-row"><span class="spin"></span>' +
          '<span class="la-text">' + esc(a.activity || 'Working…') + '</span></div>' +
        (toolText ? '<div class="la-tool">' + esc(oneLine(toolText, 90)) + '</div>' : '');
    } else {
      live.className = 'live-activity idle';
      live.innerHTML = '<div class="la-row"><span class="la-dot"></span>' +
        '<span class="la-text">idle · waiting</span></div>';
    }
  }

  function startActivityPoll(id) {
    stopActivityPoll();
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
        }
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
      if (r && r.content) {
        const conv = ensureConvo(id);
        // Only seed if we don't already have this as the latest reply.
        const hasReply = conv.some((e) => e.role === 'reply' && e.content === r.content);
        if (!hasReply) {
          conv.push({ role: 'reply', content: r.content, ts: r.timestamp });
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

  // Map a history entry (role "user"|"reply", unix-seconds ts) onto a convo turn.
  function historyToTurn(m) {
    const role = m.role === 'user' ? 'me' : 'reply';
    return { role, content: m.content || '', ts: m.ts };
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
      const conv = ensureConvo(id);
      // Keep any live turns the user fired before history landed (e.g. queued
      // 'me' rows / errors): prepend the history snapshot ahead of them.
      const live = conv.slice();
      conv.length = 0;
      msgs.forEach((m) => conv.push(historyToTurn(m)));
      live.forEach((e) => conv.push(e));
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
    const turns = msgs.map(historyToTurn);
    let insertAt = 0;
    while (insertAt < conv.length && conv[insertAt].role === 'note') insertAt++;
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

  function renderConvo(id, opts) {
    if (state.openSheetId !== id) return;
    const convo = $('#convo');
    if (!convo) return;
    const conv = ensureConvo(id);

    // Preserve the approve section (it lives outside the turn list).
    const approveMount = $('#approveMount');
    convo.innerHTML = '';
    if (approveMount) convo.appendChild(approveMount);

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
        const b = el('div', 'bubble me' + (e.queued ? ' queued' : ''));
        b.textContent = e.content;
        convo.appendChild(b);
        const ts = el('div', 'ts me', fmtTime(e.ts) + (e.queued ? ' · sent' : ''));
        convo.appendChild(ts);
      } else if (e.role === 'reply') {
        // Merge consecutive 'reply' entries (one Claude turn can yield several
        // assistant text entries) into a single bubble; skip those folded in.
        if (i > 0 && conv[i - 1].role === 'reply') return;
        let content = e.content;
        let last = e;
        for (let j = i + 1; j < conv.length && conv[j].role === 'reply'; j++) {
          content += '\n\n' + conv[j].content;
          last = conv[j];
        }
        const b = el('div', 'bubble reply');
        b.innerHTML = renderMarkdown(content);
        convo.appendChild(b);
        convo.appendChild(el('div', 'ts', fmtTime(last.ts)));
      } else if (e.role === 'error') {
        const b = el('div', 'bubble err');
        b.textContent = '⛔ ' + e.content;
        convo.appendChild(b);
        convo.appendChild(el('div', 'ts', fmtTime(e.ts)));
      } else if (e.role === 'approved') {
        const b = el('div', 'approved-strip' + (e.bad ? ' bad' : ''));
        b.innerHTML = e.html;
        convo.appendChild(b);
      } else if (e.role === 'note') {
        const b = el('div', 'pending');
        b.innerHTML = '<span>' + esc(e.content) + '</span>';
        convo.appendChild(b);
      }
    });

    // Active pending row.
    const pend = state.pending.get(id);
    if (pend) {
      const p = el('div', 'pending');
      p.innerHTML = '<span class="tdots"><i></i><i></i><i></i></span>' +
        '<span>Claude is working… <span class="ctx">' + esc(pend.ctx || 'thinking') + '</span></span>';
      convo.appendChild(p);
    }
    const wb = $('#workingBanner');
    if (wb) wb.hidden = !pend;

    // Older-paging re-renders restore scroll position themselves (the caller
    // anchors on inserted height); everything else sticks to the bottom.
    if (!(opts && opts.keepScroll)) convo.scrollTop = convo.scrollHeight;
  }

  // Tiny, safe markdown: escapes first, then re-introduces a few inline forms.
  function renderMarkdown(src) {
    let s = esc(src);
    // fenced code blocks
    s = s.replace(/```(?:\w+)?\n?([\s\S]*?)```/g, (_, code) => '<pre><code>' + code.replace(/\n$/, '') + '</code></pre>');
    // inline code
    s = s.replace(/`([^`\n]+)`/g, '<code>$1</code>');
    // bold
    s = s.replace(/\*\*([^*]+)\*\*/g, '<b>$1</b>');
    // headers (line-start #...)
    s = s.replace(/^(#{1,6})\s*(.+)$/gm, '<span class="md-h">$2</span>');
    // bullet lists
    s = s.replace(/(?:^|\n)((?:- .+(?:\n|$))+)/g, (m, block) => {
      const items = block.trim().split('\n').map((l) => '<li>' + l.replace(/^- /, '') + '</li>').join('');
      return '<ul>' + items + '</ul>';
    });
    // remaining newlines
    s = s.replace(/\n/g, '<br>');
    return s;
  }

  // ----- ask / slash -----
  async function sendAsk(id, text) { await doSend(id, text, false); }
  async function sendSlash(id, text) { await doSend(id, text, true); }

  async function doSend(id, text, slash) {
    const conv = ensureConvo(id);
    conv.push({ role: 'me', content: text, ts: Date.now() / 1000, queued: true });
    // Non-blocking pending state shown immediately.
    state.pending.set(id, { ctx: slash ? text : 'thinking' });
    renderConvo(id);
    renderDeck();

    try {
      const path = slash ? '/api/rc/slash' : '/api/rc/ask';
      const r = await api('POST', path, { sessionId: id, text });
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

  // ----- gated approve -----
  // Ask the server whether a REAL permission dialog is on screen. Only render
  // the Approve control when pending:true. Never inferred from status.
  async function checkPermission(s, userInitiated) {
    const mount = $('#approveMount');
    if (!mount || state.openSheetId !== s.id) return;

    const btn = $('#sheetCheckPerm');
    if (btn) btn.classList.add('busy');
    try {
      const r = await api('GET', '/api/rc/permission?id=' + encodeURIComponent(s.id));
      if (state.openSheetId !== s.id) return;
      if (r && r.pending) {
        renderApprovePanel(s, r.text || '');
      } else if (r && r.unavailable) {
        mount.innerHTML = '';
        if (userInitiated) mount.appendChild(approveNote('Couldn\'t read this session\'s screen.'));
      } else {
        mount.innerHTML = '';
        if (userInitiated) mount.appendChild(approveNote('No pending approval.'));
      }
    } catch (e) {
      if (state.openSheetId !== s.id) return;
      mount.innerHTML = '';
      if (userInitiated) mount.appendChild(approveNote('Couldn\'t check approval: ' + e.message));
    } finally {
      if (btn) btn.classList.remove('busy');
    }
  }

  function approveNote(text) {
    const n = el('div', 'approve-note');
    n.textContent = text;
    return n;
  }

  // Render the Approve panel with the ACTUAL permission text the server read.
  function renderApprovePanel(s, permText) {
    const mount = $('#approveMount');
    if (!mount) return;
    const wrap = el('div', 'approve-card');
    wrap.innerHTML =
      '<div class="approve-head"><span class="dot"></span> Permission requested</div>' +
      '<div class="approve-body">' +
        '<div class="approve-q">' + esc(s.tool === 'claude' ? 'Claude' : (s.tool || 'The agent')) +
          ' is paused on a permission prompt. Review the request below before approving.</div>' +
        '<div class="approve-what">' +
          '<div class="label">Permission request</div>' +
          '<pre class="perm-text" id="permText"></pre>' +
        '</div>' +
        '<div class="hold-approve" id="holdApprove"><div class="hold-fill" id="holdFill"></div>' +
          '<div class="hold-label" id="holdLabel">Hold to approve</div></div>' +
        '<div class="approve-foot" id="approveFoot">Approving sends the confirm keystroke to the live dialog. ' +
          'To deny, reply with guidance instead.</div>' +
      '</div>';
    mount.innerHTML = '';
    mount.appendChild(wrap);
    // Set the real text as textContent so line breaks/monospace are preserved
    // verbatim and nothing is interpreted as HTML.
    $('#permText', wrap).textContent = permText || '(the server reported a pending dialog but returned no text)';

    wireHoldToApprove(wrap, s);
  }

  // Real press-and-hold (~800ms) with a fill ring; early release cancels.
  function wireHoldToApprove(wrap, s) {
    const hold = $('#holdApprove', wrap);
    const fill = $('#holdFill', wrap);
    const label = $('#holdLabel', wrap);
    const HOLD_MS = 800;
    let raf = null, start = 0, done = false;

    function begin(e) {
      if (done) return;
      e.preventDefault();
      start = performance.now();
      hold.classList.add('armed');
      label.textContent = 'Keep holding…';
      tick();
    }
    function tick() {
      const p = Math.min(1, (performance.now() - start) / HOLD_MS);
      fill.style.width = (p * 100) + '%';
      if (p >= 1) { finish(); return; }
      raf = requestAnimationFrame(tick);
    }
    function cancel() {
      if (done) return;
      cancelAnimationFrame(raf);
      fill.style.width = '0%';
      hold.classList.remove('armed');
      label.textContent = 'Hold to approve';
    }
    function finish() {
      done = true;
      cancelAnimationFrame(raf);
      fill.style.width = '100%';
      hold.classList.add('done', 'busy');
      label.textContent = '✓ Approving…';
      doApprove(s);
    }
    hold.addEventListener('mousedown', begin);
    hold.addEventListener('touchstart', begin, { passive: false });
    ['mouseup', 'mouseleave', 'touchend', 'touchcancel'].forEach((ev) => hold.addEventListener(ev, cancel));
  }

  function resetHold() {
    const hold = $('#holdApprove');
    if (hold) {
      hold.classList.remove('done', 'busy', 'armed');
      const f = $('#holdFill'); if (f) f.style.width = '0%';
      const l = $('#holdLabel'); if (l) l.textContent = 'Hold to approve';
    }
  }

  async function doApprove(s) {
    try {
      const r = await api('POST', '/api/rc/approve', { sessionId: s.id });
      if (r && r.approved) {
        const conv = ensureConvo(s.id);
        conv.push({
          role: 'approved', bad: false,
          html: '✅ Approved' + (r.cleared ? ' · dialog cleared' : ' · sent (confirming…)') + ' · ' + fmtTime(),
        });
        // Dialog handled — remove the approve panel and drop into the thread.
        const mount = $('#approveMount');
        if (mount) mount.innerHTML = '';
        state.pending.set(s.id, { ctx: 'running approved action' });
        renderConvo(s.id);
      } else {
        // approved=false with reason = safe no-op (no dialog on screen).
        const reason = (r && r.reason) || 'No permission dialog on screen.';
        const foot = $('#approveFoot');
        if (foot) { foot.textContent = reason + ' Nothing was sent.'; foot.style.color = 'var(--yellow)'; }
        resetHold();
        toast(s.title || 'Session', reason);
      }
    } catch (e) {
      const foot = $('#approveFoot');
      if (foot) { foot.textContent = 'Approve failed: ' + e.message; foot.style.color = 'var(--red)'; }
      resetHold();
    }
  }

  // ---------------------------------------------------------------------------
  // SSE streams
  // ---------------------------------------------------------------------------
  let evRC = null;

  function teardownStreams() {
    if (evRC) { evRC.close(); evRC = null; }
  }

  function setConn(text, cls) {
    const strip = $('#connStrip');
    if (!strip) return;
    strip.className = 'conn' + (cls ? ' ' + cls : '');
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
    const id = ev.sessionId;
    if (ev.type === 'ask-state' && ev.state === 'sent') {
      // already reflected locally on send; ensure pending exists
      if (id && !state.pending.has(id)) state.pending.set(id, { ctx: 'thinking', requestId: ev.requestId });
      renderConvo(id);
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
        conv.push({ role: 'reply', content: ev.content || '', ts: ev.ts });
        notifyForeground(id, 'finished', titleFor(id) + ' replied', oneLine(ev.content, 90));
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
      } else {
        conv.push({ role: 'note', content: '✓ ' + (ev.command || 'command') + ' sent', ts: ev.ts });
      }
      renderConvo(id);
      renderDeck();
    } else if (ev.type === 'approve-result') {
      // The approve outcome is reflected inline by doApprove. If the SSE event
      // reports an outcome for the open session, surface it too.
      if (id && state.openSheetId === id && ev.approved === false) {
        const foot = $('#approveFoot');
        const reason = ev.reason || 'No permission dialog on screen.';
        if (foot) { foot.textContent = reason + ' Nothing was sent.'; foot.style.color = 'var(--yellow)'; }
        resetHold();
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
    tg.addEventListener('click', () => {
      prefs[tg.dataset.pref] = !prefs[tg.dataset.pref];
      tg.classList.toggle('on', prefs[tg.dataset.pref]);
      savePrefs(prefs);
    });
  });
  $('#btnEnablePush').addEventListener('click', enablePush);
  $('#forgetToken').addEventListener('click', () => {
    if (confirm('Forget the token on this device?')) forgetToken('Token cleared.');
  });

  // Escape hatch → embedded xterm.js terminal page. We deep-link to the open
  // session if any, else the most recent session; with no sessions at all we
  // fall back to the app root. terminal.html reads the token from localStorage
  // (same origin), so it stays out of the URL.
  $('#escapeTerm').addEventListener('click', (e) => {
    e.preventDefault();
    const id = state.openSheetId || (Array.from(state.sessions.keys())[0]);
    const url = id ? '/terminal.html?id=' + encodeURIComponent(id) : tokenURL('/');
    window.open(url, '_blank', 'noopener');
  });

  // ---------------------------------------------------------------------------
  // SW message: deep-link from a tapped notification
  // ---------------------------------------------------------------------------
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.addEventListener('message', (e) => {
      if (e.data && e.data.type === 'open-session' && e.data.sessionId) {
        showTab('deck');
        openSheet(e.data.sessionId);
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

  async function boot() {
    showTab('deck');
    registerSW();
    wirePresence();
    startStreams();
    await loadSessions(true);
    startRefresh();
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible') loadSessions(false);
    });
    refreshPushUI();
  }

  // Entry point.
  if (!TOKEN) {
    showTokenScreen();
  } else {
    appEl.hidden = false;
    boot();
  }
})();
