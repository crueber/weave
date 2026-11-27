/* Floating popover positioning for the board page: list "..." menus, filter-bar dropdowns and the card quick-edit
   popover. Popovers are position:fixed (so column / scroller overflow can never clip them); JS places them via
   Weave.place, flips above / to the left when there is no room and caps max-height. On phones (<=640px) CSS turns
   the list menu into a bottom sheet and JS leaves it alone.
   PORTAL: while open, a list menu's popover is moved out of its column into #popover-root (a direct child of <body>), so
   no column, scroller, stacking context or tile can ever draw over it. It goes back into its <details> on close. */
(function () {
  if (window.WeavePop) return;
  var W = window.Weave;
  var phone = window.matchMedia('(max-width:640px)');
  var place = W.place;
  window.WeavePlace = place;

  // ---- portal registry ----
  var POP = window.WeavePop = { open: {} };
  function listIdOf(d) { var c = d.closest && d.closest('.boards-col'); return (c && c.dataset.id) || d.dataset.listId || ''; }
  function portal(d) {
    var pop = d._pop || d.querySelector(':scope > .boards-menu-pop'); if (!pop) return null;
    var r = W.popRoot(), id = listIdOf(d);
    pop._home = d; d._pop = pop; pop.dataset.list = id;
    if (pop.parentNode !== r) r.appendChild(pop);
    POP.open[id || ('d' + (d._pid = d._pid || Math.random()))] = { details: d, pop: pop };
    r.firstChild.hidden = false;
    return pop;
  }
  function restore(d) {
    d._handledOpen = false;
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
  function reset(d) { if (d._pop) restore(d); else { d._handledOpen = false; var p = popOf(d); if (p) p.style.cssText = ''; } }
  function openOnes() { return document.querySelectorAll('details.boards-menu[open], details.boards-dd[open]'); }

  function items(menu) {
    return [].slice.call(menu.querySelectorAll('[role=menuitem]')).filter(function (el) {
      return !el.disabled && !!el.offsetParent;
    });
  }

  // toggle does not bubble: capture it.
  document.addEventListener('toggle', function (e) { onToggle(e.target); }, true);
  // belt and braces for engines that do not fire toggle reliably: re-run after a summary click
  document.addEventListener('click', function (e) {
    var s = e.target.closest && e.target.closest('details.boards-menu > summary, details.boards-dd > summary');
    if (s) setTimeout(function () { onToggle(s.parentNode); }, 0);
  });
  window.WeaveMenuToggled = function (d) { onToggle(d); };
  function onToggle(d) {
    if (!d || !d.matches || !d.matches('details.boards-menu, details.boards-dd')) return;
    if (d._handledOpen === d.open) return;
    d._handledOpen = d.open;
    if (d.open) {
      openOnes().forEach(function (o) { if (o !== d) o.removeAttribute('open'); });
      if (d.matches('.boards-menu')) {
        if (!portal(d)) return; // move BEFORE focusing: moving a node drops focus
      }
      placeDetails(d);
      if (d.matches('.boards-menu')) { var f = items(popOf(d))[0]; f && f.focus({ preventScroll: true }); }
    } else reset(d);
  }
  // the phone scrim (.boards-scrim in #popover-root) is an outside click
  document.addEventListener('pointerdown', function (e) {
    var t = e.target; if (t && t.matches && t.matches('.boards-scrim')) { e.preventDefault(); openOnes().forEach(function (d) { d.removeAttribute('open'); var s = d.querySelector('summary'); s && s.focus({ preventScroll: true }); }); }
  }, true);

  var refitRaf = 0;
  function refit(e) {
    if (e && e.type === 'scroll' && e.target && e.target.closest && e.target.closest('.boards-menu-pop, .boards-dd-pop')) return;
    if (!openOnes().length) return;
    if (refitRaf) return;
    refitRaf = requestAnimationFrame(function () { refitRaf = 0; openOnes().forEach(placeDetails); });
  }
  window.addEventListener('resize', refit);
  document.addEventListener('scroll', refit, { capture: true, passive: true });
  phone.addEventListener && phone.addEventListener('change', refit);

  // keyboard: arrows / Home / End move between items; Tab wraps inside the menu (focus trap lite)
  document.addEventListener('keydown', function (e) {
    var menu = e.target.closest && e.target.closest('.boards-menu-pop'); if (!menu) return;
    var list = items(menu), i = list.indexOf(document.activeElement);
    var field = /^(INPUT|SELECT|TEXTAREA)$/.test(e.target.tagName);
    if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && !field) {
      e.preventDefault();
      var n = list.length; if (!n) return;
      list[(i + (e.key === 'ArrowDown' ? 1 : -1) + n) % n].focus();
    } else if (!field && (e.key === 'Home' || e.key === 'End') && list.length) {
      e.preventDefault(); list[e.key === 'Home' ? 0 : list.length - 1].focus();
    } else if (e.key === 'Tab') {
      W.trapTab(menu, e, function (el) {
        return !(el.type === 'radio' && !el.checked && menu.querySelector('input[type=radio][name="' + el.name + '"]:checked'));
      });
    }
  });

  // the quick-edit popover is a body-level, fixed element too
  function mountQuick() { var q = document.getElementById('boards-pop'); if (q && q.parentNode !== document.body) document.body.appendChild(q); }
  mountQuick(); document.addEventListener('DOMContentLoaded', mountQuick);

  // a list menu whose content grew (the closing-setting confirm swapped in): place it again so it stays on screen
  document.addEventListener('htmx:afterSwap', function (e) {
    var t = e.target; if (!t || !t.closest || !t.closest('.boards-menu-pop')) return;
    var d = POP.menuOf(t); if (d && d.open) placeDetails(d);
  });
  // quick-edit popover: keep it on screen when its content grows (labels toggled etc.)
  document.addEventListener('htmx:afterSwap', function (e) {
    var p = document.getElementById('boards-pop');
    if (!(p && !p.hidden && e.target && p.contains(e.target) && p._anchor)) return;
    var a = p._anchor;
    if (!a.isConnected) { a = a.id ? document.getElementById(a.id) : null; if (a) p._anchor = a; }
    if (a) place(p, a, { align: 'left', cover: true });
  });
})();
