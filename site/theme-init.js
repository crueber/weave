// Runs synchronously in <head> (before first paint): apply the saved theme and flag JS support.
// Same pattern as the app's internal/web/static/theme-init.js; a static file so the CSP can forbid inline scripts.
(function () {
  var root = document.documentElement;
  root.classList.add('js');
  try {
    var t = localStorage.getItem('weave-site-theme');
    if (t === 'light' || t === 'dark') root.setAttribute('data-theme', t);
  } catch (e) { /* storage blocked */ }
})();
