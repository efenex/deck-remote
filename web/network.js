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
  // An answer walk presses one key per question and checks the live menu after each; on a loaded
  // host that is seconds per question, not a 12 s request. The budget grows with the question count.
  function answerBudget(questions) {
    return Math.min(120000, 20000 + 15000 * Math.max(1, questions | 0));
  }
  function retryDelay(failures) { return Math.min(30000, 1000 * Math.pow(2, Math.min(failures, 5))); }
  function feedback(s, now = Date.now()) {
    const contact = s.lastContact ? new Date(s.lastContact).toISOString() : 'never';
    return 'Host unreachable · attempt ' + s.attempt +
      (s.nextRetry ? ' · next retry in ' + Math.max(0, Math.ceil((s.nextRetry - now) / 1000)) + ' s' : ' · connecting…') +
      ' · last error: ' + (s.error || 'connection lost') + ' · last contact: ' + contact +
      ' · cached data may be stale' + (s.failures >= 3 ? ' · Host may be asleep.' : '');
  }
  const api = { request, answerBudget, retryDelay, feedback };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckNetwork = api;
})(typeof window !== 'undefined' ? window : globalThis);
