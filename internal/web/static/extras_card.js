// Live stopwatch display for the card time-tracking fragment.
// Only runs a 1s interval while a running .x-clock[data-started] is in the DOM; skips ticks while hidden.
(function () {
  if (window._xClockInit) return;
  window._xClockInit = true;
  var timer = null;
  function tick() {
    if (document.hidden) return;
    var clocks = document.querySelectorAll('.x-clock[data-started]');
    if (!clocks.length) { clearInterval(timer); timer = null; return; }
    clocks.forEach(function (el) {
      var t = Date.parse(el.dataset.started);
      if (isNaN(t)) return;
      var s = Math.max(0, Math.floor((Date.now() - t) / 1000)) + (parseInt(el.dataset.base, 10) || 0);
      var h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), sec = s % 60, p = function (n) { return String(n).padStart(2, '0'); };
      el.textContent = h > 0 ? h + 'h ' + p(m) + 'm ' + p(sec) + 's' : m > 0 ? m + 'm ' + p(sec) + 's' : sec + 's';
    });
  }
  function sync() {
    var has = !!document.querySelector('.x-clock[data-started]');
    if (has && !timer) { tick(); timer = setInterval(tick, 1000); }
    else if (!has && timer) { clearInterval(timer); timer = null; }
  }
  document.addEventListener('htmx:afterSwap', sync);
  document.addEventListener('close', sync, true);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) sync(); });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', sync);
  else sync();
})();
