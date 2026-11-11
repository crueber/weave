// Board settings: section navigation (deep-linkable), member combobox, copy link,
// delete confirmation and label drag handles. All handlers are delegated so they
// survive htmx section swaps.
(function () {
  if (window._bsInit) return; window._bsInit = true;
  const root = () => document.querySelector('.boards-settings');
  const ids = r => [...r.querySelectorAll('.bs-nav a[data-section]')].map(a => a.dataset.section);
  const absLinks = () => root()?.querySelectorAll('[data-link]').forEach(i => { if (i.value.startsWith('/')) i.value = location.origin + i.value; });

  function show(id, push) {
    const r = root(); if (!r) return;
    if (!ids(r).includes(id)) id = ids(r)[0];
    r.dataset.active = id;
    r.querySelectorAll('.bs-nav a').forEach(a => a.dataset.section === id ? a.setAttribute('aria-current', 'true') : a.removeAttribute('aria-current'));
    if (push) { const u = new URL(location.href); u.searchParams.set('section', id); u.hash = id; history.replaceState(null, '', u); }
  }
  function fromURL() {
    const q = new URLSearchParams(location.search).get('section');
    return q || location.hash.slice(1) || (root() && root().dataset.active) || 'general';
  }
  function init() {
    if (!root()) return;
    document.documentElement.classList.add('js-ready');
    show(fromURL(), false); absLinks();
  }
  init();
  window.addEventListener('hashchange', () => show(fromURL(), false));
  document.addEventListener('htmx:afterSettle', () => {
    absLinks();
    root()?.querySelectorAll('.bs-saved').forEach(m => setTimeout(() => m.remove(), 2500));
  });

  document.addEventListener('click', e => {
    const a = e.target.closest?.('.bs-nav a[data-section]');
    if (a) { e.preventDefault(); show(a.dataset.section, true); return; }
    const cp = e.target.closest?.('[data-copy]');
    if (cp) {
      const input = document.getElementById(cp.dataset.copy); input.select();
      const done = () => { const t = cp.dataset.label || (cp.dataset.label = cp.textContent); cp.textContent = 'Copied!'; setTimeout(() => cp.textContent = t, 1800); };
      (navigator.clipboard ? navigator.clipboard.writeText(input.value) : Promise.reject()).then(done, () => { try { document.execCommand('copy'); done(); } catch (_) {} });
      return;
    }
    document.querySelectorAll('.bs-pop[open]').forEach(d => { if (!d.contains(e.target)) d.open = false; });
    document.querySelectorAll('[data-combo]').forEach(c => { if (!c.contains(e.target)) closeCombo(c); });
  });
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape') document.querySelectorAll('.bs-pop[open]').forEach(d => { d.open = false; d.querySelector('summary').focus(); });
  });

  // delete confirmation: type the board name
  document.addEventListener('input', e => {
    const i = e.target.closest?.('[data-del-in]'); if (!i) return;
    const box = i.closest('.bs-delete'); box.querySelector('[data-del]').disabled = i.value.trim() !== box.dataset.name;
  });

  // label drag handle: only the grip makes the row draggable (keeps inputs selectable)
  document.addEventListener('pointerdown', e => { const g = e.target.closest?.('.bs-grip'); if (g) g.closest('.bs-label').draggable = true; });
  const undrag = () => document.querySelectorAll('.bs-label[draggable]').forEach(l => { if (!l.classList.contains('dragging')) l.draggable = false; });
  document.addEventListener('dragend', () => document.querySelectorAll('.bs-label[draggable]').forEach(l => l.draggable = false));
  document.addEventListener('pointerup', () => setTimeout(undrag, 0));

  // ---- member combobox ----
  const opts = c => [...c.querySelectorAll('li[role=option]')];
  function closeCombo(c) { c.querySelector('ul').hidden = true; c.querySelector('[role=combobox]').setAttribute('aria-expanded', 'false'); }
  function filter(c) {
    const q = c.querySelector('[role=combobox]').value.trim().toLowerCase(); let n = 0, first = null;
    opts(c).forEach(o => { const ok = !q || o.textContent.toLowerCase().includes(q); o.hidden = !ok; o.removeAttribute('aria-selected'); if (ok) { n++; first = first || o; } });
    c.querySelector('.bs-empty').hidden = n > 0;
    c.querySelector('ul').hidden = false; c.querySelector('[role=combobox]').setAttribute('aria-expanded', 'true');
    return first;
  }
  function pick(c, o) {
    const f = c.closest('form');
    c.querySelector('[name=userId]').value = o.dataset.user || ''; c.querySelector('[name=groupId]').value = o.dataset.group || '';
    c.querySelector('[role=combobox]').value = o.dataset.label; closeCombo(c); f.querySelector('[data-add]').disabled = false;
    f.querySelector('select[name=role]').focus();
  }
  document.addEventListener('input', e => {
    const inp = e.target.closest?.('[data-combo] [role=combobox]'); if (!inp) return;
    const c = inp.closest('[data-combo]');
    c.querySelector('[name=userId]').value = ''; c.querySelector('[name=groupId]').value = ''; c.closest('form').querySelector('[data-add]').disabled = true;
    const f = filter(c); if (f) f.setAttribute('aria-selected', 'true');
  });
  document.addEventListener('focusin', e => {
    const inp = e.target.closest?.('[data-combo] [role=combobox]'); if (!inp) return;
    const c = inp.closest('[data-combo]');
    if (!c.querySelector('[name=userId]').value && !c.querySelector('[name=groupId]').value) filter(c);
  });
  document.addEventListener('keydown', e => {
    const inp = e.target.closest?.('[data-combo] [role=combobox]'); if (!inp) return;
    const c = inp.closest('[data-combo]'); const vis = opts(c).filter(o => !o.hidden); const cur = vis.findIndex(o => o.getAttribute('aria-selected') === 'true');
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault(); if (c.querySelector('ul').hidden) filter(c);
      const n = e.key === 'ArrowDown' ? Math.min(vis.length - 1, cur + 1) : Math.max(0, cur - 1);
      vis.forEach(o => o.removeAttribute('aria-selected')); if (vis[n]) { vis[n].setAttribute('aria-selected', 'true'); vis[n].scrollIntoView({ block: 'nearest' }); }
    } else if (e.key === 'Enter' && !c.querySelector('ul').hidden) { e.preventDefault(); const t = vis[cur] || vis[0]; if (t) pick(c, t); }
    else if (e.key === 'Escape') closeCombo(c);
  });
  document.addEventListener('mousedown', e => { const o = e.target.closest?.('[data-combo] li[role=option]'); if (o) { e.preventDefault(); pick(o.closest('[data-combo]'), o); } });
})();
