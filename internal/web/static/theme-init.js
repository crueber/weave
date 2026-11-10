// Runs synchronously in <head> (before first paint): apply the saved theme and flag JS support.
// Kept as a static file (not inline) so the Content-Security-Policy can forbid inline scripts.
(function () {
  var root = document.documentElement;
  root.classList.add('js');
  try {
    var t = localStorage.getItem('weave-theme');
    if (t === 'light' || t === 'dark') root.setAttribute('data-theme', t);
  } catch (e) { /* storage blocked */ }
})();
