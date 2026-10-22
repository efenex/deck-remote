// Phone pairing by QR (pair.go). Pure helpers plus the camera scanner. The QR the laptop shows
// (scripts/pair-phone.sh) holds <origin>/#pair=<code>: a single-use, 3-minute code, never the
// token. The scanner runs inside the PWA, so the token lands in the home-screen app's storage
// (opening the link from the Camera app pairs Safari instead). jsQR (vendor/, Apache-2.0) decodes
// frames; it loads on the first scan only. Same file is required by node --test (pair.test.js).
(function (root) {
  'use strict';

  const CODE_RE = /^[A-Za-z0-9_-]{40,64}$/;

  // parse(text, origin) -> { code } | { token } | { error }. Accepts a pairing URL (#pair=), a bare
  // pairing code, or an old-style ?token= enrolment URL. A URL for another origin is refused: its
  // code would be redeemed here, where it does not exist, so say which server it is for instead.
  function parse(text, origin) {
    const s = String(text || '').trim();
    if (!s) return { error: 'empty QR' };
    if (CODE_RE.test(s)) return { code: s };
    let u;
    try { u = new URL(s); } catch (_) { return { error: 'not a deck-remote pairing QR' }; }
    if (origin && u.origin !== origin) return { error: 'this QR is for ' + u.origin + ', not ' + origin };
    const h = new URLSearchParams(u.hash.replace(/^#/, ''));
    const code = h.get('pair');
    if (code) return CODE_RE.test(code) ? { code } : { error: 'malformed pairing code' };
    const tok = u.searchParams.get('token');
    if (tok) return { token: tok };
    return { error: 'not a deck-remote pairing QR' };
  }

  // fromHash(hash) -> the code of a #pair= fragment, or ''.
  function fromHash(hash) {
    const code = new URLSearchParams(String(hash || '').replace(/^#/, '')).get('pair') || '';
    return CODE_RE.test(code) ? code : '';
  }

  let jsqr = null;
  function loadDecoder() {
    if (root.jsQR) return Promise.resolve(root.jsQR);
    if (jsqr) return jsqr;
    jsqr = new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = '/vendor/jsQR.js';
      s.onload = () => (root.jsQR ? resolve(root.jsQR) : reject(new Error('QR decoder did not load')));
      s.onerror = () => { jsqr = null; reject(new Error('QR decoder did not load')); };
      document.head.appendChild(s);
    });
    return jsqr;
  }

  // scan(video) -> { result: Promise<string>, stop() }. Rear camera, ~7 decodes a second on a frame
  // scaled to ≤ 640 px. The promise resolves with the first QR text and stops the camera; stop()
  // rejects it with 'cancelled'.
  function scan(video) {
    let stream = null, timer = 0, done = false, rejectFn = null;
    const stop = (err) => {
      done = true;
      clearTimeout(timer);
      if (stream) stream.getTracks().forEach((t) => t.stop());
      video.srcObject = null;
      if (err && rejectFn) rejectFn(err);
    };
    const result = new Promise((resolve, reject) => {
      rejectFn = reject;
      if (!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia)) {
        reject(new Error('no camera here (needs the https tailnet URL)'));
        return;
      }
      Promise.all([
        loadDecoder(),
        navigator.mediaDevices.getUserMedia({ video: { facingMode: 'environment' }, audio: false }),
      ]).then(([decode, s]) => {
        stream = s;
        if (done) { stop(); return; }
        video.setAttribute('playsinline', '');
        video.muted = true;
        video.srcObject = s;
        return video.play().then(() => {
          const canvas = document.createElement('canvas');
          const ctx = canvas.getContext('2d', { willReadFrequently: true });
          const tick = () => {
            if (done) return;
            const w = video.videoWidth, h = video.videoHeight;
            if (w && h) {
              const k = Math.min(1, 640 / Math.max(w, h));
              canvas.width = Math.round(w * k);
              canvas.height = Math.round(h * k);
              ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
              const img = ctx.getImageData(0, 0, canvas.width, canvas.height);
              const hit = decode(img.data, img.width, img.height, { inversionAttempts: 'dontInvert' });
              if (hit && hit.data) { stop(); resolve(hit.data); return; }
            }
            timer = setTimeout(tick, 150);
          };
          tick();
        });
      }).catch((e) => { stop(); reject(e && e.name === 'NotAllowedError' ? new Error('camera permission denied') : e); });
    });
    return { result, stop: () => stop(new Error('cancelled')) };
  }

  const api = { parse, fromHash, scan, CODE_RE };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckPair = api;
})(typeof window !== 'undefined' ? window : globalThis);
