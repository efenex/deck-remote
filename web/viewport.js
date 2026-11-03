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
  // Top of the visible area in getBoundingClientRect() coordinates. getBoundingClientRect
  // reports where something is PAINTED (screen frame), while vv.offsetTop is where the
  // visual viewport sits in the LAYOUT frame, and iOS can shift the whole document
  // (html top -344, device overlay 2026-11-02 23:02Z: vv off 344, innerH 449, clientH 793,
  // html/body/#app top -344 bottom 449). pageTop is the visual viewport in document
  // coordinates and html's rect top says where the document is painted, so their sum is
  // the visible top in rect coordinates: 344 + -344 = 0 there; plain Safari pan (document
  // unshifted) gives offsetTop.
  function visibleTop(vv, htmlTop) {
    if (!vv) return 0;
    const page = Number.isFinite(vv.pageTop) ? vv.pageTop : (Number(vv.offsetTop) || 0);
    return page + (Number(htmlTop) || 0);
  }
  // Where the keyboard-up sheet's inline top must be so it is PAINTED at the top of the
  // visible area. `applied` is its current inline top, `rectTop` its measured painted top
  // and `visTop` visibleTop(). Round 1 compared rectTop with vv.offsetTop (two different
  // frames): with the document shifted -344 and top 344 already painting at 0, it "fixed" a
  // phantom 344 error, set top 688 (= 2 x offsetTop) and painted the sheet 344 too low,
  // header above the accessory bar and the composer under the keyboard (phone 2026-11-02).
  function sheetTop(applied, rectTop, visTop) {
    const a = Number(applied) || 0;
    const err = (Number(rectTop) || 0) - (Number(visTop) || 0);
    return Math.abs(err) < 0.5 ? a : Math.max(0, a - err);
  }
  const api = { keyboardUp, visibleTop, sheetTop };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckViewport = api;
})(typeof window !== 'undefined' ? window : globalThis);
