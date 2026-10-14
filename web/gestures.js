/* deck-remote — swipe-to-dismiss for the session sheet.
 *
 * Loaded before app.js (window.DeckGestures). Self-contained: the sheet
 * element and a dismiss callback in, a teardown function out.
 */
(function (root) {
  'use strict';

  // ----- swipe-to-dismiss gestures -----
  // Two iOS-standard gestures dismiss the sheet, complementing the ‹ back button:
  //   1) swipe DOWN starting on the header/grab-handle region (#sheetHandle) —
  //      bound to the handle, NOT the convo, because the convo's top edge is used
  //      for scroll-up history paging (a down-drag there must keep scrolling, not
  //      close). The two gestures can't collide: the handle never scrolls.
  //   2) swipe RIGHT from the very left screen edge (touchstart clientX < ~30px).
  // A cheap translate follows the finger; release past ~60px closes, else snaps
  // back. Listeners live on `sheet` and are torn down in closeSheet().
  const SWIPE_DISMISS = 60;   // px past which a release dismisses
  const EDGE_ZONE = 30;       // px from the left edge that arms edge-back
  const SWIPE_SLOP = 10;      // px before we commit to a drag axis

  // Returns a teardown that removes the listeners.
  function wireSwipeDismiss(sheet, onDismiss) {
    let mode = null;          // null | 'down' | 'edge' | 'reject'
    let startX = 0, startY = 0, dx = 0, dy = 0;

    function onStart(e) {
      if (e.touches.length !== 1) { mode = 'reject'; return; }
      const t = e.touches[0];
      startX = t.clientX; startY = t.clientY; dx = 0; dy = 0;
      // Edge-back arms anywhere on the sheet when the touch begins at the left
      // screen edge. The down-swipe arms only on the header/handle region.
      const onHandle = !!(e.target.closest && e.target.closest('#sheetHandle'));
      if (startX < EDGE_ZONE) mode = 'edge';
      else if (onHandle) mode = 'down';
      else mode = null; // undecided; resolve direction on first move
    }

    function onMove(e) {
      if (mode === 'reject') return;
      const t = e.touches[0];
      dx = t.clientX - startX;
      dy = t.clientY - startY;
      if (mode === null) {
        // Only a clearly-downward drag that STARTED on the handle could be here
        // (non-handle starts away from the edge are left to normal scrolling).
        const onHandle = !!(e.target.closest && e.target.closest('#sheetHandle'));
        if (Math.abs(dy) < SWIPE_SLOP && Math.abs(dx) < SWIPE_SLOP) return;
        if (onHandle && dy > Math.abs(dx)) mode = 'down';
        else { mode = 'reject'; return; }
      }
      if (mode === 'down') {
        if (dy <= 0) { setSheetDrag(sheet, 0); return; } // ignore upward
        e.preventDefault();
        setSheetDrag(sheet, dy);
      } else if (mode === 'edge') {
        // Commit to horizontal only; a mostly-vertical move releases the gesture.
        if (Math.abs(dy) > Math.abs(dx) && Math.abs(dy) > SWIPE_SLOP) { mode = 'reject'; clearSheetDrag(sheet); return; }
        if (dx <= 0) { setSheetDrag(sheet, 0); return; }
        e.preventDefault();
        setSheetDrag(sheet, 0, dx);
      }
    }

    function onEnd() {
      if (mode === 'down' && dy > SWIPE_DISMISS) { onDismiss(); return; }
      if (mode === 'edge' && dx > SWIPE_DISMISS) { onDismiss(); return; }
      // Not past threshold: snap back to rest.
      if (mode === 'down' || mode === 'edge') snapSheetBack(sheet);
      mode = null;
    }

    sheet.addEventListener('touchstart', onStart, { passive: true });
    sheet.addEventListener('touchmove', onMove, { passive: false });
    sheet.addEventListener('touchend', onEnd, { passive: true });
    sheet.addEventListener('touchcancel', onEnd, { passive: true });
    return function teardown() {
      sheet.removeEventListener('touchstart', onStart);
      sheet.removeEventListener('touchmove', onMove);
      sheet.removeEventListener('touchend', onEnd);
      sheet.removeEventListener('touchcancel', onEnd);
    };
  }

  function setSheetDrag(sheet, ty, tx) {
    sheet.classList.add('dragging');
    sheet.classList.remove('snapback');
    sheet.style.transform = 'translate(' + (tx || 0) + 'px,' + (ty || 0) + 'px)';
  }
  function clearSheetDrag(sheet) {
    sheet.classList.remove('dragging');
    sheet.style.transform = '';
  }
  function snapSheetBack(sheet) {
    sheet.classList.remove('dragging');
    sheet.classList.add('snapback');
    sheet.style.transform = '';
    setTimeout(() => sheet.classList.remove('snapback'), 220);
  }

  root.DeckGestures = { wireSwipeDismiss };
})(window);
