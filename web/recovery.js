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

  function listResponse(headers, now = Date.now()) {
    const rawAge = headers && headers.get('X-Sessions-Age-Ms');
    const age = rawAge == null || rawAge === '' ? NaN : Number(rawAge);
    const validAge = Number.isFinite(age) && age >= 0;
    const kind = headers && headers.get('X-Sessions-Cache');
    // sessions.go's freshness window is 4 s. A failed rebuild of an expired
    // cache can return kind=miss with old data, so inspect age as well as kind.
    const stale = kind === 'stale' || validAge && age >= 4000;
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

  const api = { listFeedback, listResponse, updateWorker };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckRecovery = api;
})(typeof window !== 'undefined' ? window : globalThis);
