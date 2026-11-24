// Minimal client glue: toasts, modal, native drag-and-drop helpers.
document.body.addEventListener('toast', e => {
  const t = document.createElement('div'); t.className = 'toast'; t.textContent = e.detail.value || e.detail;
  document.getElementById('toasts').append(t); setTimeout(() => t.remove(), 4000);
});
// A toast for the next page (HX-Trigger "toastNext" sent with HX-Redirect): kept in sessionStorage across the
// navigation and shown once when the next page loads.
document.body.addEventListener('toastNext', e => {
  try { sessionStorage.setItem('weave-toast', String(e.detail.value || e.detail)); } catch (x) { /* storage off: no toast */ }
});
(function () {
  let msg = null;
  try { msg = sessionStorage.getItem('weave-toast'); sessionStorage.removeItem('weave-toast'); } catch (x) { return; }
  if (msg) document.body.dispatchEvent(new CustomEvent('toast', { detail: { value: msg } }));
})();
document.body.addEventListener('htmx:afterSwap', e => {
  const m = document.getElementById('modal');
  if (e.detail.target === m && !m.open) m.showModal();
});
// Theme: 'weave-theme' in localStorage = light|dark|system. Applied early by an inline <head> script;
// the toggle in the top bar updates it and saves to the server (preference values: light|dark|auto).
(function () {
  const root = document.documentElement;
  const get = () => { try { return localStorage.getItem('weave-theme') || 'system'; } catch (e) { return 'system'; } };
  function apply(mode) {
    if (mode === 'light' || mode === 'dark') root.setAttribute('data-theme', mode); else root.removeAttribute('data-theme');
    document.querySelectorAll('[data-theme-set]').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.themeSet === mode)));
  }
  function set(mode, save) {
    try { localStorage.setItem('weave-theme', mode); } catch (e) {}
    apply(mode);
    if (save && document.getElementById('user-menu')) {
      fetch('/api/v1/users/me/preferences', { method: 'PATCH', credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'weave' },
        body: JSON.stringify({ theme: mode === 'system' ? 'auto' : mode }) }).catch(() => {});
    }
  }
  apply(get());
  document.addEventListener('click', e => { const b = e.target.closest?.('[data-theme-set]'); if (b) set(b.dataset.themeSet, true); });
  // Keep the localStorage choice in sync when the settings form saves a theme.
  document.body.addEventListener('htmx:afterSwap', () => {
    const s = document.querySelector('select[name=theme]'); if (!s) return;
    const v = s.value === 'auto' ? 'system' : s.value; if (v && v !== get()) set(v, false);
  });
  // Close the account menu on outside click / Escape.
  document.addEventListener('click', e => { const m = document.getElementById('user-menu'); if (m?.open && !m.contains(e.target)) m.open = false; });
  document.addEventListener('keydown', e => { const m = document.getElementById('user-menu'); if (e.key === 'Escape' && m?.open) { m.open = false; m.querySelector('summary').focus(); } });
})();
// Sortable: any container with data-sortable="<url>" and children with data-id.
// Drag a child; on drop POSTs {id, container (data-container), position index} via htmx.ajax.
document.addEventListener('dragstart', e => { const el = e.target.closest?.('[data-id]'); if (el) { window._drag = el; el.classList.add('dragging'); } });
document.addEventListener('dragend', e => { window._drag?.classList.remove('dragging'); window._drag = null; });
document.addEventListener('dragover', e => { if (window._drag && e.target.closest?.('[data-sortable]')) e.preventDefault(); });
document.addEventListener('drop', e => {
  const box = e.target.closest?.('[data-sortable]'); const d = window._drag; if (!box || !d) return;
  e.preventDefault();
  const kids = [...box.querySelectorAll(':scope > [data-id]')].filter(k => k !== d);
  let idx = kids.findIndex(k => e.clientY < k.getBoundingClientRect().top + k.offsetHeight / 2 && (box.dataset.axis === 'x' ? e.clientX < k.getBoundingClientRect().left + k.offsetWidth / 2 : true));
  if (idx < 0) idx = kids.length;
  const prev = kids[idx - 1]?.dataset.id || '', next = kids[idx]?.dataset.id || '';
  htmx.ajax('POST', box.dataset.sortable, { target: box.dataset.target || '#main', swap: 'none',
    values: { id: d.dataset.id, container: box.dataset.container || '', index: idx, afterId: prev, beforeId: next } });
});

