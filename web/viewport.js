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
  // Where the keyboard-up sheet must sit so its VISUAL top is the top of the
  // visible area (under the status bar) whatever frame iOS uses for position:fixed.
  // `applied` is the sheet's current inline top (px), `rectTop` its
  // getBoundingClientRect().top (layout-viewport coordinates) and `offsetTop` the
  // visual viewport's offset in the same coordinates, so rectTop - offsetTop is
  // where the sheet is actually painted. The phone showed the sheet ~one safe-top
  // too high (clock over the title, list showing under the composer, caret one
  // line below its text, phone screenshot 2026-11-02) because inline top =
  // offsetTop is only right while iOS's pan and the last visualViewport event
  // agree. Measuring and correcting removes the dependency on that.
  function sheetTop(applied, rectTop, offsetTop) {
    const a = Number(applied) || 0;
    const err = (Number(rectTop) || 0) - (Number(offsetTop) || 0);
    return Math.abs(err) < 0.5 ? a : a - err;
  }
  const api = { keyboardUp, sheetTop };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckViewport = api;
})(typeof window !== 'undefined' ? window : globalThis);
