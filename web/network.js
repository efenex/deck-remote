// Bounded requests include body consumption: a server can stall after headers.
(function (root) {
  'use strict';
  async function request(fetcher, url, options, read, timeout = 12000) {
    const controller = new AbortController();
    let timer;
    try {
      return await Promise.race([
        Promise.resolve().then(() => fetcher(url, Object.assign({}, options, { signal: controller.signal }))).then(read),
        new Promise((_, reject) => {
          timer = setTimeout(() => {
            reject(new Error('Host did not respond within ' + timeout / 1000 + ' s'));
            controller.abort();
          }, timeout);
        }),
      ]);
    } finally { clearTimeout(timer); }
  }
  function retryDelay(failures) { return Math.min(30000, 1000 * Math.pow(2, Math.min(failures, 5))); }
  function feedback(s, now = Date.now()) {
    const contact = s.lastContact ? new Date(s.lastContact).toISOString() : 'never';
    return 'Host unreachable · attempt ' + s.attempt +
      (s.nextRetry ? ' · next retry in ' + Math.max(0, Math.ceil((s.nextRetry - now) / 1000)) + ' s' : ' · connecting…') +
      ' · last error: ' + (s.error || 'connection lost') + ' · last contact: ' + contact +
      ' · cached data may be stale' + (s.failures >= 3 ? ' · Host may be asleep.' : '');
  }
  const api = { request, retryDelay, feedback };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckNetwork = api;
})(typeof window !== 'undefined' ? window : globalThis);
