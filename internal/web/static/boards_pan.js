/* Board: click-drag the empty background of the lists strip to pan sideways (mouse only; touch scrolls natively).
   Only a press directly on .boards-lists itself pans (the gaps, the space below short columns), so cards, headers,
   composers and menus keep their own behaviour and DnD (boards_dnd.js) is untouched. */
(function () {
  'use strict';
  var P = null, THRESH = 4;
  function end() {
    if (!P) return;
    try { P.el.releasePointerCapture(P.id); } catch (x) { /* */ }
    P.el.classList.remove('is-dragging'); P.el.classList.remove('is-panning');
    if (P.on && window.Boards) window.Boards.dragging = false;
    P = null;
  }
  document.addEventListener('pointerdown', function (e) {
    var el = e.target;
    if (P || e.pointerType !== 'mouse' || e.button !== 0 || !el.classList || !el.classList.contains('boards-lists')) return;
    var B = window.Boards; if (B && B.dragging) return;
    if (el.scrollWidth <= el.clientWidth) return;
    // ignore presses on the native scrollbar itself
    var r = el.getBoundingClientRect();
    if (e.clientX >= r.left + el.clientWidth || e.clientY >= r.top + el.clientHeight) return;
    P = { el: el, id: e.pointerId, x: e.clientX, left: el.scrollLeft, on: false };
    try { el.setPointerCapture(e.pointerId); } catch (x) { /* */ }
  });
  document.addEventListener('pointermove', function (e) {
    if (!P || e.pointerId !== P.id) return;
    var dx = e.clientX - P.x;
    if (!P.on) {
      if (Math.abs(dx) < THRESH) return;
      P.on = true; P.el.classList.add('is-dragging', 'is-panning');
      try { window.getSelection().removeAllRanges(); } catch (x) { /* */ }
      if (window.Boards) window.Boards.dragging = true;   // defer live refresh while panning
    }
    P.el.scrollLeft = P.left - dx;
  });
  document.addEventListener('pointerup', end);
  document.addEventListener('pointercancel', end);
  document.addEventListener('lostpointercapture', end);
  window.addEventListener('blur', end);
  document.addEventListener('selectstart', function (e) { if (P) e.preventDefault(); });   // no text selection while panning
})();