// Never persist page snapshots (they can contain e-mail addresses) in the browser's storage.
if (window.htmx) htmx.config.historyCacheSize = 0;

// Polling requests (hx-trigger="... every Ns") are background traffic: tell the server so they
// do not count as user activity for idle auto-logout.
document.addEventListener('htmx:configRequest', function (e) {
  var el = e.detail && e.detail.elt, t = el && el.getAttribute && el.getAttribute('hx-trigger');
  if (t && /\bevery\s/.test(t)) e.detail.headers['X-Weave-Background'] = '1';
});

// ---- Delegated behaviours (no inline handlers: the Content-Security-Policy forbids them) ----
// data-select-on-click: select the field's text.
// data-close-dialog: close the enclosing <dialog>.  data-reload: reload the page.
// data-click-trigger="evt": fire the htmx trigger `evt` on click unless the click was on a link.
// data-bool-param="name": send the checkbox state as parameter `name` with its htmx request.
document.addEventListener('click', function (e) {
  var t = e.target; if (!t || !t.closest) return;
  var el = t.closest('[data-select-on-click]'); if (el && el.select) el.select();
  if (t.closest('[data-close-dialog]')) { var d = t.closest('dialog'); if (d) d.close(); }
  if (t.closest('[data-reload]')) location.reload();
  var ct = t.closest('[data-click-trigger]');
  if (ct && !t.closest('a') && window.htmx) htmx.trigger(ct, ct.getAttribute('data-click-trigger'));
});
document.addEventListener('htmx:configRequest', function (e) {
  var el = e.detail && e.detail.elt;
  if (el && el.getAttribute && el.hasAttribute('data-bool-param')) e.detail.parameters[el.getAttribute('data-bool-param')] = el.checked;
});

// Help tips ("?" buttons, templates/_help_tip.html): hover, keyboard focus or a tap shows the tip; it is fixed-positioned
// below (or above) the button and clamped to the viewport so no container can clip it. Escape or a click elsewhere hides it.
(function () {
  function tipOf(b) { return b.querySelector('.help-tip-pop'); }
  function hide(b) { var p = tipOf(b); if (p) { p.hidden = true; } b.removeAttribute('aria-expanded'); }
  function hideAll(except) { document.querySelectorAll('[data-help-tip][aria-expanded]').forEach(function (b) { if (b !== except) hide(b); }); }
  function show(b) {
    var p = tipOf(b); if (!p) return;
    hideAll(b);
    p.hidden = false; p.style.left = '0px'; p.style.top = '0px';
    var r = b.getBoundingClientRect(), w = p.offsetWidth, h = p.offsetHeight, vw = document.documentElement.clientWidth, vh = window.innerHeight, M = 8;
    var left = Math.max(M, Math.min(r.left + r.width / 2 - w / 2, vw - w - M));
    var top = r.bottom + 6; if (top + h > vh - M) top = Math.max(M, r.top - 6 - h);
    p.style.left = left + 'px'; p.style.top = top + 'px';
    b.setAttribute('aria-expanded', 'true'); b.__shownAt = Date.now();
  }
  function btn(e) { return e.target && e.target.closest ? e.target.closest('[data-help-tip]') : null; }
  document.addEventListener('mouseover', function (e) { var b = btn(e); if (b) show(b); });
  document.addEventListener('mouseout', function (e) { var b = btn(e); if (b && !b.contains(e.relatedTarget)) hide(b); });
  document.addEventListener('focusin', function (e) { var b = btn(e); if (b) show(b); });
  document.addEventListener('focusout', function (e) { var b = btn(e); if (b) hide(b); });
  document.addEventListener('click', function (e) { var b = btn(e); if (b) { e.preventDefault(); if (!b.getAttribute('aria-expanded')) show(b); else if (Date.now() - (b.__shownAt || 0) > 400) hide(b); /* a tap fires mouseover then click: do not undo the show */ } else hideAll(null); });
  // Escape closes an open tip first (capture phase, so a dialog or popover behind it stays open)
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || !document.querySelector('[data-help-tip][aria-expanded]')) return;
    hideAll(null); e.preventDefault(); e.stopPropagation();
  }, true);
})();
