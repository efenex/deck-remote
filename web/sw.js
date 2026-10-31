/* deck-remote service worker
 *
 * Responsibilities:
 *   1. Cache the app shell so the PWA is installable and opens offline.
 *   2. Handle Web Push: render a notification from the push payload.
 *   3. Handle notificationclick: focus an open client (or open one) and tell it
 *      which session to deep-link into.
 *
 * Scope is "/" (registered from the root). No external/CDN deps.
 */

// Bump with the ?v= on index.html's script tags.
const CACHE = 'deck-remote-v46';
const SHELL_NET_TIMEOUT = 1500; // ms before the shell is served from cache
const SHELL = [
  '/',
  '/index.html',
  '/theme.js?v=37',
  '/version.js?v=37',
  '/viewport.js?v=37',
  '/pulse.js?v=37',
  '/runbang.js?v=46',
  '/pair.js?v=37',
  '/thread.js?v=45',
  '/markdown.js?v=37',
  '/gestures.js?v=37',
  '/network.js?v=45',
  '/photos.js?v=42',
  '/app.js?v=46',
  '/manifest.webmanifest',
  // Embedded terminal page + its vendored (offline) xterm assets, so the
  // escape-hatch terminal works without a network round-trip to a CDN.
  '/terminal.html',
  '/terminal.js',
  '/vendor/xterm.js',
  '/vendor/xterm.css',
  '/vendor/xterm-addon-fit.js',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

/* Fetch strategy:
 *   - Never touch API / SSE / WS / push traffic — those must hit the network and
 *     carry auth; caching them would break live data and bearer-gated calls.
 *   - App shell + same-origin GETs: network-first, fall back to cache offline so
 *     the installed app still launches without connectivity.
 */
self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  if (
    url.pathname.startsWith('/api/') ||
    url.pathname.startsWith('/events/') ||
    url.pathname.startsWith('/ws/') ||
    url.pathname.startsWith('/vault/') // the OIDC callback's URL carries a one-time code: never cached
  ) {
    return; // let the network handle live endpoints
  }

  // Network-first (so a deploy shows up on the next launch) but never hang on a
  // slow/unreachable server: after SHELL_NET_TIMEOUT answer from the cache and let
  // the network response refresh it for next time.
  const cached = () => caches.match(req, { ignoreSearch: true });
  const net = fetch(req).then((res) => {
    // Cache successful shell-ish responses for offline launch.
    if (res && res.ok && res.type === 'basic') {
      const copy = res.clone();
      caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
    }
    return res;
  });
  event.respondWith(new Promise((resolve) => {
    const t = setTimeout(() => cached().then((hit) => { if (hit) resolve(hit); }), SHELL_NET_TIMEOUT);
    net.then(
      (res) => { clearTimeout(t); resolve(res); },
      () => { clearTimeout(t); cached().then((hit) => (hit ? resolve(hit) : caches.match('/index.html').then(resolve))); }
    );
  }));
});

/* Push: render a notification from the server payload.
 *
 * Expected payload (best-effort; tolerant of partial/plain-text bodies):
 *   { title, body, sessionId, tag, kind, id, url }
 * id is the server's inbox entry; url deep-links a tap to it (/?notif=<id>).
 * kind ∈ reply | approval | question | stall | test.
 */
self.addEventListener('push', (event) => {
  let data = {};
  if (event.data) {
    try {
      data = event.data.json();
    } catch (_) {
      data = { body: event.data.text() };
    }
  }

  const title = data.title || 'deck-remote';
  const body = data.body || 'A session needs you.';
  const sessionId = data.sessionId || data.id || '';
  const tag = data.tag || (sessionId ? 'session-' + sessionId : 'deck-remote');

  const options = {
    body,
    tag,
    renotify: true,
    icon: ICON,
    badge: ICON,
    data: { sessionId, id: data.id || '', kind: data.kind || '', url: data.url || '/' },
    requireInteraction: data.kind === 'approval' || data.kind === 'question',
  };

  event.waitUntil(self.registration.showNotification(title, options));
});

/* notificationclick: focus an existing window and deep-link it to the inbox
 * entry (the full text), or open a new one at a URL that carries it. Pushes from
 * servers without an inbox id fall back to the session. */
self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const d = event.notification.data || {};
  const sessionId = d.sessionId || '';
  const notifId = d.id || '';
  const kind = d.kind || '';
  // The cold-start URL and the message to an already-open window carry the same three facts:
  // which inbox entry, which session, which kind. The app decides where that lands (an
  // approval/question opens its session; it must not just show the inbox behind an open sheet).
  const target = notifId
    ? '/?notif=' + encodeURIComponent(notifId) + (sessionId ? '&session=' + encodeURIComponent(sessionId) : '') + (kind ? '&kind=' + encodeURIComponent(kind) : '')
    : sessionId ? '/?session=' + encodeURIComponent(sessionId) : '/';
  const msg = notifId ? { type: 'open-notification', id: notifId, sessionId, kind } : { type: 'open-session', sessionId };

  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clients) => {
      for (const client of clients) {
        if ('focus' in client) {
          client.postMessage(msg);
          return client.focus();
        }
      }
      if (self.clients.openWindow) return self.clients.openWindow(target);
    })
  );
});

/* Inline SVG icon (data URI) so the worker needs no extra network fetch. */
const ICON =
  "data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'%20viewBox='0%200%20512%20512'%3E%3Crect%20width='512'%20height='512'%20rx='112'%20fill='%231a1b26'/%3E%3Crect%20x='96'%20y='150'%20width='320'%20height='212'%20rx='28'%20fill='%2324283b'%20stroke='%237aa2f7'%20stroke-width='10'/%3E%3Ccircle%20cx='168'%20cy='214'%20r='16'%20fill='%2373daca'/%3E%3Ccircle%20cx='168'%20cy='262'%20r='16'%20fill='%23e0af68'/%3E%3Ccircle%20cx='168'%20cy='310'%20r='16'%20fill='%23f7768e'/%3E%3C/svg%3E";
