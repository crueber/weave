// Drag and drop for cards and lists: pointer events (mouse, pen and touch via long-press),
// a floating ghost + dashed placeholder, edge auto-scroll, and a keyboard alternative
// (Space to grab, arrows to move, Space to drop, Esc to cancel). Drops POST to the
// existing server endpoints; without JS nothing here runs and cards stay clickable.
(function () {
  var B = window.Boards; if (!B || B.dndInit) return; B.dndInit = true;
  var EDGE = 56, MAXSPEED = 22, THRESH = 5, HOLD = 230;
  var S = null, raf = 0, last = { x: 0, y: 0 };
  B.grab = { active: false };

  function lists() { return B.lists(); }
  function nameOf(tile) { var l = tile.querySelector('.boards-tile-link'); return l ? l.textContent.trim() : 'card'; }
  function colName(col) { var h = col.querySelector('.boards-col-title'); return h ? h.textContent.trim() : 'list'; }
  function isTile(n) { return n && n.classList && n.classList.contains('boards-tile') && !n.classList.contains('is-pending'); }
  function tileSiblings(box, skip) { return [].slice.call(box.children).filter(function (n) { return isTile(n) && n !== skip; }); }
  function prevTile(n) { for (var p = n.previousElementSibling; p; p = p.previousElementSibling) if (isTile(p)) return p; return null; }
  function nextTile(n) { for (var p = n.nextElementSibling; p; p = p.nextElementSibling) if (isTile(p)) return p; return null; }
  function openCols() { return [].slice.call(lists().children).filter(function (c) { return c.classList.contains('boards-col') && !c.classList.contains('is-collapsed'); }); }

  // browsers start native drags on links/images; we drive our own
  document.addEventListener('dragstart', function (e) { if (e.target.closest && e.target.closest('.boards-lists')) { e.preventDefault(); e.stopImmediatePropagation(); } }, true);

  // ---------- pointer drag ----------
  document.addEventListener('pointerdown', function (e) {
    if (S || B.grab.active || !lists() || !B.canEdit()) return;
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    var t = e.target; if (!t.closest) return;
    if (t.closest('.boards-composer, .boards-menu, .boards-pop, input, textarea, select, [data-quick], [data-label-toggle], [data-collapse], [data-add-top], .boards-col-title-input')) return;
    var kind = '', el = null;
    var tile = t.closest('.boards-tile');
    if (tile && tile.closest('.boards-cards')) { kind = 'card'; el = tile; }
    else { var head = t.closest('.boards-col-head'); if (head) { kind = 'list'; el = head.closest('.boards-col'); } }
    if (!el) return;
    S = { kind: kind, el: el, id: e.pointerId, x0: e.clientX, y0: e.clientY, touch: e.pointerType === 'touch', started: false, timer: 0 };
    last.x = e.clientX; last.y = e.clientY;
    if (S.touch) S.timer = setTimeout(function () { if (S && !S.started) begin(); }, HOLD);
  });

  document.addEventListener('pointermove', function (e) {
    if (!S || e.pointerId !== S.id) return;
    last.x = e.clientX; last.y = e.clientY;
    if (!S.started) {
      var d = Math.hypot(e.clientX - S.x0, e.clientY - S.y0);
      if (S.touch) { if (d > 9) cancelPending(); return; }           // finger moved: that is a scroll
      if (d >= THRESH) begin();
      return;
    }
    e.preventDefault();
    moveGhost();
  }, { passive: false });

  function cancelPending() { if (S) { clearTimeout(S.timer); S = null; } }
  function noScroll(e) { if (S && S.started) e.preventDefault(); }
  function noMenu(e) { if (S) e.preventDefault(); }
  document.addEventListener('touchmove', noScroll, { passive: false });
  document.addEventListener('contextmenu', noMenu);

  function begin() {
    clearTimeout(S.timer);
    var el = S.el, r = el.getBoundingClientRect();
    S.started = true; B.dragging = true;
    S.offX = S.x0 - r.left; S.offY = S.y0 - r.top;
    S.home = { parent: el.parentNode, next: el.nextElementSibling };
    var g = el.cloneNode(true); g.removeAttribute('id'); g.classList.add('boards-ghost');
    g.style.width = r.width + 'px'; g.style.height = r.height + 'px';
    document.body.appendChild(g); S.ghost = g;
    el.classList.add('is-origin');
    document.body.classList.add('boards-dragging'); lists().classList.add('is-dragging');
    try { document.activeElement && document.activeElement.blur && document.activeElement.blur(); } catch (x) { /* */ }
    B.announce('Picked up ' + (S.kind === 'card' ? 'card ' + nameOf(el) : 'list ' + colName(el)));
    moveGhost(); loop();
  }

  function moveGhost() {
    S.ghost.style.left = (last.x - S.offX) + 'px'; S.ghost.style.top = (last.y - S.offY) + 'px';
    place(last.x, last.y);
  }

  function nearestCol(x) {
    var cols = openCols(), best = null, bd = 1e9;
    cols.forEach(function (c) {
      var r = c.getBoundingClientRect(), d = x < r.left ? r.left - x : x > r.right ? x - r.right : 0;
      if (d < bd) { bd = d; best = c; }
    });
    return best;
  }

  function place(x, y) {
    if (!S) return;
    if (S.kind === 'card') {
      var el = document.elementFromPoint(x, y), col = el && el.closest && el.closest('.boards-col');
      if (!col || col.classList.contains('is-collapsed') || !lists().contains(col)) col = nearestCol(x);
      if (!col) return;
      var box = col.querySelector('.boards-cards'); if (!box) return;
      var before = null, tiles = tileSiblings(box, S.el);
      for (var i = 0; i < tiles.length; i++) { var r = tiles[i].getBoundingClientRect(); if (y < r.top + r.height / 2) { before = tiles[i]; break; } }
      var anchor = before || box.querySelector('.boards-empty-list') || null;
      if (S.el.parentNode !== box || (before ? S.el.nextElementSibling !== before : nextTile(S.el))) box.insertBefore(S.el, anchor);
      lists().querySelectorAll('.boards-cards.is-drop-target').forEach(function (b) { if (b !== box) b.classList.remove('is-drop-target'); });
      box.classList.add('is-drop-target');
    } else {
      var cols = openCols().filter(function (c) { return c !== S.el; }), bef = null;
      for (var j = 0; j < cols.length; j++) { var rr = cols[j].getBoundingClientRect(); if (x < rr.left + rr.width / 2) { bef = cols[j]; break; } }
      var add = [].slice.call(lists().children).filter(function (c) { return c.classList.contains('boards-addlist'); })[0];
      var want = bef || add;
      if (want ? S.el.nextElementSibling !== want : S.el.nextElementSibling) lists().insertBefore(S.el, want);
    }
  }

  function loop() {
    cancelAnimationFrame(raf);
    (function tick() {
      if (!S || !S.started) return;
      var x = last.x, y = last.y, moved = false;
      var lr = lists().getBoundingClientRect();
      if (y > lr.top - 40 && y < lr.bottom + 40) {
        if (x < lr.left + EDGE) { lists().scrollLeft -= speed(lr.left + EDGE - x); moved = true; }
        else if (x > lr.right - EDGE) { lists().scrollLeft += speed(x - (lr.right - EDGE)); moved = true; }
      }
      if (S.kind === 'card') {
        var col = S.el.closest('.boards-col'), box = col && col.querySelector('.boards-cards');
        if (box) {
          var br = box.getBoundingClientRect();
          if (y < br.top + EDGE && y > br.top - 80) { box.scrollTop -= speed(br.top + EDGE - y); moved = true; }
          else if (y > br.bottom - EDGE && y < br.bottom + 80) { box.scrollTop += speed(y - (br.bottom - EDGE)); moved = true; }
        }
      }
      if (y < 60) { window.scrollBy(0, -speed(60 - y)); moved = true; }
      else if (y > window.innerHeight - 60) { window.scrollBy(0, speed(y - (window.innerHeight - 60))); moved = true; }
      if (moved) place(x, y);
      raf = requestAnimationFrame(tick);
    })();
  }
  function speed(d) { return Math.max(2, Math.min(MAXSPEED, d / 3)); }

  function end(cancel) {
    if (!S) return;
    clearTimeout(S.timer);
    var s = S; S = null;
    if (!s.started) return;
    cancelAnimationFrame(raf);
    s.ghost.remove();
    document.body.classList.remove('boards-dragging'); lists().classList.remove('is-dragging');
    lists().querySelectorAll('.is-drop-target').forEach(function (b) { b.classList.remove('is-drop-target'); });
    B.suppressClick = Date.now();
    if (cancel) { s.home.parent.insertBefore(s.el, s.home.next); s.el.classList.remove('is-origin'); B.dragging = false; B.announce('Move cancelled'); return; }
    s.el.classList.remove('is-origin');
    B.dragging = false;
    commit(s.kind, s.el, s.home);
  }
  document.addEventListener('pointerup', function (e) { if (S && e.pointerId === S.id) end(false); });
  document.addEventListener('pointercancel', function (e) { if (S && e.pointerId === S.id) end(true); });
  document.addEventListener('keydown', function (e) { if (S && S.started && e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); end(true); } }, true);

  // ---------- persist ----------
  function commit(kind, el, home) {
    var l = lists();
    if (kind === 'card') {
      var box = el.parentNode, col = box.closest('.boards-col'), oldCol = home.parent.closest('.boards-col');
      var p = prevTile(el), n = nextTile(el);
      var sameSpot = el.parentNode === home.parent && el.nextElementSibling === home.next;
      if (sameSpot) return;
      var all = tileSiblings(box), pos = all.indexOf(el) + 1;
      B.recount(col); if (oldCol && oldCol !== col) B.recount(oldCol);
      B.announce('Moved card ' + nameOf(el) + ' to ' + colName(col) + ', position ' + pos + ' of ' + all.length);
      B.post(l.dataset.cardDrop, { id: el.dataset.id, container: col.dataset.id, afterId: p ? p.dataset.id : '', beforeId: n ? n.dataset.id : '' }).then(function (r) {
        if (!r.ok) { B.toast(r.error || 'Could not move the card', { error: true }); B.refresh(true); }
      });
    } else {
      var cols = openCols(), i = cols.indexOf(el), pc = cols[i - 1], nc = cols[i + 1];
      var same = el.nextElementSibling === home.next && el.parentNode === home.parent;
      if (same) return;
      B.announce('Moved list ' + colName(el) + ' to position ' + (i + 1));
      B.post(l.dataset.listDrop, { id: el.dataset.id, afterId: pc ? pc.dataset.id : '', beforeId: nc ? nc.dataset.id : '' }).then(function (r) {
        if (!r.ok) { B.toast(r.error || 'Could not move the list', { error: true }); B.refresh(true); }
      });
    }
  }

  // ---------- keyboard: Space grabs, arrows move, Space drops ----------
  var G = B.grab;
  function link(el) { return el.querySelector('.boards-tile-link'); }
  function refocus() { var a = link(G.el); if (a) { a.focus({ preventScroll: true }); G.el.scrollIntoView({ block: 'nearest', inline: 'nearest' }); } }
  function say() {
    var box = G.el.parentNode, all = tileSiblings(box);
    B.announce(nameOf(G.el) + ': position ' + (all.indexOf(G.el) + 1) + ' of ' + all.length + ' in ' + colName(box.closest('.boards-col')));
  }
  function stopGrab(cancel) {
    G.el.classList.remove('is-grabbed'); G.el.removeAttribute('aria-grabbed');
    B.dragging = false; G.active = false;
    if (cancel) { G.home.parent.insertBefore(G.el, G.home.next); B.announce('Move cancelled'); refocus(); return; }
    commit('card', G.el, G.home); refocus();
  }
  document.addEventListener('keydown', function (e) {
    if (!lists() || e.ctrlKey || e.metaKey || e.altKey) return;
    if (!G.active) {
      var a = e.target;
      if (e.key === ' ' && a && a.classList && a.classList.contains('boards-tile-link') && B.canEdit() && !S) {
        e.preventDefault(); e.stopImmediatePropagation();
        G.active = true; G.el = a.closest('.boards-tile'); B.dragging = true;
        G.home = { parent: G.el.parentNode, next: G.el.nextElementSibling };
        G.el.classList.add('is-grabbed'); G.el.setAttribute('aria-grabbed', 'true');
        B.announce('Grabbed ' + nameOf(G.el) + '. Use the arrow keys to move it, Space to drop, Escape to cancel.');
      }
      return;
    }
    e.stopImmediatePropagation();
    var box = G.el.parentNode, k = e.key;
    if (k === 'ArrowUp' || k === 'ArrowDown') {
      e.preventDefault();
      var sib = k === 'ArrowUp' ? prevTile(G.el) : nextTile(G.el);
      if (sib) { if (k === 'ArrowUp') box.insertBefore(G.el, sib); else box.insertBefore(G.el, nextTile(sib) || box.querySelector('.boards-empty-list') || null); refocus(); say(); }
    } else if (k === 'ArrowLeft' || k === 'ArrowRight') {
      e.preventDefault();
      var cols = openCols(), ci = cols.indexOf(box.closest('.boards-col')), tc = cols[ci + (k === 'ArrowLeft' ? -1 : 1)];
      if (tc) {
        var tb = tc.querySelector('.boards-cards'), idx = tileSiblings(box).indexOf(G.el), ts = tileSiblings(tb);
        tb.insertBefore(G.el, ts[Math.min(idx, ts.length)] || tb.querySelector('.boards-empty-list') || null);
        refocus(); say();
      }
    } else if (k === ' ' || k === 'Enter') { e.preventDefault(); stopGrab(false); }
    else if (k === 'Escape') { e.preventDefault(); stopGrab(true); }
    else if (k === 'Tab') { stopGrab(false); }
  }, true);
})();
