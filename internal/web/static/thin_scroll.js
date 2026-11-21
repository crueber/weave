// Marks a scroll container with .is-scrolling while it moves (and for a moment after) so CSS can widen its slim
// scrollbar only then. Scroll events do not bubble, so one capturing listener on the document covers every container.
(function () {
  'use strict';
  var timers = new WeakMap();
  document.addEventListener('scroll', function (e) {
    var el = e.target;
    if (!el || el === document || !el.classList) return;
    el.classList.add('is-scrolling');
    clearTimeout(timers.get(el));
    timers.set(el, setTimeout(function () { el.classList.remove('is-scrolling'); }, 900));
  }, true);
})();
