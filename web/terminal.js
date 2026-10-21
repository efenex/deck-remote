// The terminal page's script (was inline in terminal.html; moved out for the CSP's script-src 'self',
// run-bang-phase2-review §5 #7).
(function () {
  'use strict';

  var TOKEN_KEY = 'deck_remote_token';

  // ---- params -------------------------------------------------------------
  var params = new URLSearchParams(location.search);
  var sessionId = params.get('id') || '';
  // Token: prefer localStorage (set when the deck was opened with ?token=),
  // fall back to an explicit ?token= for direct links. It only ever travels in the
  // Authorization header of the ticket request; the WebSocket URL carries the ticket.
  var token = localStorage.getItem(TOKEN_KEY) || params.get('token') || '';

  var $term = document.getElementById('term');
  var $sid = document.getElementById('sid');
  var $conn = document.getElementById('conn');
  var $connText = document.getElementById('conntext');
  var $notice = document.getElementById('notice');
  var $noticeTitle = document.getElementById('noticeTitle');
  var $noticeMsg = document.getElementById('noticeMsg');
  var $noticeActions = document.getElementById('noticeActions');

  $sid.textContent = sessionId ? ('session · ' + sessionId) : 'no session';
  // Back lands on this session's thread, not the bare deck.
  if (sessionId) document.querySelector('#bar a.back').href = '/?session=' + encodeURIComponent(sessionId);

  function setConn(stateName, text) {
    $conn.className = stateName; // '', 'live', 'bad'
    $connText.textContent = text;
  }

  function showNotice(title, msg, actionsHTML) {
    $noticeTitle.textContent = title;
    $noticeMsg.textContent = msg;
    $noticeActions.innerHTML = actionsHTML || '';
    $notice.classList.add('show');
  }
  function hideNotice() { $notice.classList.remove('show'); }

  // ---- guards -------------------------------------------------------------
  if (!sessionId) {
    setConn('bad', 'no session');
    showNotice('No session selected',
      'This page needs a session id. Open the deck and pick a session, then tap the terminal icon.',
      '<a class="btn" href="/">Open the deck</a>');
    return;
  }
  if (!token) {
    setConn('bad', 'no token');
    showNotice('Open from the deck first',
      'No saved token on this device. Open the deck (which stores your token), then reopen the terminal.',
      '<a class="btn" href="/">Open the deck</a>');
    return;
  }

  // ---- xterm --------------------------------------------------------------
  var isMobile = typeof window.matchMedia === 'function' &&
    window.matchMedia('(pointer: coarse)').matches;

  // 5.x UMD globals (verified in the vendored files).
  var Terminal = window.Terminal;
  var FitAddon = window.FitAddon && window.FitAddon.FitAddon;

  var terminal = new Terminal({
    convertEol: false,
    cursorBlink: !isMobile,
    disableStdin: false,
    fontFamily: 'IBM Plex Mono, Menlo, Consolas, monospace',
    fontSize: 13,
    scrollback: 10000,
    theme: {
      background: '#16161e',
      foreground: '#d9e2ec',
      cursor: '#9ecbff',
    },
  });
  var fitAddon = new FitAddon();
  terminal.loadAddon(fitAddon);
  terminal.open($term);

  // State mirrored from TerminalPanel.js:
  //  - terminalAttached: only forward input/resize once the server confirms.
  //  - readOnly: server gates writes; we mirror disableStdin.
  //  - decoder: stream-decode binary frames so multi-byte UTF-8 isn't split.
  var ws = null;
  var terminalAttached = false;
  var readOnly = false;
  var decoder = new TextDecoder();
  var reconnectTimer = null;
  var reconnectAttempt = 0;
  var wsReconnectEnabled = true;
  var manuallyClosed = false;
  var ticketPending = false;
  // Give up after a few auto-retries and offer a manual Reconnect, so a dead
  // backend doesn't leave us spinning forever.
  var MAX_RECONNECT_ATTEMPTS = 6;

  // ---- on-screen keyboard -------------------------------------------------
  // iOS doesn't shrink the layout viewport for the keyboard, so the fixed
  // #wrap kept its full height and the bottom rows (the input line) sat
  // behind the keyboard. Track the visual viewport instead. While the
  // keyboard is up we don't refit: the tmux pane is shared with the laptop,
  // and #term is bottom-aligned so the top rows are what gets clipped.
  var $wrap = document.getElementById('wrap');
  var vv = window.visualViewport;
  var keyboardOpen = false;
  function onViewport() {
    if (!vv) return;
    keyboardOpen = vv.height < window.innerHeight - 120;
    $wrap.classList.toggle('kb', keyboardOpen);
    $wrap.style.height = keyboardOpen ? vv.height + 'px' : '';
    $wrap.style.bottom = keyboardOpen ? 'auto' : '';
    $wrap.style.transform = keyboardOpen && vv.offsetTop ? 'translateY(' + vv.offsetTop + 'px)' : '';
    // Safe-area bottom padding belongs to the home bar, not the keyboard.
    $wrap.style.paddingBottom = keyboardOpen ? '0px' : '';
  }
  if (vv) {
    vv.addEventListener('resize', onViewport);
    vv.addEventListener('scroll', onViewport);
  }

  function doFit() {
    if (keyboardOpen) return;
    if (!$term.offsetWidth || !$term.offsetHeight) return;
    try { fitAddon.fit(); } catch (_e) { return; }
    var cols = terminal.cols, rows = terminal.rows;
    if (cols >= 10 && rows >= 3 && ws && ws.readyState === WebSocket.OPEN && terminalAttached) {
      ws.send(JSON.stringify({ type: 'resize', cols: cols, rows: rows }));
    }
  }

  var fitTimer = null;
  function scheduleFit(delay) {
    clearTimeout(fitTimer);
    fitTimer = setTimeout(doFit, delay);
  }

  if ($term.offsetWidth && $term.offsetHeight) { try { fitAddon.fit(); } catch (_e) {} }

  window.addEventListener('resize', function () { scheduleFit(120); });
  window.addEventListener('orientationchange', function () { scheduleFit(250); });
  if (typeof ResizeObserver === 'function') {
    new ResizeObserver(function () { scheduleFit(90); }).observe($term);
  }

  // Mobile: tapping the terminal focuses the xterm helper textarea to raise
  // the on-screen keyboard.
  $term.addEventListener('click', function () { terminal.focus(); });

  // Key bar: send the sequence without taking focus from xterm's textarea (so
  // the keyboard stays up).
  var KEYS = { esc: '\x1b', tab: '\t', ctrlc: '\x03', up: '\x1b[A', down: '\x1b[B', enter: '\r' };
  Array.prototype.forEach.call(document.querySelectorAll('#keys button'), function (b) {
    b.addEventListener('mousedown', function (e) { e.preventDefault(); });
    b.addEventListener('touchstart', function (e) { e.preventDefault(); b.click(); }, { passive: false });
    b.addEventListener('click', function () {
      var data = KEYS[b.dataset.key];
      if (!data || !ws || ws.readyState !== WebSocket.OPEN || !terminalAttached || readOnly) return;
      ws.send(JSON.stringify({ type: 'input', data: data }));
    });
  });

  // Input forwarding — gated on terminalAttached + !readOnly, exactly as the
  // reference client does (server also enforces read-only).
  terminal.onData(function (data) {
    if (!ws || ws.readyState !== WebSocket.OPEN || !terminalAttached || readOnly) return;
    ws.send(JSON.stringify({ type: 'input', data: data }));
  });

  // ---- WebSocket ----------------------------------------------------------
  // /ws/ takes a 30 s single-use ticket (ws_ticket.go), never the bearer: every connect and
  // reconnect fetches a fresh one with the token in the Authorization header.
  function wsURL(ticket) {
    var proto = location.protocol === 'https:' ? 'wss' : 'ws';
    var u = new URL(proto + '://' + location.host + '/ws/session/' + encodeURIComponent(sessionId));
    u.searchParams.set('ticket', ticket);
    return u.toString();
  }

  function fetchTicket() {
    return fetch('/api/rc/ws/ticket', {
      method: 'POST',
      cache: 'no-store',
      headers: { 'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json' },
      body: JSON.stringify({ session: sessionId })
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { return { status: r.status, body: j || {} }; });
    });
  }

  function reconnectDelay(attempt) {
    var capped = Math.min(attempt, 8);
    return Math.min(8000, Math.round(350 * Math.pow(1.8, capped - 1)));
  }

  function giveUpReconnect() {
    setConn('bad', 'disconnected');
    showNotice('Disconnected',
      'Could not reach the terminal after several attempts.',
      '<span class="btn" id="reconnectBtn">Reconnect</span>' +
      '<a class="btn ghost" href="/">Back to deck</a>');
    wireReconnect();
  }

  function scheduleReconnect() {
    if (!wsReconnectEnabled || manuallyClosed) return;
    if (reconnectTimer || ws) return;
    if (reconnectAttempt >= MAX_RECONNECT_ATTEMPTS) { giveUpReconnect(); return; }
    reconnectAttempt += 1;
    setConn('', 'reconnecting… (' + reconnectAttempt + '/' + MAX_RECONNECT_ATTEMPTS + ')');
    reconnectTimer = setTimeout(function () {
      reconnectTimer = null;
      connect();
    }, reconnectDelay(reconnectAttempt));
  }

  function connect() {
    if (ws) { try { ws.close(); } catch (_e) {} ws = null; }
    if (ticketPending) return;
    terminalAttached = false;
    manuallyClosed = false;
    hideNotice();
    setConn('', 'connecting…');
    ticketPending = true;
    fetchTicket().then(function (res) {
      ticketPending = false;
      if (res.status === 200 && res.body.ticket) { openSocket(res.body.ticket); return; }
      if (res.status >= 400 && res.status < 500) {
        // Refused (read-only token, wrong identity, LAN origin): retrying will not help.
        wsReconnectEnabled = false;
        setConn('bad', 'refused');
        showNotice('Terminal refused', res.body.error || ('HTTP ' + res.status),
          '<span class="btn" id="reconnectBtn">Retry</span>' +
          '<a class="btn ghost" href="/">Back to deck</a>');
        wireReconnect();
        return;
      }
      setConn('bad', 'connection error');
      scheduleReconnect();
    }, function () {
      ticketPending = false;
      setConn('bad', 'connection error');
      scheduleReconnect();
    });
  }

  function openSocket(ticket) {
    if (manuallyClosed) return;
    ws = new WebSocket(wsURL(ticket));
    ws.binaryType = 'arraybuffer';
    var sock = ws;

    sock.onopen = function () {
      if (ws !== sock) return;
      if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
      reconnectAttempt = 0;
      setConn('live', 'connected');
      sock.send(JSON.stringify({ type: 'ping' }));
    };

    sock.onmessage = function (event) {
      if (ws !== sock) return;
      // String frame => control JSON (status / error). Never write these into
      // the terminal — mirror TerminalPanel.js onmessage exactly.
      if (typeof event.data === 'string') {
        var payload;
        try { payload = JSON.parse(event.data); }
        catch (_e) { return; } // ignore non-JSON control messages
        if (payload.type === 'status') {
          if (payload.event === 'connected') {
            readOnly = !!payload.readOnly;
            terminal.options.disableStdin = readOnly;
            setConn('live', readOnly ? 'connected (read-only)' : 'connected');
          } else if (payload.event === 'terminal_attached') {
            terminalAttached = true;
            scheduleFit(0);
          } else if (payload.event === 'session_closed') {
            terminalAttached = false;
          }
        } else if (payload.type === 'error') {
          if (payload.code === 'TERMINAL_ATTACH_FAILED' || payload.code === 'TMUX_SESSION_NOT_FOUND') {
            terminalAttached = false;
          }
          // TMUX_SESSION_NOT_FOUND is terminal-fatal: the session is gone, so
          // stop reconnecting and surface a banner (reconnect button rebuilds).
          if (payload.code === 'TMUX_SESSION_NOT_FOUND') {
            wsReconnectEnabled = false;
            setConn('bad', 'session gone');
            showNotice('Terminal disconnected',
              payload.message || 'tmux session is not available' +
                (payload.hint ? (' — ' + payload.hint) : ''),
              '<span class="btn" id="reconnectBtn">Reconnect</span>' +
              '<a class="btn ghost" href="/">Back to deck</a>');
            wireReconnect();
            return;
          }
          terminal.write('\r\n[error:' + (payload.code || 'unknown') + '] ' +
            (payload.message || 'unknown error') + '\r\n');
        }
        return;
      }
      // Binary frame => raw terminal output.
      if (event.data instanceof ArrayBuffer) {
        terminal.write(decoder.decode(new Uint8Array(event.data), { stream: true }));
      }
    };

    sock.onerror = function () {
      if (ws !== sock) return;
      setConn('bad', 'connection error');
    };

    sock.onclose = function () {
      if (ws !== sock) return;
      ws = null;
      terminalAttached = false;
      if (manuallyClosed) return;
      if (wsReconnectEnabled) {
        scheduleReconnect();
        return;
      }
      setConn('bad', 'disconnected');
      showNotice('Disconnected',
        'The terminal connection was closed.',
        '<span class="btn" id="reconnectBtn">Reconnect</span>' +
        '<a class="btn ghost" href="/">Back to deck</a>');
      wireReconnect();
    };
  }

  function wireReconnect() {
    var btn = document.getElementById('reconnectBtn');
    if (!btn) return;
    btn.addEventListener('click', function () {
      wsReconnectEnabled = true;
      reconnectAttempt = 0;
      hideNotice();
      connect();
    }, { once: true });
  }

  terminal.writeln('Connecting to terminal…');
  connect();
  if (!isMobile) terminal.focus();
})();
