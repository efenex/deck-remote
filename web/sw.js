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

const CACHE = 'deck-remote-v2';
const SHELL = [
  '/',
  '/index.html',
  '/app.js',
  '/manifest.webmanifest',
  // Embedded terminal page + its vendored (offline) xterm assets, so the
  // escape-hatch terminal works without a network round-trip to a CDN.
  '/terminal.html',
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
    url.pathname.startsWith('/ws/')
  ) {
    return; // let the network handle live endpoints
  }

  event.respondWith(
    fetch(req)
      .then((res) => {
        // Cache successful shell-ish responses for offline launch.
        if (res && res.ok && res.type === 'basic') {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
        }
        return res;
      })
      .catch(() =>
        caches.match(req).then((hit) => hit || caches.match('/index.html'))
      )
  );
});

/* Push: render a notification from the server payload.
 *
 * Expected payload (best-effort; tolerant of partial/plain-text bodies):
 *   { title, body, sessionId, tag, kind }
 * kind ∈ reply | approval | test (server vocabulary; requireInteraction keys off
 * "approval"). Field names match what pushManager.send() emits in push.go.
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
    data: { sessionId, kind: data.kind || '', url: data.url || '/' },
    requireInteraction: data.kind === 'approval',
  };

  event.waitUntil(self.registration.showNotification(title, options));
});

/* notificationclick: focus an existing window (and deep-link it to the session)
 * or open a new one at a URL that carries the session id. */
self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const sessionId = (event.notification.data && event.notification.data.sessionId) || '';
  const target = sessionId ? '/?session=' + encodeURIComponent(sessionId) : '/';

  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clients) => {
      for (const client of clients) {
        if ('focus' in client) {
          client.postMessage({ type: 'open-session', sessionId });
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
