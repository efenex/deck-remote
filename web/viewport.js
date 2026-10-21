// On-screen keyboard detection. Pure so it is testable (node --test web/).
(function (root) {
  'use strict';
  // True when the visual viewport has been shrunk by the keyboard. The baseline
  // is the LARGER of innerHeight and the document's clientHeight: in an iOS
  // home-screen app innerHeight shrinks with the keyboard too (device overlay
  // 13:36Z: innerH 447, clientH 793, vv 449), so innerHeight alone never saw a
  // keyboard. A pinch-zoomed page (scale > 1) also has a short visual viewport
  // and is not a keyboard.
  function keyboardUp(vv, innerHeight, clientHeight) {
    if (!vv) return false;
    if (vv.scale && vv.scale > 1.01) return false;
    return vv.height < Math.max(innerHeight || 0, clientHeight || 0) - 120;
  }
  const api = { keyboardUp };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckViewport = api;
})(typeof window !== 'undefined' ? window : globalThis);
