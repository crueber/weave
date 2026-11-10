// Minimal client glue: toasts, modal, native drag-and-drop helpers.
document.body.addEventListener('toast', e => {
  const t = document.createElement('div'); t.className = 'toast'; t.textContent = e.detail.value || e.detail;
  document.getElementById('toasts').append(t); setTimeout(() => t.remove(), 4000);
});
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
// data-select-on-click: select the field's text.   data-autosubmit: submit the form when it changes.
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
document.addEventListener('change', function (e) {
  var f = e.target && e.target.closest && e.target.closest('[data-autosubmit]');
  if (f && f.form) f.form.requestSubmit();
});
document.addEventListener('htmx:configRequest', function (e) {
  var el = e.detail && e.detail.elt;
  if (el && el.getAttribute && el.hasAttribute('data-bool-param')) e.detail.parameters[el.getAttribute('data-bool-param')] = el.checked;
});
