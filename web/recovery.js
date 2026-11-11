// Pure list freshness and bounded service-worker update helpers.
(function (root) {
  'use strict';
  function listFeedback(s, now = Date.now()) {
    const seconds = Math.max(0, Math.floor((now - s.at) / 1000));
    const age = seconds < 60 ? seconds + ' s' : seconds < 3600 ? Math.floor(seconds / 60) + ' min' :
      seconds < 86400 ? Math.floor(seconds / 3600) + ' h' : Math.floor(seconds / 86400) + ' d';
    const label = s.at ? 'List from ' + age + ' ago' : 'List age unknown (no successful refresh)';
    return label + (s.cached ? ' · cached' : '') +
      (s.error ? ' · refresh failed: ' + s.error : s.loading ? ' · refreshing…' : '') +
      (s.serverStale ? ' · server refresh pending or failed; retrying' : '');
  }

  const STALE_AFTER_MS = 45000;
  // Visible polling runs every 20 s: a list older than 90 s means polling is not landing
  // (frozen timers, hung request), so the UI must not claim Live over it.
  const LIST_OLD_MS = 90000;
  function listIsStale(s, visible = true, now = Date.now()) {
    return !!(s.cached || s.error || !s.at || (visible && now - s.at > LIST_OLD_MS));
  }

  function listResponse(headers, now = Date.now()) {
    const rawAge = headers && headers.get('X-Sessions-Age-Ms');
    const age = rawAge == null || rawAge === '' ? NaN : Number(rawAge);
    const validAge = Number.isFinite(age) && age >= 0;
    const kind = headers && headers.get('X-Sessions-Cache');
    // sessions.go serves stale-while-revalidate: with a 4 s fresh window, a 1.2 s grace
    // and builds of 2-5 s on a busy host, EVERY 20 s poll can legitimately answer with
    // the previous rebuild (kind=stale, age ~20 s). That is a healthy server, not a
    // failure, so only call the list server-stale once it is older than two poll
    // intervals plus a build. A failed rebuild of an expired cache (kind=miss, old
    // body) has a large age and lands here too. Without an age, trust kind=stale.
    const stale = validAge ? age >= STALE_AFTER_MS : kind === 'stale';
    return { at: stale && !validAge ? 0 : Math.max(0, now - (validAge ? age : 0)),
      cached: stale, serverStale: stale };
  }

  // update() resolves once the update check finishes, not once installation/activation does.
  // Keep Resync bounded if an update is stalled; do not reload into the old controller.
  async function updateWorker(reg, timeout = 12000) {
    if (!reg) return false;
    const before = reg.active;
    let timer, finished = false;
    let cleanup = () => {};
    try {
      return await Promise.race([
        (async () => {
          await reg.update();
          if (finished) return false;
          const worker = reg.installing || reg.waiting;
          if (worker) await new Promise((resolve, reject) => {
            const changed = () => {
              if (worker.state === 'activated') resolve();
              else if (worker.state === 'redundant') reject(new Error('App update installation failed'));
            };
            cleanup = () => worker.removeEventListener('statechange', changed);
            worker.addEventListener('statechange', changed);
            changed();
          });
          return !!before && reg.active !== before;
        })(),
        new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('App update check timed out')), timeout); }),
      ]);
    } finally { finished = true; clearTimeout(timer); cleanup(); }
  }

  const api = { listFeedback, listResponse, listIsStale, updateWorker };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckRecovery = api;
})(typeof window !== 'undefined' ? window : globalThis);
