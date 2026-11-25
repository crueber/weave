// The column sheet: configuration view of one column, shown in the shared <dialog id="modal">.
// Opened from the column header settings icon, or on load at /lists/{id}.
// History: one entry per sheet session (/lists/{id}); closing goes back; prev/next/jump replace the entry. The single
// popstate listener lives in cards_ui.js and calls ListSheet.popped() / ListSheet.open(). The card sheet's close logic never
// runs for this sheet (it keys off its own state, empty here) and none of its hooks (#card-modal, data-card-nav, #cards-saved,
// data-desc-edit, ...) appear in the column sheet's markup.
// The sheet reuses the card sheet's look (.cm-* classes) but its popovers, sections and description editor are handled here.
(function () {
  if (window.ListSheet) return;
  var st = { id: '', board: '', pushed: false, fromPop: false, gone: false, escAt: 0 };
  function modal() { return document.getElementById('modal'); }
  function sheet() { return document.getElementById('list-sheet'); }
  function toast(msg, err) { if (window.Boards && Boards.toast) Boards.toast(msg, { error: !!err }); }
  function lsGet(k) { try { return localStorage.getItem(k); } catch (x) { return null; } }
  function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (x) { /* private mode */ } }
  function q(sel) { var s = sheet(); return s && s.querySelector(sel); }

  var LS = window.ListSheet = {
    open: function (id, o) {
      o = o || {};
      if (!window.htmx) { location.href = '/lists/' + id; return; }
      st.fromPop = !!o.fromPop;
      htmx.ajax('GET', '/ui/lists/' + id, { target: '#modal', swap: 'innerHTML' });
    },
    close: function () { var m = modal(); if (m && m.open) m.close(); },
    popped: function () { st.pushed = false; }, // the browser already moved the history: do not go back again
    isOpen: function () { var m = modal(); return !!(m && m.open && sheet()); }
  };

  document.body.addEventListener('htmx:afterSwap', function (e) {
    var m = modal(); if (!m || e.detail.target !== m) return;
    var s = sheet(); if (!s) return;
    var id = s.dataset.list;
    if (!m.open) m.showModal();
    m.setAttribute('tabindex', '-1');
    if (st.fromPop) st.pushed = true;
    else if (location.pathname !== '/lists/' + id) {
      // one history entry per sheet session: further swaps (prev/next, jump) replace it
      if (st.pushed) history.replaceState({ list: id }, '', '/lists/' + id);
      else { history.pushState({ list: id }, '', '/lists/' + id); st.pushed = true; }
    }
    st.fromPop = false; st.id = id; st.board = s.dataset.board;
    applySecState();
    if (!m.contains(document.activeElement) || document.activeElement === m) m.focus({ preventScroll: true });
  });

  // closing (Esc, X, backdrop, delete): restore the URL
  function onClose() {
    var id = st.id; if (!id) return;
    var board = st.board; st.id = '';
    if (st.gone) { // deleted/archived: leave no /lists/{id} entry behind (not even as a forward entry), so rewrite it in place
      st.gone = false; st.pushed = false;
      history.replaceState(null, '', '/boards/' + board + location.search);
    } else if (st.pushed) { st.pushed = false; history.back(); }
    else if (location.pathname === '/lists/' + id && board) history.replaceState(null, '', '/boards/' + board + location.search);
  }
  if (modal()) modal().addEventListener('close', onClose);

  // ---- open from the board ----
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('[data-list-sheet]');
    if (!b || e.button !== 0) return;
    e.preventDefault();
    var menu = window.WeavePop && WeavePop.menuOf(b); // opened from a column "..." menu: close it first
    if (menu) menu.removeAttribute('open');
    LS.open(b.dataset.listSheet);
  });
  function autoOpen() {
    var mk = document.getElementById('open-list');
    if (mk && !LS.isOpen()) LS.open(mk.dataset.list);
  }
  // DOMContentLoaded (not earlier): htmx initialises on it, and deferred scripts run before it fires
  if (document.readyState !== 'complete') document.addEventListener('DOMContentLoaded', autoOpen); else autoOpen();

  // ---- popovers: <details class="cards-pop" data-pop> inside #list-sheet (one open at a time) ----
  function openPops() { var s = sheet(); return s ? [].slice.call(s.querySelectorAll('details.cards-pop[open]')) : []; }
  function closePops(except) { openPops().forEach(function (d) { if (d !== except) d.removeAttribute('open'); }); }
  function focusIn(d) {
    var f = d.querySelector('input:not([type=hidden]):not([type=checkbox]), select, textarea, button:not([disabled])');
    if (f) f.focus({ preventScroll: true });
  }
  document.addEventListener('toggle', function (e) { // 'toggle' does not bubble: capture
    var d = e.target; if (!d || d.tagName !== 'DETAILS' || !d.classList.contains('cards-pop') || !d.closest('#list-sheet')) return;
    if (!d.open) { var cs = d.querySelector('.ls-closes-confirm'); if (cs) cs.textContent = ''; return; } // a pending confirm is dropped
    closePops(d);
    if (d.dataset.pop !== 'menu' && d.dataset.pop !== 'list') focusIn(d);
  }, true);

  // ---- collapsible sections (Agent prompt starts collapsed); state is remembered ----
  function secKey(n) { return 'weave.list.sec.' + n; }
  function applySecState() {
    var s = sheet(); if (!s) return;
    s.querySelectorAll('[data-ls-sec]').forEach(function (sec) {
      var v = lsGet(secKey(sec.dataset.lsSec)), c = v === null ? sec.classList.contains('is-collapsed') : v === '1';
      setCollapsed(sec, c);
    });
  }
  function setCollapsed(sec, c) {
    sec.classList.toggle('is-collapsed', c);
    var b = sec.querySelector('[data-ls-sec-toggle]'); if (b) b.setAttribute('aria-expanded', c ? 'false' : 'true');
  }

  // ---- description: rendered view <-> editor (client side; nothing re-renders it while you type) ----
  function descForm() { return q('#ls-desc-form'); }
  function editDesc(on) {
    var f = descForm(); if (!f) return;
    var view = q('#ls-desc-view'), btn = q('[data-ls-desc-edit].cm-ghost'), ta = f.querySelector('textarea');
    f.hidden = !on; if (view) view.hidden = on;
    if (btn) btn.hidden = on || !ta.defaultValue;
    if (on) { grow(ta); ta.focus(); ta.selectionStart = ta.selectionEnd = ta.value.length; }
  }
  function grow(ta) { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight + 2, Math.round(window.innerHeight * .45)) + 'px'; }
  function cancelDesc() { var f = descForm(); if (!f) return; var ta = f.querySelector('textarea'); ta.value = ta.defaultValue; editDesc(false); var b = q('[data-ls-desc-edit]'); if (b) b.focus({ preventScroll: true }); }
  document.addEventListener('input', function (e) { if (e.target.matches && e.target.matches('[data-ls-desc-input]')) grow(e.target); });
  document.addEventListener('keydown', function (e) {
    var ta = e.target; if (!ta.matches || !ta.matches('[data-ls-desc-input]')) return;
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); ta.form.requestSubmit(); }
  });

  // ---- clicks inside the sheet ----
  document.addEventListener('click', function (e) {
    var s = sheet(), t = e.target;
    if (!s || !t.closest) return;
    var m = modal();
    if (m && m.open && t === m) { // backdrop (the dialog element itself is only hit outside its box)
      var r = m.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) { m.close(); return; }
    }
    if (!s.contains(t)) return;
    var nav = t.closest('[data-ls-nav]');
    if (nav) { if (!nav.disabled && nav.dataset.lsNav) LS.open(nav.dataset.lsNav); return; }
    var go = t.closest('[data-ls-goto]');
    if (go) { if (go.dataset.lsGoto !== s.dataset.list) LS.open(go.dataset.lsGoto); else closePops(); return; }
    var tg = t.closest('[data-ls-sec-toggle]');
    if (tg) { var sec = tg.closest('[data-ls-sec]'); var c = !sec.classList.contains('is-collapsed'); lsSet(secKey(sec.dataset.lsSec), c ? '1' : '0'); setCollapsed(sec, c); return; }
    if (t.closest('[data-ls-desc-edit]')) { editDesc(true); return; }
    var dv = t.closest('[data-ls-desc-click]');
    if (dv && !t.closest('a') && !(window.getSelection && String(window.getSelection()))) { editDesc(true); return; }
    if (t.closest('[data-ls-desc-cancel]')) { cancelDesc(); return; }
    var cur = t.closest('[data-closes-opt].is-on'); // the setting already has this value: nothing to ask
    if (cur) { closePops(); var sm0 = q('details[data-pop=closes]>summary'); if (sm0) sm0.focus({ preventScroll: true }); return; }
    var mi = t.closest('.cm-menu-panel .cm-mi'); // menu items close the menu
    if (mi) { var mn = mi.closest('details'); setTimeout(function () { if (mn) mn.removeAttribute('open'); }, 0); return; }
    if (!t.closest('details.cards-pop')) closePops();
  });

  // ---- Esc layers: popover -> description editor / title edit -> sheet ----
  function layerEsc() {
    var s = sheet(); if (!s) return false;
    var open = openPops();
    if (open.length) {
      var last = open[open.length - 1]; last.removeAttribute('open');
      var sm = last.querySelector('summary'); if (sm) sm.focus({ preventScroll: true });
      return true;
    }
    var ae = document.activeElement, f = descForm();
    if (f && !f.hidden && ae && f.contains(ae)) { cancelDesc(); return true; }
    if (ae && ae.matches && ae.matches('[data-ls-name]') && ae.value !== ae.defaultValue) { ae.value = ae.defaultValue; ae.blur(); return true; }
    return false;
  }
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || e.defaultPrevented || !LS.isOpen()) return;
    if (layerEsc()) { e.preventDefault(); e.stopPropagation(); st.escAt = Date.now(); }
  }, true);
  if (modal()) modal().addEventListener('cancel', function (e) { if (LS.isOpen() && (Date.now() - st.escAt < 80 || layerEsc())) e.preventDefault(); });
  // the title commits on Enter (the form submits natively) and on blur (change)

  // ---- keep the sheet consistent after a save ----
  function renderChips() {
    var s = sheet(); if (!s) return;
    var tot = parseInt(s.dataset.total, 10) || 0, lim = parseInt(s.dataset.wip, 10) || 0, over = lim > 0 && tot > lim;
    var c = s.querySelector('[data-ls-count]'); if (c) c.textContent = tot + (tot === 1 ? ' card' : ' cards');
    var cv = s.querySelector('[data-ls-count-val]'); if (cv) cv.textContent = tot;
    var w = s.querySelector('[data-ls-wip]'); if (!w) return;
    w.hidden = !lim; w.classList.toggle('is-over', over); w.classList.toggle('cards-badge-warn', over);
    var flag = w.querySelector('.ls-flag');
    if (over && !flag) { flag = document.createElement('b'); flag.className = 'ls-flag'; flag.setAttribute('aria-hidden', 'true'); flag.textContent = '!'; w.insertBefore(flag, w.firstChild); }
    else if (!over && flag) flag.remove();
    var tx = w.querySelector('[data-ls-wip-text]'); if (tx) tx.textContent = lim ? 'WIP ' + tot + '/' + lim + (over ? ', over the limit' : '') : '';
    var val = s.querySelector('[data-ls-wip-val]'), emp = s.querySelector('[data-ls-wip-empty]'), clr = s.querySelector('[data-ls-wip-clear]'), inp = s.querySelector('[data-ls-wip-input]');
    if (val) { val.hidden = !lim; val.textContent = lim ? 'Max ' + lim : ''; }
    if (emp) emp.hidden = !!lim;
    if (clr) clr.hidden = !lim;
    if (inp) inp.value = lim ? lim : '';
  }
  function renderKeep() {
    var s = sheet(); if (!s) return;
    var keep = parseInt(s.dataset.keep, 10) || 0, chip = s.querySelector('[data-ls-keep]');
    var ico = chip && chip.querySelector('svg');
    if (chip) { chip.hidden = !keep; var t = chip.querySelector('[data-ls-keep-text]'); if (t) t.textContent = keep ? 'Keeps newest ' + keep : ''; }
    var val = s.querySelector('[data-ls-keep-val]');
    if (val) { val.textContent = ''; if (keep && ico) val.appendChild(ico.cloneNode(true)); val.appendChild(document.createTextNode(keep ? 'Keep ' + keep : 'Off')); }
    var inp = s.querySelector('[data-ls-keep-input]'); if (inp) inp.value = keep ? keep : '';
    var clr = s.querySelector('[data-ls-keep-clear]'); if (clr) clr.hidden = !keep;
  }
  function flash(msg) {
    var el = q('[data-ls-saved]'); if (!el) return;
    el.textContent = ''; setTimeout(function () { el.textContent = msg || 'Saved'; }, 20);
  }
  function refreshActivity() {
    var box = q('[data-ls-activity]'); if (!box || !window.htmx) return;
    var id = sheet().dataset.list;
    htmx.ajax('GET', '/ui/lists/' + id + '/activity', { target: box, swap: 'innerHTML' });
  }
  function setName(name) {
    var s = sheet();
    s.querySelectorAll('[data-ls-crumb]').forEach(function (n) { n.textContent = name; });
    s.setAttribute('aria-label', 'Column: ' + name);
    var sum = s.querySelector('.cm-list>summary'); if (sum) sum.setAttribute('aria-label', 'Column: ' + name + '. Go to another column');
    var opt = s.querySelector('[data-ls-goto="' + s.dataset.list + '"]');
    if (opt) opt.lastChild.textContent = name;
  }
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var el = e.detail.elt, s = sheet();
    if (!el || !s || !s.contains(el) || !e.detail.successful) return;
    if (el.matches('[data-ls-delete]') || el.matches('[data-ls-archive]')) {
      st.gone = true; LS.close(); toast(el.matches('[data-ls-delete]') ? 'List deleted' : 'List archived'); return;
    }
    var kind = el.dataset && el.dataset.lsForm; if (!kind) return;
    if (kind === 'name') {
      var inp0 = el.querySelector('[data-ls-name]'), name = inp0.value.trim();
      if (name) { inp0.defaultValue = name; setName(name); }
    } else if (kind === 'desc') {
      var ta = el.querySelector('textarea');
      ta.defaultValue = ta.value;
      editDesc(false);
      htmx.ajax('GET', '/ui/lists/' + s.dataset.list + '/description', { target: '#ls-desc-view', swap: 'innerHTML' }).then(function () {
        var b = q('[data-ls-desc-edit].cm-ghost'); if (b) b.hidden = !ta.defaultValue;
      });
    } else if (kind === 'type') {
      // the closing setting ("Close cards that land here"): type closed = on, active = off
      var on = el.dataset.type === 'closed';
      var chip = s.querySelector('[data-ls-type]'); if (chip) chip.hidden = !on;
      var val = s.querySelector('[data-ls-type-val]'), ico = chip && chip.querySelector('svg');
      if (val) { val.textContent = ''; if (on && ico) val.appendChild(ico.cloneNode(true)); val.appendChild(document.createTextNode(on ? 'On' : 'Off')); }
      s.querySelectorAll('[data-closes-opt]').forEach(function (b) {
        var sel = (b.dataset.closesOpt === 'closed') === on;
        b.classList.toggle('is-on', sel); b.setAttribute('aria-checked', sel ? 'true' : 'false'); b.querySelector('.cm-tick').textContent = sel ? '✓' : '';
      });
      var slot = q('#ls-closes-confirm'); if (slot) slot.textContent = '';
      closePops();
      var sm = q('details[data-pop=closes]>summary'); if (sm) sm.focus({ preventScroll: true });
      // the switch may have closed or re-opened the column's cards: redraw the board behind the sheet (check marks)
      if (window.Boards && window.Boards.refresh) window.Boards.refresh(true);
    } else if (kind === 'wip') {
      var wi = s.querySelector('[data-ls-wip-input]'), n = el.hasAttribute('data-ls-wip-clear') ? 0 : parseInt(wi.value, 10);
      s.dataset.wip = n > 0 ? n : 0; renderChips();
      closePops();
    } else if (kind === 'archive') {
      // auto-archive ("Keep the newest N cards"): data-keep is the new setting (0 = off), data-archived what it archived
      var keep = parseInt(el.dataset.keep, 10) || 0, gone = parseInt(el.dataset.archived, 10) || 0;
      s.dataset.keep = keep;
      if (gone) s.dataset.total = Math.max(0, (parseInt(s.dataset.total, 10) || 0) - gone);
      renderChips(); renderKeep();
      var slot2 = q('#ls-archive-confirm'); if (slot2) slot2.textContent = '';
      closePops();
      var sm2 = q('details[data-pop=archive]>summary'); if (sm2) sm2.focus({ preventScroll: true });
      if (window.Boards && window.Boards.refresh) window.Boards.refresh(true);
      flash(gone ? 'Archived ' + gone + (gone === 1 ? ' card' : ' cards') : 'Saved');
      refreshActivity();
      return;
    } else if (kind === 'move') {
      s.dataset.total = 0; renderChips();
    }
    flash('Saved');
    refreshActivity();
  });

  // ---- the closing-setting confirm (column sheet popover and column "..." menu; boards_list_closes_confirm) ----
  // Cancel drops it and returns focus to the control that asked; a fresh confirm focuses its default action.
  document.addEventListener('click', function (e) {
    var c = e.target.closest && e.target.closest('[data-closes-cancel]'); if (!c) return;
    var slot = c.closest('.ls-closes-confirm'); if (!slot) return;
    var back = slot.id === 'ls-closes-confirm' ? slot.parentNode.querySelector('[data-closes-opt].is-on')
      : slot.id === 'ls-archive-confirm' ? slot.parentNode.querySelector('[data-ls-keep-input]') : slot.previousElementSibling;
    slot.textContent = '';
    if (back && back.tagName === 'FORM') back = back.querySelector('input:not([type=hidden])');
    if (back) back.focus({ preventScroll: true });
  });
  document.body.addEventListener('htmx:afterSwap', function (e) {
    var t = e.detail.target; if (!t || !t.matches || !t.matches('.ls-closes-confirm')) return;
    var f = t.querySelector('[data-closes-default]'); if (f) f.focus({ preventScroll: true });
    var cf = t.querySelector('.closes-confirm'); if (cf && cf.scrollIntoView) cf.scrollIntoView({ block: 'nearest' });
  });
})();
