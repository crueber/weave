/* Floating popover positioning for the board page: list "..." menus, filter-bar dropdowns and the card quick-edit
   popover. Popovers are position:fixed (so column / scroller overflow can never clip them); JS places them from the
   anchor's rect, flips above / to the left when there is no room and caps max-height so the content scrolls instead of
   running off screen. On phones (<=640px) CSS turns the list menu into a bottom sheet and JS leaves it alone.
   PORTAL: while open, a list menu's popover is moved out of its column into #popover-root (a direct child of <body>), so
   no column, scroller, stacking context or tile can ever draw over it. It goes back into its <details> on close.
   The registry window.WeavePop.open maps list id -> {details, pop}; the live-merge refresh (boards_board.js) uses it to
   defer a column whose menu is open, and sweeps orphans (menu whose column was removed). The quick-edit popover
   (#boards-pop) is moved to <body> once. Filter dropdowns stay in their form (they live outside columns, and their
   checkboxes must remain form controls) and are only fixed-positioned.
   Also: roving arrow-key navigation inside the list menu, one expander open at a time, tab wrap-around. */
(function () {
  if (window.WeavePlace) return;
  var phone = window.matchMedia('(max-width:640px)');
  var M = 8, GAP = 4;

  /* place(pop, anchor, opts): anchor is an element or a {left,right,top,bottom} rect.
     opts.align 'right' (default: pop's right edge on the anchor's right) | 'left'
     opts.cap   max pixel height (default none)
     opts.cover true = top aligned with the anchor top (quick edit) instead of below/above it */
  function place(pop, anchor, opts) {
    opts = opts || {};
    if (opts.sheet !== false && phone.matches && pop.dataset.sheet) { pop.style.cssText = ''; return; }
    var r = anchor.getBoundingClientRect ? anchor.getBoundingClientRect() : anchor;
    var vw = document.documentElement.clientWidth, vh = window.innerHeight;
    pop.style.position = 'fixed'; pop.style.right = 'auto'; pop.style.bottom = 'auto';
    pop.style.left = '0px'; pop.style.top = '0px'; pop.style.maxHeight = opts.cap ? opts.cap + 'px' : 'none';
    var w = pop.offsetWidth, h = pop.offsetHeight;
    var left = opts.align === 'left' ? r.left : r.right - w;
    left = Math.max(M, Math.min(left, vw - w - M));
    var top, mh;
    if (opts.cover) {
      mh = vh - 2 * M; top = Math.max(M, Math.min(r.top, vh - M - Math.min(h, mh)));
    } else {
      var below = vh - r.bottom - GAP - M, above = r.top - GAP - M;
      if (h <= below || (below >= above && h > above)) { top = r.bottom + GAP; mh = below; }
      else { mh = above; top = Math.max(M, r.top - GAP - Math.min(h, above)); }
    }
    mh = Math.max(120, opts.cap ? Math.min(mh, opts.cap) : mh);
    pop.style.left = left + 'px'; pop.style.top = top + 'px'; pop.style.maxHeight = mh + 'px';
  }
  window.WeavePlace = place;

  // ---- portal registry ----
  var POP = window.WeavePop = { open: {} };
  function root() {
    var r = document.getElementById('popover-root');
    if (!r) { r = document.createElement('div'); r.id = 'popover-root'; r.innerHTML = '<div class="boards-scrim" hidden></div>'; document.body.appendChild(r); }
    return r;
  }
  function listIdOf(d) { var c = d.closest && d.closest('.boards-col'); return (c && c.dataset.id) || d.dataset.listId || ''; }
  function portal(d) {
    var pop = d._pop || d.querySelector(':scope > .boards-menu-pop'); if (!pop) return null;
    var r = root(), id = listIdOf(d);
    pop._home = d; d._pop = pop; pop.dataset.list = id;
    if (pop.parentNode !== r) r.appendChild(pop);
    POP.open[id || ('d' + (d._pid = d._pid || Math.random()))] = { details: d, pop: pop };
    r.firstChild.hidden = false;
    return pop;
  }
  function restore(d) {
    var pop = d._pop; if (!pop) return;
    pop.style.cssText = '';
    if (pop.parentNode !== d && d.isConnected) d.appendChild(pop); else if (!d.isConnected) pop.remove();
    Object.keys(POP.open).forEach(function (k) { if (POP.open[k].details === d) delete POP.open[k]; });
    var r = document.getElementById('popover-root');
    if (r && !Object.keys(POP.open).length) r.firstChild.hidden = true;
  }
  // drop any popover whose column vanished (list deleted remotely, view switched); call after every DOM merge
  POP.sweep = function () {
    Object.keys(POP.open).forEach(function (k) {
      var e = POP.open[k];
      if (!e.details.isConnected || !e.details.open) { e.details.removeAttribute('open'); restore(e.details); if (!e.details.isConnected) e.pop.remove(); delete POP.open[k]; }
    });
    var r = document.getElementById('popover-root');
    if (r) { [].forEach.call(r.querySelectorAll('.boards-menu-pop'), function (p) { if (!p._home || !p._home.isConnected) p.remove(); }); if (!Object.keys(POP.open).length) r.firstChild.hidden = true; }
  };
  POP.isOpen = function (listId) { var e = POP.open[listId]; return !!(e && e.details.isConnected && e.details.open); };
  // the <details> that owns an element inside a (possibly portaled) list menu
  POP.menuOf = function (el) {
    if (!el || !el.closest) return null;
    var d = el.closest('details.boards-menu'); if (d) return d;
    var p = el.closest('.boards-menu-pop'); return (p && p._home) || null;
  };
  POP.colOf = function (el) {
    if (!el || !el.closest) return null;
    var c = el.closest('.boards-col'); if (c) return c;
    var d = POP.menuOf(el); return d ? d.closest('.boards-col') : null;
  };

  function popOf(d) { return d._pop || d.querySelector('.boards-menu-pop, .boards-dd-pop'); }
  function placeDetails(d) {
    var p = popOf(d), s = d.querySelector('summary'); if (!p || !s) return;
    if (p.classList.contains('boards-menu-pop')) place(p, s, { align: 'right' });
    else place(p, s, { align: 'left', cap: 320 });
  }
  function reset(d) { if (d._pop) restore(d); else { var p = popOf(d); if (p) p.style.cssText = ''; } }
  function openOnes() { return document.querySelectorAll('details.boards-menu[open], details.boards-dd[open]'); }

  function items(menu) {
    return [].slice.call(menu.querySelectorAll('[role=menuitem]')).filter(function (el) {
      if (el.disabled || !el.offsetParent) return false;
      var sub = el.closest('details.boards-sub'); // controls inside a closed expander are not items
      return !(sub && !sub.open && el.tagName !== 'SUMMARY');
    });
  }

  // toggle does not bubble: capture it.
  document.addEventListener('toggle', function (e) { onToggle(e.target); }, true);
  // belt and braces for engines that do not fire toggle reliably: re-run after a summary click
  document.addEventListener('click', function (e) {
    var s = e.target.closest && e.target.closest('details.boards-menu > summary, details.boards-dd > summary, details.boards-sub > summary');
    if (s) setTimeout(function () { onToggle(s.parentNode); }, 0);
  });
  window.WeaveMenuToggled = function (d) { onToggle(d); };
  function onToggle(d) {
    if (!d || !d.matches) return;
    if (d.matches('details.boards-menu, details.boards-dd')) {
      if (d.open) {
        openOnes().forEach(function (o) { if (o !== d) o.removeAttribute('open'); });
        if (d.matches('.boards-menu')) { if (!portal(d)) return; }   // move BEFORE focusing: moving a node drops focus
        placeDetails(d);
        if (d.matches('.boards-menu')) { var f = items(popOf(d))[0]; f && f.focus({ preventScroll: true }); }
      } else reset(d);
    } else if (d.matches('details.boards-sub')) {
      if (d.open) d.parentNode.querySelectorAll('details.boards-sub[open]').forEach(function (o) { if (o !== d) o.removeAttribute('open'); });
      var m = POP.menuOf(d); if (m) placeDetails(m);
    }
  }
  // the phone scrim (.boards-scrim in #popover-root) is an outside click
  document.addEventListener('pointerdown', function (e) {
    var t = e.target; if (t && t.matches && t.matches('.boards-scrim')) { e.preventDefault(); openOnes().forEach(function (d) { d.removeAttribute('open'); var s = d.querySelector('summary'); s && s.focus({ preventScroll: true }); }); }
  }, true);
  function refit() { openOnes().forEach(placeDetails); }
  window.addEventListener('resize', refit);
  document.addEventListener('scroll', refit, true);
  phone.addEventListener && phone.addEventListener('change', refit);

  // keyboard: arrows / Home / End move between items; Tab wraps inside the menu (focus trap lite)
  document.addEventListener('keydown', function (e) {
    var menu = e.target.closest && e.target.closest('.boards-menu-pop'); if (!menu) return;
    var list = items(menu), i = list.indexOf(document.activeElement);
    var field = /^(INPUT|SELECT|TEXTAREA)$/.test(e.target.tagName);
    if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && !(field && e.target.tagName !== 'INPUT')) {
      if (e.target.tagName === 'INPUT' && e.target.type !== 'radio') return;
      if (e.target.type === 'radio') return; // native radio arrows
      e.preventDefault();
      var n = list.length; if (!n) return;
      list[(i + (e.key === 'ArrowDown' ? 1 : -1) + n) % n].focus();
    } else if (!field && (e.key === 'Home' || e.key === 'End') && list.length) {
      e.preventDefault(); list[e.key === 'Home' ? 0 : list.length - 1].focus();
    } else if (e.key === 'Tab') {
      var foc = [].slice.call(menu.querySelectorAll('button,summary,input,select,textarea,[tabindex]')).filter(function (el) { return !el.disabled && el.offsetParent && el.tabIndex >= 0 && !(el.type === 'radio' && !el.checked && menu.querySelector('input[type=radio][name="' + el.name + '"]:checked')); });
      if (!foc.length) return;
      if (!e.shiftKey && document.activeElement === foc[foc.length - 1]) { e.preventDefault(); foc[0].focus(); }
      else if (e.shiftKey && document.activeElement === foc[0]) { e.preventDefault(); foc[foc.length - 1].focus(); }
    }
  });

  // the quick-edit popover is a body-level, fixed element too
  function mountQuick() { var q = document.getElementById('boards-pop'); if (q && q.parentNode !== document.body) document.body.appendChild(q); }
  mountQuick(); document.addEventListener('DOMContentLoaded', mountQuick);

  // quick-edit popover: keep it on screen when its content grows (labels toggled etc.)
  document.addEventListener('htmx:afterSwap', function (e) {
    var p = document.getElementById('boards-pop');
    if (p && !p.hidden && e.target && p.contains(e.target) && p._anchor) place(p, p._anchor, { align: 'left', cover: true });
  });
})();
