// The whole site script: the theme switch and the screenshot tabs. No network, no storage beyond the theme choice.
(function () {
  var root = document.documentElement;

  // Theme: light | system | dark, saved per browser (theme-init.js applies it before first paint).
  function current() { try { return localStorage.getItem('weave-site-theme') || 'system'; } catch (e) { return 'system'; } }
  function apply(mode) {
    if (mode === 'light' || mode === 'dark') root.setAttribute('data-theme', mode); else root.removeAttribute('data-theme');
    document.querySelectorAll('[data-theme-set]').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.themeSet === mode)); });
  }
  apply(current());
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('[data-theme-set]');
    if (!b) return;
    try { localStorage.setItem('weave-site-theme', b.dataset.themeSet); } catch (err) { /* storage blocked */ }
    apply(b.dataset.themeSet);
    var dark = root.getAttribute('data-theme') === 'dark' ||
      (!root.hasAttribute('data-theme') && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    var t = document.querySelector('[role="tab"][data-tab="' + (dark ? 'dark' : 'light') + '"]');
    if (t) t.click(); // the Screens gallery follows the chosen theme
  });

  // Screenshot tabs (WAI-ARIA tabs: click, arrow keys, Home/End). Without JS both panels simply show.
  document.querySelectorAll('[role="tablist"]').forEach(function (list) {
    var tabs = Array.prototype.slice.call(list.querySelectorAll('[role="tab"]'));
    function select(tab, focus) {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.setAttribute('aria-selected', String(on));
        t.tabIndex = on ? 0 : -1;
        document.getElementById(t.getAttribute('aria-controls')).hidden = !on;
      });
      if (focus) tab.focus();
    }
    var dark = root.getAttribute('data-theme') === 'dark' ||
      (!root.hasAttribute('data-theme') && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    select(tabs.filter(function (t) { return t.dataset.tab === (dark ? 'dark' : 'light'); })[0] || tabs[0], false);
    list.addEventListener('click', function (e) { var t = e.target.closest('[role="tab"]'); if (t) select(t, false); });
    list.addEventListener('keydown', function (e) {
      var i = tabs.indexOf(document.activeElement), n = null;
      if (i < 0) return;
      if (e.key === 'ArrowRight') n = tabs[(i + 1) % tabs.length];
      else if (e.key === 'ArrowLeft') n = tabs[(i - 1 + tabs.length) % tabs.length];
      else if (e.key === 'Home') n = tabs[0];
      else if (e.key === 'End') n = tabs[tabs.length - 1];
      if (n) { e.preventDefault(); select(n, true); }
    });
  });

  // The demo video plays only while visible, and never when the visitor prefers reduced motion (poster only).
  var v = document.querySelector('video[data-autoplay]');
  var still = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)');
  if (v && 'IntersectionObserver' in window) {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (still && still.matches) return;
        if (en.isIntersecting) { v.preload = 'auto'; var p = v.play(); if (p && p.catch) p.catch(function () {}); } else v.pause();
      });
    }, {threshold: 0.35});
    io.observe(v);
    if (still) still.addEventListener('change', function () { if (still.matches) v.pause(); });
  }
})();
