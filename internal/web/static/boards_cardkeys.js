// Card action shortcuts (docs/FEATURES.md "Keyboard shortcuts"). They act on the focused tile (else the hovered one,
// the same targeting as `e`) and, while the card sheet is open, on the open card:
//   L labels picker   m assign members   d due date   p location   s start / S stop time tracking   x close or re-open
//   A archive (Undo)  Delete / Backspace: move to trash, after a confirmation (Undo); an Inbox draft or a card already
//   in the trash is deleted permanently, after a confirmation that says so.
// In the sheet, d / l / L / m / p open the sheet's own popovers (cards_ui.js); the rest is handled here.
// Never while typing, while a dialog, picker, menu or the grab mode owns the keyboard, or with Ctrl / Meta / Alt.
// Viewers get a small "read-only" toast. Every write goes through an existing UI route, which calls the owning op.
(function () {
  var B = window.Boards, W = window.Weave; if (!B || B.cardKeysInit) return; B.cardKeysInit = true;
  var HDR = { 'HX-Request': 'true', 'X-Requested-With': 'weave' };
  var esc = B.esc;

  function byId(id) { return document.getElementById(id); }
  function sheet() { var m = byId('modal'), cm = byId('card-modal'); return m && m.open && cm && m.contains(cm) ? cm : null; }
  function pop() { return byId('boards-pop'); }
  function popOpen() { var p = pop(); return !!(p && !p.hidden); }
  function confirmOpen() { return W.confirmOpen(); }
  function menusOpen() { return !!document.querySelector('details.boards-menu[open], details.boards-dd[open], #boards-help[open]'); }
  function sheetBusy(cm) {
    var h = byId('cm-help');
    return !!(cm.querySelector('details.cards-pop[open], details.x-pop[open]') || (h && !h.hidden));
  }
  function link(t) { return t && t.querySelector('.boards-tile-link'); }
  function focusTile(t) { var l = link(t); if (l) { l.focus({ preventScroll: true }); t.scrollIntoView({ block: 'nearest', inline: 'nearest' }); return true; } return false; }
  function nameOf(t) { var l = link(t); return l ? l.textContent.trim() : 'this card'; }
  // a card on other boards too (the tile's data-also, the sheet's data-others: boards the viewer can see): archive and
  // trash are CARD actions, so the wording says they cover every board
  function others(el) { return el ? (parseInt(el.dataset.also || el.dataset.others || '0', 10) || 0) : 0; }
  function everywhere(n) { return n ? ' (on all ' + (n + 1) + ' boards it is on)' : ''; }
  function trashText(name, n) { return n ? '“' + name + '” is also on ' + n + ' other board' + (n === 1 ? '' : 's') + '; it goes to the trash on all of them. You can undo or restore it from there.' : '“' + name + '” goes to the board’s trash. You can undo or restore it from there.'; }
  var roAt = 0;
  function readOnly() { if (Date.now() - roAt > 2500) { roAt = Date.now(); B.toast('Read-only: you cannot change cards on this board'); } }

  // where keyboard focus goes after the tile leaves: next card in the column, else the previous one, else its composer
  function neighbour(t) {
    var box = t && t.parentNode; if (!box) return null;
    var all = [].slice.call(box.querySelectorAll(':scope > .boards-tile:not(.is-pending)')), i = all.indexOf(t);
    var n = all[i + 1] || all[i - 1];
    if (n) return { tile: n.id };
    var col = t.closest('.boards-col'); return col ? { col: col.id } : null;
  }
  function focusNeighbour(nb) {
    if (!nb) return;
    if (nb.tile && focusTile(byId(nb.tile))) return;
    var col = nb.col && byId(nb.col), open = col && col.querySelector('.boards-add-open');
    if (open && open.offsetParent) open.focus({ preventScroll: true });
  }
  function refreshTile(id, hadFocus) { return B.refreshTile(id, hadFocus); }
  function tileHasFocus(t) { return !!(t && t.contains(document.activeElement)); }

  // ---------------------------------------------------------------- confirm (Delete) ----
  function confirmBox(o) {
    return W.confirm({
      title: o.title, text: o.text, ok: o.ok, danger: true, anchor: o.anchor,
      className: 'boards-confirm', onOk: o.onOk, onCancel: function () { B.announce('Cancelled'); }
    });
  }

  // ---------------------------------------------------------------- tile actions ----
  function archive(t, trash) {
    var id = t.dataset.id, nb = neighbour(t), had = tileHasFocus(t) || !!nb, n = others(t);
    t.classList.add('is-pending');
    B.post('/ui/cards/' + id + '/quick/archive', trash ? { trash: '1' } : {}).then(function (r) {
      t.classList.remove('is-pending');
      if (!r.ok) { B.toast(r.error || 'Could not change the card', { error: true }); return; }
      var d = r.trigger['boards:undo'] || { msg: trash ? 'Card moved to trash' : 'Card archived', url: '/ui/cards/' + id + '/quick/restore', id: id };
      d.refocus = true; d.msg += everywhere(n);
      document.dispatchEvent(new CustomEvent('boards:undo', { detail: d }));   // removes the tile, toasts Undo (boards_board.js)
      if (had) focusNeighbour(nb);
    });
  }
  function deleteDraft(t) {
    var id = t.dataset.id, nb = neighbour(t), col = t.closest('.boards-col');
    B.post('/ui/cards/' + id + '/delete', {}).then(function (r) {
      if (!r.ok) { B.toast(r.error || 'Could not delete the draft', { error: true }); return; }
      t.remove(); if (col) B.recount(col);
      B.announce('Draft deleted'); B.toast('Draft deleted');
      focusNeighbour(nb);
    });
  }
  function askDelete(t) {
    var draft = t.classList.contains('boards-draft'), name = nameOf(t);
    confirmBox(draft ? { title: 'Delete this draft?', text: '“' + name + '” is deleted permanently. This cannot be undone.', ok: 'Delete', anchor: t, onOk: function () { deleteDraft(t); } }
      : { title: 'Move to trash?', text: trashText(name, others(t)), ok: 'Move to trash', anchor: t, onOk: function () { archive(t, true); } });
  }
  function closeToggle(t) {
    var id = t.dataset.id, closing = !t.classList.contains('is-closed'), nb = closing ? neighbour(t) : null, had = tileHasFocus(t);
    setClosed(id, closing).then(function (ok) {
      if (!ok) return;
      Promise.resolve(B.refresh(true)).then(function () {
        if (closing) { if (had || nb) focusNeighbour(nb); }
        else focusTile(byId('tile-' + id));
      });
      undoToast(closing ? 'Card closed' : 'Card re-opened', function () {
        setClosed(id, !closing).then(function (ok2) { if (ok2) Promise.resolve(B.refresh(true)).then(function () { focusTile(byId('tile-' + id)); }); });
      });
    });
  }
  function setClosed(id, closed) {
    // board: a card on several boards closes into THIS board's closing column (closed itself is per card)
    return B.post('/ui/cards/' + id + '/close', { closed: closed ? 'true' : 'false', _part: 'none', board: B.boardId() }).then(function (r) {
      if (!r.ok) B.toast(r.error || 'Could not change the card', { error: true });
      return r.ok;
    });
  }
  function undoToast(msg, undo) { B.announce(msg + '. Press Undo to revert.'); B.toast(msg, { action: 'Undo', onAction: undo }); }

  // time tracking: s starts, S stops (explicit; pressing s on a running card says so instead of stopping it)
  var ALREADY = { true: 'Time tracking is already running (Shift+S stops it)', false: 'Time tracking is not running (s starts it)' };
  function timer(id, start, after, running) {
    if (running === start) { B.toast(ALREADY[start]); return Promise.resolve(null); }   // the page already shows that state
    return B.post('/ui/cards/' + id + '/time/' + (start ? 'start' : 'stop'), {}).then(function (r) {
      if (r.ok) { var m = start ? 'Time tracking started' : 'Time tracking stopped'; B.announce(m); B.toast(m); }
      else if (r.status === 409) B.toast(ALREADY[start]);
      else B.toast(r.error || 'Could not change time tracking', { error: true });
      if (after) after(r);
      return r;
    });
  }

  // ---------------------------------------------------------------- pickers (#boards-pop) ----
  var pick = null;   // { id, kind }
  function openPicker(t, kind) {
    var p = pop(); if (!p) return;
    var id = t.dataset.id;
    B.closePop(false);
    var seq = B.bumpPopSeq ? B.bumpPopSeq() : 0;
    fetch('/ui/cards/' + id + '/quick/' + kind + B.boardQ(), { credentials: 'same-origin', headers: HDR }).then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); }).then(function (html) {
      if (B.popSeq && seq !== B.popSeq()) return;
      if (!document.contains(t)) t = byId('tile-' + id); if (!t) return;
      if (B.setPopTile) B.setPopTile(t);
      p.innerHTML = html; p.hidden = false; p._anchor = t; pick = { id: id, kind: kind };
      if (kind === 'due') fillDue(p);
      if (kind === 'labels' || kind === 'members') number(p);
      place(p);
      var f = (kind === 'labels' || kind === 'members') ? (p.querySelector('.boards-kopt') || p.querySelector('[data-kpick-filter]'))
        : kind === 'due' ? p.querySelector('.boards-kopt') : p.querySelector('input[name=locationName]');
      (f || p.querySelector('.boards-kpick')).focus({ preventScroll: true });   // (a board without labels/members: the picker itself)
    }).catch(function () { if (B.popSeq && seq !== B.popSeq()) return; B.toast('Could not open the picker', { error: true }); });
  }
  function place(p) {
    var a = p._anchor; if (a && !document.contains(a)) a = p._anchor = byId(a.id);   // the tile was re-rendered (refreshTile, live merge)
    if (a) W.place(p, a, { align: 'left', cover: true });
  }
  function closePicker() { pick = null; B.closePop(); }
  function curPicker() { var p = pop(); return p && !p.hidden ? p.querySelector('.boards-kpick') : null; }
  function visibleOpts(box) { return [].slice.call(box.querySelectorAll('.boards-kopt')).filter(function (o) { return !o.hidden; }); }
  function number(box) {
    visibleOpts(box).forEach(function (o, i) { var k = o.querySelector('.boards-kn'); if (k) k.textContent = i < 9 ? String(i + 1) : ''; });
    box.querySelectorAll('.boards-kopt[hidden] .boards-kn').forEach(function (k) { k.textContent = ''; });
  }
  function filterOpts(box) {
    var f = box.querySelector('[data-kpick-filter]'), q = f ? f.value.trim().toLowerCase() : '';
    var n = 0;
    box.querySelectorAll('.boards-kopt').forEach(function (o) { var hide = !!q && (o.dataset.text || '').toLowerCase().indexOf(q) < 0; o.hidden = hide; if (!hide) n++; });
    var none = box.querySelector('.boards-kpick-none'); if (none) none.hidden = n > 0;
    number(box);
  }
  function toggleLabel(box, opt) {
    var id = box.dataset.card, on = opt.getAttribute('aria-pressed') !== 'true', label = opt.dataset.label, name = (opt.dataset.text || 'label');
    var f = box.querySelector('[data-kpick-filter]'), q = f ? f.value : '', focusedFilter = document.activeElement === f;
    opt.disabled = true;
    B.post('/ui/cards/' + id + '/quick/label', { labelId: label, on: on ? '1' : '0', _part: 'labels', board: B.boardId() }).then(function (r) {
      var p = pop();
      if (!r.ok) { opt.disabled = false; B.toast(r.error || 'Could not change the labels', { error: true }); return; }
      if (!p || p.hidden || !pick || pick.id !== id) return;
      p.innerHTML = r.html;                                   // single-select groups may have dropped a sibling label: re-render
      var nb = p.querySelector('.boards-kpick'); if (!nb) return;
      var nf = nb.querySelector('[data-kpick-filter]'); if (nf) nf.value = q;
      filterOpts(nb); place(p);
      if (focusedFilter && nf) nf.focus({ preventScroll: true });
      else { var same = nb.querySelector('.boards-kopt[data-label="' + esc(label) + '"]'); (same || nb.querySelector('.boards-kopt') || nf).focus({ preventScroll: true }); }
      B.announce('Label ' + name + (on ? ' added' : ' removed'));
      refreshTile(id, false);
    });
  }
  function toggleMember(box, opt) {
    var id = box.dataset.card, on = opt.getAttribute('aria-pressed') !== 'true', member = opt.dataset.member, name = (opt.dataset.name || opt.dataset.text || 'member');
    var f = box.querySelector('[data-kpick-filter]'), q = f ? f.value : '', focusedFilter = document.activeElement === f;
    opt.disabled = true;
    B.post('/ui/cards/' + id + '/quick/member', { userId: member, on: on ? '1' : '0', board: B.boardId() }).then(function (r) {
      var p = pop();
      if (!r.ok) { opt.disabled = false; B.toast(r.error || 'Could not change the members', { error: true }); return; }
      if (!p || p.hidden || !pick || pick.id !== id) return;
      p.innerHTML = r.html;
      var nb = p.querySelector('.boards-kpick'); if (!nb) return;
      var nf = nb.querySelector('[data-kpick-filter]'); if (nf) nf.value = q;
      filterOpts(nb); place(p);
      if (focusedFilter && nf) nf.focus({ preventScroll: true });
      else { var same = nb.querySelector('.boards-kopt[data-member="' + esc(member) + '"]'); (same || nb.querySelector('.boards-kopt') || nf).focus({ preventScroll: true }); }
      B.announce((on ? 'Assigned ' : 'Unassigned ') + name);
      refreshTile(id, false);
    });
  }
  function toggleOpt(box, opt) {
    if (box.dataset.kpick === 'members') toggleMember(box, opt);
    else toggleLabel(box, opt);
  }
  function pad(n) { return String(n).padStart(2, '0'); }
  function dueTarget(btn, cur) {
    var d = new Date();
    if (btn.dataset.dueMonths) d.setMonth(d.getMonth() + parseInt(btn.dataset.dueMonths, 10));
    else d.setDate(d.getDate() + parseInt(btn.dataset.dueDays || '0', 10));
    var tm = cur && cur.indexOf('T') > 0 ? cur.split('T')[1] : '17:00';   // keep the time of day, like the card sheet
    return { value: d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + 'T' + tm, date: d };
  }
  function fmtDay(d) { try { return d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' }); } catch (x) { return d.toDateString(); } }
  function fillDue(p) {
    var inp = p.querySelector('input[name=dueDate]'), cur = inp ? inp.value : '';
    p.querySelectorAll('.boards-kdue:not(.is-clear)').forEach(function (b) {
      var w = b.querySelector('[data-due-when]'); if (w) w.textContent = fmtDay(dueTarget(b, cur).date);
    });
  }
  function setDue(box, value, label) {
    var id = box.dataset.card;
    B.post('/ui/cards/' + id + '/due', { dueDate: value, _part: 'none' }).then(function (r) {
      if (!r.ok) { B.toast(r.error || 'Could not set the due date', { error: true }); return; }
      closePicker();
      var m = value ? 'Due date set' + (label ? ': ' + label : '') : 'Due date removed';
      B.announce(m); B.toast(m);
      refreshTile(id, true);
    });
  }
  function setPlace(box, vals) {
    var id = box.dataset.card;
    vals._part = 'none';
    B.post('/ui/cards/' + id + '/location', vals).then(function (r) {
      if (!r.ok) { B.toast(r.error || 'Could not set the location', { error: true }); return; }
      closePicker();
      var m = vals.clear ? 'Location removed' : 'Location set';
      B.announce(m); B.toast(m);
      refreshTile(id, true);
    });
  }

  document.addEventListener('click', function (e) {
    var box = e.target.closest && e.target.closest('#boards-pop .boards-kpick'); if (!box) return;
    var opt = e.target.closest('.boards-kopt');
    if (box.dataset.kpick === 'labels' && opt && opt.dataset.label) { e.preventDefault(); toggleLabel(box, opt); return; }
    if (box.dataset.kpick === 'members' && opt && opt.dataset.member) { e.preventDefault(); toggleMember(box, opt); return; }
    if (box.dataset.kpick === 'due' && opt) {
      e.preventDefault();
      if (opt.hasAttribute('data-due-clear')) { setDue(box, '', ''); return; }
      var inp = box.querySelector('input[name=dueDate]'), t = dueTarget(opt, inp ? inp.value : '');
      setDue(box, t.value, fmtDay(t.date)); return;
    }
    if (e.target.closest('[data-place-clear]')) { e.preventDefault(); setPlace(box, { clear: 'true' }); }
  });
  document.addEventListener('submit', function (e) {
    var f = e.target; if (!f.matches || !f.matches('#boards-pop form[data-kpick-form]')) return;
    e.preventDefault();
    var box = f.closest('.boards-kpick'); if (!box || !f.reportValidity()) return;
    if (box.dataset.kpick === 'due') { var v = f.elements.dueDate.value; setDue(box, v, v ? fmtDay(new Date(v)) : ''); }
    else setPlace(box, { locationName: f.elements.locationName.value, lat: f.elements.lat.value, lng: f.elements.lng.value });
  });
  document.addEventListener('input', function (e) {
    if (e.target.matches && e.target.matches('#boards-pop [data-kpick-filter]')) filterOpts(e.target.closest('.boards-kpick'));
  });
  // keys inside a picker (Esc is handled by boards_keys.js: it closes #boards-pop and puts focus back on the tile)
  document.addEventListener('keydown', function (e) {
    var box = e.target.closest && e.target.closest('#boards-pop .boards-kpick'); if (!box || e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
    var k = e.key, opts = visibleOpts(box), i = opts.indexOf(document.activeElement), filter = box.querySelector('[data-kpick-filter]');
    var inField = W.typing(e.target);
    if (k === 'Tab') { W.trapTab(box, e); return; }
    if (k === 'ArrowDown' || k === 'ArrowUp') {
      if (inField && e.target !== filter) return;   // date / number inputs use their arrows
      if (!opts.length) return;
      e.preventDefault();
      if (e.target === filter) { if (k === 'ArrowDown') opts[0].focus(); return; }
      opts[(i + (k === 'ArrowDown' ? 1 : -1) + opts.length) % opts.length].focus();
      return;
    }
    if (inField) {
      if (k === 'Enter' && e.target === filter) { e.preventDefault(); if (opts[0]) toggleOpt(box, opts[0]); }
      return;
    }
    if ((k === 'Home' || k === 'End') && opts.length) { e.preventDefault(); opts[k === 'Home' ? 0 : opts.length - 1].focus(); return; }
    if (box.dataset.kpick === 'labels' || box.dataset.kpick === 'members') {
      if (/^[1-9]$/.test(k)) { e.preventDefault(); var o = opts[+k - 1]; if (o) toggleOpt(box, o); return; }
      if (filter && (k === 'Backspace' || (k.length === 1 && k !== ' '))) {   // typing on an option goes to the filter
        e.preventDefault();
        filter.value = k === 'Backspace' ? filter.value.slice(0, -1) : filter.value + k;
        filter.focus(); filterOpts(box);
      }
      return;
    }
    if (box.dataset.kpick === 'due') {
      var btn = null;
      if (/^[1-4]$/.test(k)) btn = box.querySelectorAll('.boards-kdue:not(.is-clear)')[+k - 1];
      else if (k === '0') btn = box.querySelector('.boards-kdue.is-clear');
      if (btn) { e.preventDefault(); btn.click(); }
    }
  });
  // a live refresh may re-render the tile under an open picker: keep the anchor current
  window.addEventListener('resize', function () { var p = pop(); if (p && !p.hidden && curPicker()) place(p); });

  // ---------------------------------------------------------------- sheet actions ----
  function sheetToast(msg) { B.announce(msg); B.toast(msg); }
  function sheetReload(cm, url, vals) {
    // re-render the sheet through htmx so its section stash / popover logic (cards_ui.js) applies
    // the sheet's context board rides along (a card on several boards re-renders on the board the sheet shows)
    if (cm && cm.dataset.board && vals && !vals.board) vals.board = cm.dataset.board;
    return window.htmx ? htmx.ajax('POST', url, { target: '#card-modal', swap: 'outerHTML', values: vals }) : Promise.resolve();
  }
  function sheetKey(cm, e) {
    var id = cm.dataset.card, k = e.key;
    var draft = !!cm.dataset.draft || !!cm.dataset.template, lt = cm.dataset.listType || '', gone = lt === 'archive' || lt === 'trash';
    var anchor = cm.querySelector('#card-title') || cm;
    switch (k) {
      case 'x':
        if (draft) return false;
        if (gone) { B.toast('Restore the card before closing it'); return true; }
        var closing = !cm.dataset.closed;
        sheetReload(cm, '/ui/cards/' + id + '/close', { closed: closing ? 'true' : 'false' }).then(function () {
          undoToast(closing ? 'Card closed' : 'Card re-opened', function () {
            var cur = sheet();
            if (cur && cur.dataset.card === id) sheetReload(cur, '/ui/cards/' + id + '/close', { closed: closing ? 'false' : 'true' });
            else setClosed(id, !closing).then(function () { B.refresh(true); });
          });
        });
        return true;
      case 'A':
        if (draft) return false;
        if (gone) { B.toast(lt === 'trash' ? 'The card is in the trash' : 'The card is already archived'); return true; }
        sheetArchive(id, false, others(cm));
        return true;
      case 'Delete': case 'Backspace':
        if (e.repeat) return true;
        if (draft || lt === 'trash') {
          confirmBox({ title: cm.dataset.template ? 'Delete this template?' : draft ? 'Delete this draft?' : 'Delete permanently?', text: 'The card is deleted permanently. This cannot be undone.', ok: 'Delete', anchor: anchor, onOk: function () {
            B.post('/ui/cards/' + id + '/delete', {}).then(function (r) {
              if (!r.ok) { B.toast(r.error || 'Could not delete the card', { error: true }); return; }
              if (window.CardSheet) CardSheet.close();
              sheetToast('Card deleted'); B.refresh(true);
            });
          } });
          return true;
        }
        if (lt === 'archive') { B.toast('Restore the card before moving it to the trash'); return true; }
        confirmBox({ title: 'Move to trash?', text: others(cm) ? 'The card is also on ' + others(cm) + ' other board' + (others(cm) === 1 ? '' : 's') + '; it goes to the trash on all of them. You can undo or restore it from there.' : 'The card goes to the board’s trash. You can undo or restore it from there.', ok: 'Move to trash', anchor: anchor, onOk: function () { sheetArchive(id, true, others(cm)); } });
        return true;
      case 's': case 'S':
        if (draft) return false;
        var xt = cm.querySelector('#x-time .x-timer');
        timer(id, k === 's', function (r) {
          var x = cm.querySelector('#x-time');
          if (r.ok && x && document.contains(x) && window.htmx) htmx.swap(x, r.html, { swapStyle: 'outerHTML' });
        }, xt ? xt.classList.contains('is-running') : undefined);
        return true;
    }
    return false;
  }
  function sheetArchive(id, trash, n) {
    B.post('/ui/cards/' + id + '/quick/archive', trash ? { trash: '1' } : {}).then(function (r) {
      if (!r.ok) { B.toast(r.error || 'Could not change the card', { error: true }); return; }
      if (window.CardSheet) CardSheet.close();
      var d = r.trigger['boards:undo'] || { msg: trash ? 'Card moved to trash' : 'Card archived', url: '/ui/cards/' + id + '/quick/restore', id: id };
      d.refocus = true; d.msg += everywhere(n);
      document.dispatchEvent(new CustomEvent('boards:undo', { detail: d }));
    });
  }

  // ---------------------------------------------------------------- the key handler ----
  var KEYS = { L: 1, m: 1, d: 1, p: 1, s: 1, S: 1, x: 1, A: 1, Delete: 1, Backspace: 1 };
  document.addEventListener('keydown', function (e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.isComposing || !KEYS[e.key]) return;
    if (W.typing(e.target) || confirmOpen()) return;
    var cm = sheet();
    if (cm) {
      if (sheetBusy(cm) || e.key === 'L' || e.key === 'm' || e.key === 'd' || e.key === 'p') return;   // the sheet's own popovers (cards_ui.js)
      var tag = e.target && e.target.tagName; if (tag === 'SUMMARY' && e.key === ' ') return;
      if (!cm.dataset.canEdit) { readOnly(); e.preventDefault(); return; }
      if (sheetKey(cm, e)) e.preventDefault();
      return;
    }
    if (byId('modal') && byId('modal').open) return;          // another dialog (agent prompt...) is open
    if (popOpen() || menusOpen() || (B.grab && B.grab.active)) return;
    var t = B.targetTile && B.targetTile(); if (!t || !document.contains(t) || t.classList.contains('is-pending')) return;
    if (!B.canEdit()) { readOnly(); e.preventDefault(); return; }
    var k = e.key;
    if (t.classList.contains('boards-draft')) {                 // Inbox drafts: only Delete makes sense (they are not on the board yet)
      if ((k === 'Delete' || k === 'Backspace') && !e.repeat) { e.preventDefault(); askDelete(t); }
      return;
    }
    e.preventDefault();
    if (e.repeat && (k === 'Delete' || k === 'Backspace' || k === 'A' || k === 'x')) return;
    switch (k) {
      case 'L': openPicker(t, 'labels'); break;
      case 'm': openPicker(t, 'members'); break;
      case 'd': openPicker(t, 'due'); break;
      case 'p': openPicker(t, 'place'); break;
      case 's': case 'S':
        var id = t.dataset.id, had = tileHasFocus(t);
        timer(id, k === 's', function (r) { if (r.ok) refreshTile(id, had); }, !!t.querySelector('.boards-timer'));
        break;
      case 'x': closeToggle(t); break;
      case 'A': archive(t, false); break;
      case 'Delete': case 'Backspace': askDelete(t); break;
    }
  });
})();
