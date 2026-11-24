// One card on several boards, the write half (docs/DESIGN.md "Placements"):
//   - the "Add to board..." picker: [data-place-open] in the card sheet, and the shortcut b on the focused / hovered
//     tile or in the open card. Boards you can edit, by project, filtered as you type; Enter (or a click) on a board
//     shows its columns with the first open one focused; Enter there places the card and the picker stays open for
//     the next board. Never clipped: it is fixed-positioned in #popover-root over the page, or inside the sheet's
//     <dialog> while the sheet is open (a body-level element would sit under the modal dialog's top layer and be
//     inert); on phones it is a bottom sheet.
//   - "Remove from this board" ([data-unplace]: the sheet's Boards row "x" and "..." menu item) after a confirmation.
//   - the styled confirmation of every hx-confirm that carries data-confirm-title (archive / trash / delete of a card
//     on several boards, "Remove from this board" on a tile): Enter confirms, Escape cancels.
//   - the live refresh of an open card sheet when the card is placed on or removed from a board (or moved there),
//     which swaps only the board crumb, the Boards row and the "..." menu, never what is being typed.
// Loaded by the board page and by the card sheet itself (the full /cards/{id} page has no board scripts).
(function () {
  if (window.CardPlace) return;
  var HDR = { 'HX-Request': 'true', 'X-Requested-With': 'weave' };
  var phone = window.matchMedia ? matchMedia('(max-width:640px)') : { matches: false };

  function byId(id) { return document.getElementById(id); }
  function typing(el) { return !!(el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName))); }
  function B() { return window.Boards || null; }
  function sheet() { var cm = byId('card-modal'); if (!cm) return null; var d = cm.closest('dialog'); return !d || d.open ? cm : null; }
  function modalOpen() { var m = byId('modal'); return !!(m && m.open); }
  // a toast that is seen: the board's toasts on the board page, else (the card sheet is a modal <dialog>: anything
  // outside it is painted under its top layer) a toast host inside the dialog, else the layout's toasts
  function toast(msg, err) {
    var b = B(), m = byId('modal');
    if (m && m.open) {
      var h = m.querySelector(':scope > .cm-toasts');
      if (!h) { h = document.createElement('div'); h.className = 'cm-toasts'; h.setAttribute('aria-live', 'polite'); m.appendChild(h); }
      var t = document.createElement('div'); t.className = 'toast' + (err ? ' toast-error' : ''); if (err) t.setAttribute('role', 'alert'); t.textContent = msg;
      h.appendChild(t); setTimeout(function () { t.remove(); }, 4000);
      return;
    }
    if (b && b.toast) { b.toast(msg, { error: !!err }); return; }
    document.body.dispatchEvent(new CustomEvent('toast', { detail: { value: msg } }));
  }
  function announce(msg) { var b = B(); if (b && b.announce) b.announce(msg); }
  function errorOf(html) { var m = /<div[^>]*toast[^>]*>([^<]*)</.exec(html || ''); return (m && m[1]) || ''; }
  function post(url, data) {
    var body = new URLSearchParams(); Object.keys(data || {}).forEach(function (k) { if (data[k] !== undefined) body.append(k, data[k]); });
    var h = { 'Content-Type': 'application/x-www-form-urlencoded' }; for (var k in HDR) h[k] = HDR[k];
    return fetch(url, { method: 'POST', credentials: 'same-origin', body: body, headers: h }).then(function (r) {
      return r.text().then(function (html) {
        var trig = {}; try { trig = JSON.parse(r.headers.get('HX-Trigger') || '{}'); } catch (x) { trig = {}; }
        return { ok: r.ok, status: r.status, html: html, trigger: trig, error: r.ok ? '' : (errorOf(html) || 'Something went wrong') };
      });
    }, function () { return { ok: false, status: 0, html: '', trigger: {}, error: 'You appear to be offline' }; });
  }
  // re-render the open sheet (through htmx, so the sheet's section stash and popover logic in cards_ui.js apply)
  function reloadSheet(id, board) {
    var cm = sheet(); if (!cm || cm.dataset.card !== id || !window.htmx) return Promise.resolve();
    return htmx.ajax('GET', '/ui/cards/' + id + (board ? '?board=' + encodeURIComponent(board) : ''), { target: '#card-modal', swap: 'outerHTML' });
  }
  // after a placement changed: the sheet and the board page under it follow
  function afterChange(id, board, placed, sheetBoard) {
    var b = B();
    if (sheet()) reloadSheet(id, sheetBoard);
    if (b && b.boardId && b.boardId()) {
      if (!placed && board === b.boardId()) b.schedule ? b.schedule(50) : b.refresh(true);   // the tile leaves this board
      else if (byId('tile-' + id) && b.refreshTile) b.refreshTile(id, false);                // its "also on" marker changes
    }
  }

  // ---------------------------------------------------------------- confirm ----
  // A small modal <dialog> (top layer: above the card sheet too). Focus starts on the confirming button: Enter / Space
  // confirm, Escape or Cancel cancel; Tab stays inside.
  function confirmBox(o) {
    var d = document.createElement('dialog'); d.className = 'cm-confirm';
    d.setAttribute('aria-labelledby', 'cm-confirm-h'); d.setAttribute('aria-describedby', 'cm-confirm-p');
    var h = document.createElement('p'); h.className = 'cm-confirm-h'; h.id = 'cm-confirm-h'; h.textContent = o.title;
    var p = document.createElement('p'); p.className = 'cm-confirm-p'; p.id = 'cm-confirm-p'; p.textContent = o.text || '';
    var row = document.createElement('div'); row.className = 'cm-confirm-row';
    var no = document.createElement('button'); no.type = 'button'; no.className = 'btn btn-ghost cm-confirm-no'; no.textContent = 'Cancel';
    var yes = document.createElement('button'); yes.type = 'button'; yes.className = 'btn ' + (o.danger ? 'btn-danger' : 'btn-primary') + ' cm-confirm-yes'; yes.textContent = o.ok || 'OK';
    row.appendChild(no); row.appendChild(yes); d.appendChild(h); if (o.text) d.appendChild(p); d.appendChild(row);
    (modalOpen() ? byId('modal') : document.body).appendChild(d);
    var back = document.activeElement, done = false;
    function finish(ok) {
      if (done) return; done = true;
      if (d.open) d.close(); d.remove();
      if (!ok) { if (back && document.contains(back) && back.focus) back.focus({ preventScroll: true }); if (o.onCancel) o.onCancel(); return; }
      o.onOk();
    }
    yes.addEventListener('click', function () { finish(true); });
    no.addEventListener('click', function () { finish(false); });
    d.addEventListener('cancel', function (e) { e.preventDefault(); finish(false); });
    d.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finish(false); }
      else if (e.key === 'Tab') { e.preventDefault(); (document.activeElement === yes ? no : yes).focus(); }
      else e.stopPropagation();   // no shortcut of the page or the sheet runs under the confirmation
    });
    d.addEventListener('pointerdown', function (e) { if (e.target === d) { var r = d.getBoundingClientRect(); if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) finish(false); } });
    d.showModal();
    yes.focus();
    return d;
  }
  // hx-confirm with data-confirm-title: the styled confirmation instead of the browser's
  document.addEventListener('htmx:confirm', function (e) {
    var el = e.detail && e.detail.elt; if (!el || !el.dataset || !el.dataset.confirmTitle || !e.detail.question) return;
    e.preventDefault();
    confirmBox({ title: el.dataset.confirmTitle, text: e.detail.question, ok: el.dataset.confirmOk || 'OK', danger: true, onOk: function () { e.detail.issueRequest(true); } });
  });

  // ---------------------------------------------------------------- the picker ----
  var cur = null;   // { id, board, host, anchor, back }
  function popRoot() {
    var r = byId('popover-root');
    if (!r) { r = document.createElement('div'); r.id = 'popover-root'; r.innerHTML = '<div class="boards-scrim" hidden></div>'; document.body.appendChild(r); }
    return r;
  }
  // the host: inside the sheet's dialog while it is open (outside #card-modal, which re-renders), else #popover-root
  function hostFor() {
    var m = byId('modal'), parent = m && m.open ? m : popRoot();
    var h = parent.querySelector(':scope > .cm-place-host');
    if (!h) { h = document.createElement('div'); h.className = 'cm-place-host'; parent.appendChild(h); }
    return h;
  }
  function place(box, anchor) {
    if (!box) return;
    if (phone.matches) { box.style.cssText = ''; return; }        // CSS: a bottom sheet
    if (!anchor || !document.contains(anchor)) return;             // re-rendered under us: stay where we are
    var r = anchor.getBoundingClientRect(), vw = document.documentElement.clientWidth, vh = window.innerHeight, M = 8, G = 4;
    box.style.position = 'fixed'; box.style.left = '0px'; box.style.top = '0px'; box.style.maxHeight = 'none';
    var w = box.offsetWidth, h = box.offsetHeight;
    var left = Math.max(M, Math.min(r.left, vw - w - M)), below = vh - r.bottom - G - M, above = r.top - G - M, top, mh;
    if (h <= below || below >= above) { top = r.bottom + G; mh = below; } else { mh = above; top = Math.max(M, r.top - G - Math.min(h, above)); }
    box.style.left = left + 'px'; box.style.top = top + 'px'; box.style.maxHeight = Math.max(200, mh) + 'px';
  }
  function box() { return cur && cur.host ? cur.host.querySelector('.cm-place') : null; }
  function close(focusBack) {
    if (!cur) return;
    var c = cur; cur = null;
    c.host.innerHTML = ''; c.host.remove();
    var t = c.back && document.contains(c.back) && c.back !== document.body ? c.back : null;
    if (!t && c.tileId) { var tl = byId(c.tileId); t = tl && tl.querySelector('.boards-tile-link'); }
    if (!t) { var cm = sheet(); t = cm && (cm.querySelector('.cm-board-add') || cm.closest('dialog')); }
    if (focusBack !== false && t && t.focus) t.focus({ preventScroll: true });
  }
  function open(id, board, anchor, tile) {
    close(false);
    var b = B(); if (b && b.closePop) b.closePop(false);
    var back = document.activeElement;
    return fetch('/ui/cards/' + encodeURIComponent(id) + '/place-picker' + (board ? '?board=' + encodeURIComponent(board) : ''), { credentials: 'same-origin', headers: HDR })
      .then(function (r) { return r.text().then(function (html) { return { ok: r.ok, html: html, status: r.status }; }); })
      .then(function (r) {
        if (!r.ok) { toast(errorOf(r.html) || (r.status === 403 ? 'Read-only: you cannot change this card' : 'Could not open the board picker'), true); return; }
        var host = hostFor();
        host.innerHTML = r.html;
        cur = { id: id, board: board, host: host, anchor: anchor, back: back, tileId: tile ? tile.id : '' };
        var bx = box(); if (!bx) return;
        place(bx, anchor);
        var f = bx.querySelector('[data-place-filter]') || bx.querySelector('.cm-place-opt') || bx;
        f.focus({ preventScroll: true });
        announce('Add to board: type to find a board');
      }, function () { toast('You appear to be offline', true); });
  }
  function step() { var bx = box(); return bx ? bx.querySelector('.cm-place-step:not([hidden])') : null; }
  function opts(st) { return st ? [].slice.call(st.querySelectorAll('.cm-place-opt')).filter(function (o) { return !o.hidden && !(o.closest('[data-place-group]') || {}).hidden; }) : []; }
  function filter() {
    var bx = box(); if (!bx) return;
    var f = bx.querySelector('[data-place-filter]'), q = f ? f.value.trim().toLowerCase() : '', n = 0;
    bx.querySelectorAll('[data-place-group]').forEach(function (g) {
      var any = false;
      g.querySelectorAll('[data-place-board]').forEach(function (o) { var hide = !!q && (o.dataset.text || '').toLowerCase().indexOf(q) < 0; o.hidden = hide; if (!hide) { any = true; n++; } });
      g.hidden = !any;
    });
    var none = bx.querySelector('.cm-place-none'); if (none) none.hidden = n > 0;
  }
  function msg(kind, text) {
    var bx = box(); if (!bx) return;
    var e = bx.querySelector('.cm-place-err'), ok = bx.querySelector('.cm-place-ok');
    if (e) { e.textContent = kind === 'err' ? text : ''; e.hidden = kind !== 'err'; }
    if (ok) ok.textContent = kind === 'ok' ? text : '';
  }
  function showBoards(focusFilter) {
    var bx = box(); if (!bx) return;
    bx.querySelectorAll('.cm-place-step').forEach(function (s) { s.hidden = s.dataset.step !== 'boards'; });
    var f = bx.querySelector('[data-place-filter]');
    if (focusFilter !== false && f) f.focus({ preventScroll: true });
    place(bx, cur && cur.anchor);
  }
  function showCols(boardId) {
    var bx = box(); if (!bx) return;
    var cols = bx.querySelector('[data-cols-for="' + (window.CSS && CSS.escape ? CSS.escape(boardId) : boardId) + '"]'); if (!cols) return;
    bx.querySelectorAll('.cm-place-step').forEach(function (s) { s.hidden = s !== cols; });
    msg('', '');
    var d = cols.querySelector('.cm-place-col.is-default') || cols.querySelector('.cm-place-col');
    if (d) d.focus({ preventScroll: true });
    place(bx, cur && cur.anchor);
  }
  function placeIn(opt) {
    if (!cur || opt.disabled) return;
    var c = cur, list = opt.dataset.placeList, to = opt.dataset.placeTo, name = opt.dataset.boardName || 'the board', col = (opt.querySelector('.cm-place-t') || opt).textContent.trim();
    opt.disabled = true; opt.setAttribute('aria-busy', 'true');
    post('/ui/cards/' + encodeURIComponent(c.id) + '/placements', { listId: list, board: c.board }).then(function (r) {
      opt.disabled = false; opt.removeAttribute('aria-busy');
      if (cur !== c) return;
      if (!r.ok) { msg('err', r.error); opt.focus({ preventScroll: true }); return; }
      // the board is done: drop it from the list, back to the boards (filter cleared) for the next one
      var bx = box();
      var bo = bx && bx.querySelector('[data-place-board="' + (window.CSS && CSS.escape ? CSS.escape(to) : to) + '"]');
      if (bo) { var g = bo.closest('[data-place-group]'); bo.remove(); if (g && !g.querySelector('[data-place-board]')) g.remove(); }
      var cs = bx && bx.querySelector('[data-cols-for="' + (window.CSS && CSS.escape ? CSS.escape(to) : to) + '"]'); if (cs) cs.remove();
      var f = bx && bx.querySelector('[data-place-filter]'); if (f) f.value = '';
      filter();
      var left = bx ? bx.querySelectorAll('[data-place-board]').length : 0;
      showBoards();
      msg('ok', 'Added to ' + name + ' · ' + col + (left ? '' : '. No other board left.'));
      announce('Added to ' + name); toast('Added to ' + name);
      if (!left && bx) { var st = bx.querySelector('[data-step="boards"]'); if (st) st.hidden = true; bx.focus({ preventScroll: true }); }
      afterChange(c.id, to, true, c.board);
    });
  }

  document.addEventListener('click', function (e) {
    var t = e.target; if (!t || !t.closest) return;
    var bx = t.closest('.cm-place');
    if (bx) {
      if (t.closest('[data-place-close]')) { e.preventDefault(); close(); return; }
      if (t.closest('[data-place-back]')) { e.preventDefault(); showBoards(); return; }
      var bo = t.closest('[data-place-board]'); if (bo) { e.preventDefault(); showCols(bo.dataset.placeBoard); return; }
      var co = t.closest('[data-place-list]'); if (co) { e.preventDefault(); placeIn(co); return; }
      return;
    }
    var op = t.closest('[data-place-open]');
    if (op) {
      var cm = op.closest('#card-modal'); if (!cm) return;
      e.preventDefault();
      var menu = op.closest('details'); if (menu && menu.classList.contains('cm-menu')) menu.removeAttribute('open');
      var anchor = cm.querySelector('.cm-board-add') || op;
      open(cm.dataset.card, cm.dataset.board, anchor.offsetParent ? anchor : op);
      return;
    }
    var un = t.closest('[data-unplace]');
    if (un && un.closest('#card-modal')) { e.preventDefault(); askUnplace(un); }
  });
  document.addEventListener('input', function (e) { if (e.target.matches && e.target.matches('.cm-place [data-place-filter]')) { msg('', ''); filter(); } });
  document.addEventListener('pointerdown', function (e) {
    if (!cur || !e.target.closest) return;
    if (e.target.closest('.cm-place, .cm-confirm, [data-place-open]')) return;
    close(false);
  }, true);
  window.addEventListener('resize', function () { if (cur) place(box(), cur.anchor); });
  // the sheet closing takes its picker with it
  document.addEventListener('close', function (e) { if (cur && e.target && e.target.id === 'modal') close(false); }, true);

  // keys inside the picker (capture: before the sheet's and the board's own shortcuts)
  document.addEventListener('keydown', function (e) {
    var bx = e.target.closest && e.target.closest('.cm-place'); if (!bx || !cur || e.isComposing) return;
    var k = e.key, st = step(), os = opts(st), i = os.indexOf(document.activeElement), f = bx.querySelector('[data-place-filter]');
    var inCols = !!(st && st.dataset.step === 'cols');
    if (k === 'Escape') { e.preventDefault(); e.stopPropagation(); if (inCols) showBoards(); else close(); return; }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    e.stopPropagation();
    if (k === 'Tab') {
      var foc = [].slice.call(bx.querySelectorAll('button:not([disabled]),input')).filter(function (el) { return el.offsetParent && !el.closest('[hidden]'); });
      if (!foc.length) return;
      var j = foc.indexOf(document.activeElement);
      if (!e.shiftKey && j === foc.length - 1) { e.preventDefault(); foc[0].focus(); }
      else if (e.shiftKey && j <= 0) { e.preventDefault(); foc[foc.length - 1].focus(); }
      return;
    }
    if (k === 'ArrowDown' || k === 'ArrowUp') {
      if (!os.length) return;
      e.preventDefault();
      if (e.target === f) { if (k === 'ArrowDown') os[0].focus(); return; }
      if (i === 0 && k === 'ArrowUp' && f && !inCols) { f.focus(); return; }
      os[(i + (k === 'ArrowDown' ? 1 : -1) + os.length) % os.length].focus();
      return;
    }
    if (k === 'Enter' && e.target === f) { e.preventDefault(); if (os[0]) showCols(os[0].dataset.placeBoard); return; }
    if (inCols && (k === 'ArrowLeft' || k === 'Backspace')) { e.preventDefault(); showBoards(); return; }
    if (!inCols && k === 'ArrowRight' && e.target.dataset && e.target.dataset.placeBoard) { e.preventDefault(); showCols(e.target.dataset.placeBoard); return; }
    if ((k === 'Home' || k === 'End') && os.length && !typing(e.target)) { e.preventDefault(); os[k === 'Home' ? 0 : os.length - 1].focus(); return; }
    // typing on a board goes to the filter
    if (!inCols && f && !typing(e.target) && (k === 'Backspace' || (k.length === 1 && k !== ' '))) {
      e.preventDefault();
      f.value = k === 'Backspace' ? f.value.slice(0, -1) : f.value + k;
      f.focus(); filter();
    }
  }, true);

  // ---------------------------------------------------------------- Remove from this board ----
  function askUnplace(btn) {
    var cm = btn.closest('#card-modal'); if (!cm) return;
    var id = cm.dataset.card, board = btn.dataset.unplace, name = btn.dataset.boardName || 'this board', ctx = cm.dataset.board;
    var menu = btn.closest('details'); if (menu) menu.removeAttribute('open');
    confirmBox({ title: 'Remove from ' + name + '?', text: 'The card stays on its other boards.', ok: 'Remove from board', danger: true, onOk: function () {
      post('/ui/cards/' + encodeURIComponent(id) + '/placements/' + encodeURIComponent(board) + '/delete', { board: ctx }).then(function (r) {
        if (!r.ok) { toast(r.error, true); return; }
        announce('Removed from ' + name); toast('Removed from ' + name);
        afterChange(id, board, false, board === ctx ? '' : ctx);
      });
    } });
  }
  // a tile's quick edit removed the card from this board (hx-post, HX-Trigger cards:placement)
  document.addEventListener('cards:placement', function (e) {
    var d = e.detail || {}, el = e.target;
    if (!el || !el.closest || !el.closest('.boards-quick')) return;   // the sheet and the picker handle their own
    var b = B(); if (b && b.closePop) b.closePop(false);
    afterChange(d.cardId, d.boardId, !!d.placed, '');
  });

  // ---------------------------------------------------------------- shortcut b ----
  var roAt = 0;
  function readOnly() { if (Date.now() - roAt > 2500) { roAt = Date.now(); toast('Read-only: you cannot add this card to another board'); } }
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'b' || e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing || e.repeat) return;
    if (typing(e.target) || (e.target.closest && e.target.closest('.cm-place'))) return;
    if (document.querySelector('dialog.cm-confirm[open], dialog.boards-confirm[open]')) return;
    var cm = sheet();
    if (cm) {
      if (cm.querySelector('details.cards-pop[open], details.x-pop[open]') || (byId('cm-help') && !byId('cm-help').hidden)) return;
      if (e.target.tagName === 'SUMMARY') return;
      e.preventDefault();
      if (cm.dataset.draft || cm.dataset.template) return;
      if (!cm.dataset.canPlace) { if (!cm.dataset.canEdit) readOnly(); else toast('No other board to add this card to'); return; }
      var a = cm.querySelector('.cm-board-add');
      open(cm.dataset.card, cm.dataset.board, a && a.offsetParent ? a : (cm.querySelector('#card-title') || cm));
      return;
    }
    if (modalOpen()) return;                                   // another dialog (agent prompt...) owns the keys
    var b = B(); if (!b || !b.targetTile) return;
    var p = byId('boards-pop');
    if ((p && !p.hidden) || document.querySelector('details.boards-menu[open], details.boards-dd[open], #boards-help[open]') || (b.grab && b.grab.active)) return;
    var t = b.targetTile(); if (!t || !document.contains(t) || t.classList.contains('is-pending') || t.classList.contains('boards-draft')) return;
    e.preventDefault();
    if (!b.canEdit() && !t.querySelector('[data-quick]')) { readOnly(); return; }
    open(t.dataset.id, b.boardId(), t, t);
  });

  // ---------------------------------------------------------------- live refresh of the sheet ----
  // A placement event about the open card (placed, removed, moved on one of its boards, re-homed): refetch the
  // sheet's board parts (card.placements' UI route) and swap each one that is not in use; when the context board
  // itself is gone, reload the whole sheet once nothing is being typed.
  var liveTimer = null, stale = false;
  function busy(el) { return !!(el && (el.querySelector('details[open]') || el.contains(document.activeElement) || (el.matches && el.matches('details[open]')))); }
  function sheetIdle(cm) {
    var a = document.activeElement;
    if (a && cm.contains(a) && typing(a)) return false;
    var ta = cm.querySelector('[data-comment-input]');
    return !(cm.querySelector('details[open], #card-desc form') || (ta && ta.value.trim()) || cur || document.querySelector('dialog.cm-confirm[open]'));
  }
  function liveRefresh() {
    var cm = sheet(); if (!cm || !window.htmx) return;
    var id = cm.dataset.card, ctx = cm.dataset.board;
    fetch('/ui/cards/' + encodeURIComponent(id) + '/placements?board=' + encodeURIComponent(ctx), { credentials: 'same-origin', headers: { 'HX-Request': 'true', 'X-Requested-With': 'weave', 'X-Weave-Background': '1' } })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) {
        var now = sheet(); if (!html || !now || now.dataset.card !== id) return;
        var t = document.createElement('template'); t.innerHTML = html.trim();
        var live = t.content.querySelector('[data-boards-live]'); if (!live) return;
        if (live.dataset.board !== now.dataset.board) { stale = true; tryStale(); return; }
        ['cm-crumb-board', 'cm-boards-prop', 'cm-menu'].forEach(function (k) {
          var old = now.querySelector('#' + k), fresh = live.querySelector('#' + k);
          if (!old || !fresh || busy(old)) return;
          if (old.outerHTML === fresh.outerHTML) return;
          var imp = document.importNode(fresh, true); old.replaceWith(imp); htmx.process(imp);
        });
        now.toggleAttribute('data-multi', !!live.dataset.multi);
        now.toggleAttribute('data-can-place', !!live.dataset.canPlace);
        if (now.dataset.multi === '') now.dataset.multi = '1';
        if (now.dataset.canPlace === '') now.dataset.canPlace = '1';
      }).catch(function () { /* the next event retries */ });
  }
  function tryStale() {
    var cm = sheet(); if (!stale || !cm) { stale = false; return; }
    if (!sheetIdle(cm)) { setTimeout(tryStale, 1500); return; }
    stale = false; reloadSheet(cm.dataset.card, '');
  }
  function onLive(ev) {
    var cm = sheet(); if (!cm) return;
    var d; try { d = JSON.parse(ev.data); } catch (x) { return; }
    var data = (d && d.data) || {}, id = data.cardId || data.id || (d && d.cardId);
    if (id !== cm.dataset.card) return;
    clearTimeout(liveTimer); liveTimer = setTimeout(liveRefresh, 400);
  }
  // the layout's notify_live.js (WeaveLive) is deferred: on the full /cards/{id} page this script runs first, so hook up
  // once it exists (now, else at DOMContentLoaded)
  var hooked = false;
  function hookLive() {
    var live = window.WeaveLive; if (hooked || !live) return;
    hooked = true;
    ['card.placement.changed', 'card.placed', 'card.unplaced', 'card.moved', 'card.updated'].forEach(function (n) { live.on(n, onLive); });
    // the full /cards/{id} page has no board room of its own: listen on the sheet's board
    var cm = byId('card-modal');
    if (cm && !cm.closest('dialog') && !(B() && B().boardId && B().boardId())) live.setRooms('cardsheet', cm.dataset.board ? ['board:' + cm.dataset.board] : []);
  }
  hookLive(); document.addEventListener('DOMContentLoaded', hookLive);

  window.CardPlace = { open: open, close: close, confirm: confirmBox };
})();
