// Board interactions: opening cards, quick edit popover, inline list rename, collapse,
// label chips, keyboard shortcuts and the ? help overlay.
(function () {
  var B = window.Boards; if (!B || B.keysInit) return; B.keysInit = true;
  var esc = B.esc;
  var HDR = { 'HX-Request': 'true', 'X-Requested-With': 'weave' };

  function typing(el) { return !!(el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName))); }
  function modalOpen() { var m = document.getElementById('modal'); return !!(m && m.open); }
  function helpOpen() { var h = document.getElementById('boards-help'); return !!(h && h.open); }

  // ---- open a card (links keep working without JS / with ctrl+click) ----
  B.openCard = function (id) { if (window.CardSheet) CardSheet.open(id); else location.href = '/cards/' + id; };

  var hovered = null;
  document.addEventListener('mouseover', function (e) { var t = e.target.closest && e.target.closest('.boards-tile'); if (t) hovered = t; });
  document.addEventListener('mouseout', function (e) { var t = e.target.closest && e.target.closest('.boards-tile'); if (t && !t.contains(e.relatedTarget) && hovered === t) hovered = null; });

  function tileFocused() { var a = document.activeElement; return a && a.closest ? a.closest('.boards-tile') : null; }
  function currentCol() {
    var t = tileFocused() || hovered, c = t && t.closest('.boards-col'); if (c) return c;
    var a = document.activeElement; c = a && a.closest && a.closest('.boards-col'); if (c) return c;
    var h = document.querySelector('.boards-col:hover'); if (h) return h;
    return document.querySelector('.boards-col:not(.is-collapsed)');
  }

  document.addEventListener('click', function (e) {
    if (B.suppressClick && Date.now() - B.suppressClick < 250) { e.preventDefault(); e.stopPropagation(); return; }
    var t = e.target;
    var chip = t.closest('[data-label-toggle]');
    if (chip) { e.preventDefault(); e.stopPropagation(); var l = B.lists(); if (l) { var on = l.classList.toggle('labels-open'); try { localStorage.setItem('weave.boards.labels', on ? '1' : ''); } catch (x) { /* */ } } return; }
    var q = t.closest('[data-quick]');
    if (q) { e.preventDefault(); e.stopPropagation(); B.openQuick(q.closest('.boards-tile')); return; }
    var title = t.closest('[data-rename]');
    if (title && !e.defaultPrevented) { B.renameList(title.closest('.boards-col')); return; }
    var col = t.closest('[data-collapse],[data-collapse-btn]');
    if (col) { var c = B.colOf(col); var d = B.menuOf(col); if (d) d.removeAttribute('open'); if (c) B.setCollapsed(c, !c.classList.contains('is-collapsed')); return; }
    if (t.closest('[data-rename-btn]')) { var cc = B.colOf(t); var dd = B.menuOf(t); if (dd) dd.removeAttribute('open'); B.renameList(cc); return; }
    if (t.closest('[data-help]')) { B.showHelp(); return; }
    var openLink = t.closest('[data-open-card]');
    if (openLink) {
      if (e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey || !window.htmx) return;
      e.preventDefault(); B.closePop(); B.openCard(openLink.dataset.openCard); return;
    }
    // a click anywhere else on a tile opens it
    var tile = t.closest('.boards-tile');
    if (tile && !t.closest('a,button,input,textarea,select,summary,label') && e.button === 0 && window.htmx) { B.closePop(); B.openCard(tile.dataset.id); }
  });
  try { if (localStorage.getItem('weave.boards.labels') && B.lists()) B.lists().classList.add('labels-open'); } catch (x) { /* */ }

  // ---- quick edit popover ----
  var popTile = null;
  B.closePop = function (focusTile) {
    var p = document.getElementById('boards-pop'); if (!p || p.hidden) return;
    p.hidden = true; p.innerHTML = ''; p._anchor = null;
    if (focusTile !== false && popTile && document.contains(popTile)) { var l = popTile.querySelector('.boards-tile-link'); l && l.focus({ preventScroll: true }); }
    popTile = null;
  };
  B.openQuick = function (tile) {
    if (!tile) return;
    var p = document.getElementById('boards-pop'); if (!p) return;
    var id = tile.dataset.id;
    popTile = tile;
    fetch('/ui/cards/' + id + '/quick', { credentials: 'same-origin', headers: HDR }).then(function (r) { return r.ok ? r.text() : Promise.reject(); }).then(function (html) {
      p.innerHTML = html; B.process(p); p.hidden = false;
      p._anchor = tile;
      if (window.WeavePlace) window.WeavePlace(p, tile, { align: 'left', cover: true });
      else { var r = tile.getBoundingClientRect(), w = p.offsetWidth || 304; p.style.left = Math.max(8, Math.min(r.left, window.innerWidth - w - 8)) + 'px'; p.style.top = Math.max(8, Math.min(r.top, window.innerHeight - p.offsetHeight - 8)) + 'px'; }
      var n = p.querySelector('[data-quick-name]'); if (n) { n.focus(); n.select(); }
    }).catch(function () { B.toast('Could not open quick edit', { error: true }); popTile = null; });
  };
  document.addEventListener('boards:closepop', function () { var t = popTile; B.announce('Card updated'); B.closePop(); if (t) B.refreshTile(t.dataset.id, true); });
  document.addEventListener('pointerdown', function (e) {
    var p = document.getElementById('boards-pop');
    if (p && !p.hidden && !p.contains(e.target) && !(e.target.closest && e.target.closest('[data-quick]'))) B.closePop(false);
  });
  document.addEventListener('keydown', function (e) {
    var p = document.getElementById('boards-pop'); if (!p || p.hidden) return;
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); B.closePop(); }
    else if (e.key === 'Enter' && !e.shiftKey && e.target.matches && e.target.matches('[data-quick-name]')) { e.preventDefault(); e.target.form.requestSubmit(); }
  }, true);
  // popover label buttons re-render the popover; keep the refreshed tile in the DOM
  document.addEventListener('htmx:afterSwap', function (e) { if (e.target && e.target.closest && e.target.closest('#boards-pop')) B.announce('Labels updated'); });

  // ---- inline list rename ----
  B.renameList = function (col) {
    if (!col || col.querySelector('.boards-col-title-input')) return;
    var h = col.querySelector('.boards-col-title'); if (!h || !h.dataset.rename) return;
    var id = h.dataset.rename, old = h.textContent;
    var input = document.createElement('input'); input.className = 'boards-col-title-input'; input.value = old; input.maxLength = 128; input.setAttribute('aria-label', 'List name'); input.setAttribute('data-busy', '');
    h.style.display = 'none'; h.after(input); input.focus(); input.select();
    var done = false;
    function finish(save) {
      if (done) return; done = true;
      var v = input.value.trim();
      input.remove(); h.style.display = '';
      if (!save || !v || v === old) return;
      h.textContent = v;
      B.post('/ui/lists/' + id, { name: v }, 'PATCH').then(function (r) {
        if (!r.ok) { h.textContent = old; B.toast(r.error || 'Could not rename the list', { error: true }); } else B.announce('List renamed to ' + v);
      });
      h.focus && h.focus();
    }
    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') { e.preventDefault(); finish(true); h.focus(); }
      else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finish(false); h.focus(); }
    });
    input.addEventListener('blur', function () { finish(true); });
  };
  document.addEventListener('keydown', function (e) {
    var h = e.target.closest && e.target.closest('[data-rename]');
    if (h && (e.key === 'Enter' || e.key === 'F2')) { e.preventDefault(); B.renameList(h.closest('.boards-col')); }
  });
  document.querySelectorAll('[data-rename]').forEach(function (h) { h.tabIndex = 0; });
  new MutationObserver(function (ms) {
    ms.forEach(function (m) { m.addedNodes.forEach(function (n) { if (n.querySelectorAll) { n.querySelectorAll('[data-rename]').forEach(function (h) { h.tabIndex = 0; }); if (n.matches && n.matches('[data-rename]')) n.tabIndex = 0; } }); });
  }).observe(document.body, { childList: true, subtree: true });

  // ---- help overlay ----
  B.showHelp = function () { var h = document.getElementById('boards-help'); if (h && !h.open) { if (h.showModal) h.showModal(); else h.setAttribute('open', ''); } };

  // ---- keyboard navigation + shortcuts ----
  function tilesOf(col) { return [].slice.call(col.querySelectorAll('.boards-tile:not(.is-pending) .boards-tile-link')); }
  function focusTileLink(a) { if (a) { a.focus(); a.closest('.boards-tile').scrollIntoView({ block: 'nearest', inline: 'nearest' }); } }
  function moveFocus(dir, horizontal) {
    var cols = [].slice.call(document.querySelectorAll('.boards-col:not(.is-collapsed)'));
    var cur = tileFocused() || hovered, col = cur ? cur.closest('.boards-col') : null;
    if (!cur || !col) { for (var i = 0; i < cols.length; i++) { var f = tilesOf(cols[i])[0]; if (f) { focusTileLink(f); return; } } return; }
    var links = tilesOf(col), idx = links.indexOf(cur.querySelector('.boards-tile-link'));
    if (!horizontal) { focusTileLink(links[Math.max(0, Math.min(links.length - 1, idx + dir))]); return; }
    var ci = cols.indexOf(col);
    for (var j = ci + dir; j >= 0 && j < cols.length; j += dir) {
      var l2 = tilesOf(cols[j]); if (l2.length) { focusTileLink(l2[Math.min(idx < 0 ? 0 : idx, l2.length - 1)]); return; }
    }
  }
  document.addEventListener('keydown', function (e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
    if (typing(e.target)) return;
    var key = e.key;
    if (key === '?') { e.preventDefault(); B.showHelp(); return; }
    if (modalOpen() || helpOpen()) return;
    if (B.grab && B.grab.active) return;                       // the grab mode owns the arrows/space
    var col;
    switch (key) {
      case 'c': case 'C':
        col = currentCol(); if (col && B.canEdit() && B.openComposer(col, { top: key === 'C' })) e.preventDefault(); break;
      case 'n': case 'N':
        if (B.canEdit() && B.openListComposer()) e.preventDefault(); break;
      case '/':
        var f = document.querySelector('[data-filter-input]'); if (f) { e.preventDefault(); f.focus(); f.select(); } break;
      case 'e':
        var t = tileFocused() || hovered; if (t && B.canEdit()) { e.preventDefault(); B.openQuick(t); } break;
      case 'j': e.preventDefault(); moveFocus(1); break;
      case 'k': e.preventDefault(); moveFocus(-1); break;
      case 'l': e.preventDefault(); moveFocus(1, true); break;
      case 'h': e.preventDefault(); moveFocus(-1, true); break;
      case 'Escape':
        if (document.activeElement && document.activeElement.closest && document.activeElement.closest('.boards-tile')) document.activeElement.blur();
        break;
    }
  });
})();
