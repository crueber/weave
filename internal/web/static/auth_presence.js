// Presence heartbeat: reports online/idle/away to the server every 60s while the tab is open.
(function () {
  if (window.__weavePresence) return; window.__weavePresence = true;
  var last = Date.now(), state = '';
  ['mousemove', 'keydown', 'pointerdown', 'scroll', 'touchstart'].forEach(function (ev) {
    addEventListener(ev, function () { last = Date.now(); }, { passive: true });
  });
  function current() { return document.hidden ? 'away' : (Date.now() - last > 5 * 60 * 1000 ? 'idle' : 'online'); }
  function beat() {
    state = current();
    if (window.htmx) htmx.ajax('POST', '/ui/presence', { values: { state: state }, swap: 'none', headers: { 'X-Weave-Background': '1' } });
  }
  document.addEventListener('visibilitychange', beat);
  addEventListener('pagehide', function () { navigator.sendBeacon && navigator.sendBeacon('/ui/presence', new URLSearchParams({ state: 'offline' })); });
  setInterval(beat, 60000); beat();
})();
