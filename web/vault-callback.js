// The Vault OIDC redirect target (/vault/callback, docs/design/run-bang-vault-sign.md §2.4 step 2).
// It carries state/code only: the code is single use and worthless without the client_nonce the
// arm sheet holds, and no token ever passes here. The page strips them from the address bar,
// hands them to the app in this tab (sessionStorage, read once by app.js) only when an arm is
// pending in this same storage, and goes back to the app. Nothing is sent anywhere.
(function () {
  'use strict';
  const q = new URLSearchParams(location.search);
  const cb = { state: q.get('state') || '', code: q.get('code') || '', error: (q.get('error_description') || q.get('error') || '').slice(0, 200), at: Date.now() };
  history.replaceState(null, '', location.pathname);
  let pending = false;
  try {
    pending = !!sessionStorage.getItem('deckArmPending');
    if (pending) sessionStorage.setItem('deckArmCallback', JSON.stringify(cb));
  } catch (_) { /* storage off: fall through to the message */ }
  if (pending) {
    location.replace('/#arm-callback');
    return;
  }
  document.getElementById('msg').textContent = cb.error
    ? 'Vault sign-in failed: ' + cb.error
    : 'This sign-in finished outside the deck-remote app (no arm is waiting in this window). Open deck-remote and tap Arm again; if this keeps happening, use the laptop path (run_bang.py seq arm --via tui --payload).';
})();
