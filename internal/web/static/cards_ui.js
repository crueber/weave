// Card UI glue: the card sheet (popovers, collapsible sections, tabs, shortcuts, add-flows, attachments by
// drop/paste), markdown toolbar, modal close after delete, calendar drag-to-reschedule.
(function () {
  if (window.__cardsUI) return;
  window.__cardsUI = true;

  function lsGet(k) { try { return localStorage.getItem(k); } catch (x) { return null; } }
  function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (x) { /* private mode */ } }
  function typing(el) { return !!(el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName))); }
  function root() { return document.getElementById('card-modal'); }
  function modal() { return document.getElementById('modal'); }
  function cardInModal() { var c = root(); return c ? c.dataset.card : ''; }
  function tileOf(id) { return document.getElementById('tile-' + id); }
  function sheetActive() { var cm = root(); if (!cm) return false; var d = cm.closest('dialog'); return !d || d.open; }
  function toast(msg, err) { if (window.Boards && Boards.toast) Boards.toast(msg, { error: !!err }); }

  // ---- markdown-lite toolbar: data-md="pre|post" wraps the selection, data-md-line prefixes lines ----
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('[data-md],[data-md-line]');
    if (!b) return;
    var ta = b.closest('form').querySelector('textarea');
    if (!ta) return;
    var s = ta.selectionStart, en = ta.selectionEnd, v = ta.value, sel = v.slice(s, en);
    if (b.dataset.md) {
      var parts = b.dataset.md.split('|');
      ta.value = v.slice(0, s) + parts[0] + sel + parts[1] + v.slice(en);
      ta.selectionStart = s + parts[0].length; ta.selectionEnd = s + parts[0].length + sel.length;
    } else {
      var pre = b.dataset.mdLine, ls = v.lastIndexOf('\n', s - 1) + 1;
      var block = v.slice(ls, en || s).split('\n').map(function (l) { return pre + l; }).join('\n');
      ta.value = v.slice(0, ls) + block + v.slice(en || s);
    }
    ta.focus();
    ta.dispatchEvent(new Event('input', { bubbles: true }));
  });

  // ---- after deleting a card: close the dialog, or leave the full-page view ----
  document.body.addEventListener('cardsClosed', function (e) {
    var m = modal();
    if (m && m.open) { m.close(); document.body.dispatchEvent(new CustomEvent('cardsRefresh')); return; }
    var board = e.detail && e.detail.boardId;
    if (board) location.href = '/boards/' + board;
  });
  document.body.addEventListener('cardCreated', function () { document.body.dispatchEvent(new CustomEvent('cardsRefresh')); });

  // ---- calendar: drag a chip onto a day cell to set the due date (time of day is kept) ----
  var dragged = null;
  document.addEventListener('dragstart', function (e) {
    var chip = e.target.closest && e.target.closest('.cards-cal-chip[data-card]');
    if (chip) { dragged = chip.dataset.card; e.dataTransfer.setData('text/plain', dragged); e.dataTransfer.effectAllowed = 'move'; }
  });
  document.addEventListener('dragover', function (e) {
    var cell = dragged && e.target.closest && e.target.closest('.cards-cal-cell');
    if (cell) { e.preventDefault(); cell.classList.add('cards-cal-over'); }
  });
  document.addEventListener('dragleave', function (e) {
    var cell = e.target.closest && e.target.closest('.cards-cal-cell');
    if (cell) cell.classList.remove('cards-cal-over');
  });
  document.addEventListener('drop', function (e) {
    var cell = dragged && e.target.closest && e.target.closest('.cards-cal-cell');
    if (!cell) return;
    e.preventDefault();
    var id = dragged; dragged = null;
    cell.classList.remove('cards-cal-over');
    htmx.ajax('POST', '/ui/cards/' + id + '/due', { target: 'body', swap: 'none', values: { dueDate: cell.dataset.date, _part: 'none' } })
      .then(function () {
        htmx.ajax('GET', location.pathname + location.search, { target: '#cards-cal', select: '#cards-cal', swap: 'outerHTML' });
      });
  });
  document.addEventListener('dragend', function () { dragged = null; });

  // =====================================================================================
  // The card sheet
  // =====================================================================================
  var st = { pushed: false, id: '', fromPop: false, reopen: null, stash: null, stashCard: '', focusTask: '', escAt: 0, drafts: {} };

  function siblings() {
    var t = tileOf(st.id); if (!t) return null;
    var box = t.closest('.boards-cards, .boards-grid'); if (!box) return null;
    var list = [].slice.call(box.querySelectorAll('.boards-tile:not(.is-pending)'));
    return { list: list, i: list.indexOf(t) };
  }
  function updateNav() {
    var s = siblings(), p = document.querySelector('[data-card-nav=prev]'), n = document.querySelector('[data-card-nav=next]');
    if (p) p.disabled = !s || s.i <= 0;
    if (n) n.disabled = !s || s.i < 0 || s.i >= s.list.length - 1;
  }
  function go(dir) { var s = siblings(); if (!s) return; var t = s.list[s.i + dir]; if (t) CardSheet.open(t.dataset.id, { replace: true }); }
  function flashSaved(msg) { var el = document.getElementById('cards-saved'); if (!el) return; el.textContent = ''; setTimeout(function () { el.textContent = msg || 'Saved'; }, 20); }

  var CardSheet = window.CardSheet = {
    open: function (id, o) {
      o = o || {};
      if (!window.htmx) { location.href = '/cards/' + id; return; }
      st.fromPop = !!o.fromPop; st.nav = !!o.replace;
      htmx.ajax('GET', '/ui/cards/' + id, { target: '#modal', swap: 'innerHTML' });
    },
    close: function () { var m = modal(); if (m && m.open) m.close(); }
  };

  document.body.addEventListener('htmx:afterSwap', function (e) {
    var m = modal(); if (!m || e.detail.target !== m) return;
    var id = cardInModal(); if (!id) return;
    if (!m.open) m.showModal();
    m.setAttribute('tabindex', '-1');
    if (st.fromPop) { st.pushed = true; }
    else if (location.pathname !== '/cards/' + id) {
      // one history entry per sheet session: further swaps (next/prev, duplicate, publish...) replace it
      if (st.pushed) history.replaceState({ card: id }, '', '/cards/' + id);
      else { history.pushState({ card: id }, '', '/cards/' + id); st.pushed = true; }
    }
    st.fromPop = false; st.nav = false; st.id = id;
    if (!m.contains(document.activeElement) || document.activeElement === m) m.focus({ preventScroll: true });
    updateNav();
  });
  // closing (Esc, X, backdrop): restore URL, refresh that tile, put focus back on it
  function onClose() {
    var m = modal();
    var id = st.id; st.id = '';
    if (st.pushed) { st.pushed = false; history.back(); }
    m.innerHTML = '';
    var B = window.Boards;
    if (!B || !id) return;
    if (!tileOf(id)) { B.refresh && B.refresh(true); return; }
    B.refreshTile(id, true);
  }
  if (modal()) modal().addEventListener('close', onClose);
  window.addEventListener('popstate', function (e) {
    var m = modal();
    if (m && m.open) { st.pushed = false; m.close(); }
    else if (e.state && e.state.card && window.Boards) CardSheet.open(e.state.card, { fromPop: true });
  });

  // ---- popovers: <details class="cards-pop" data-pop> (rail, list switcher, menu) and <details class="x-pop"> (fragments) ----
  function openPops() { var cm = root(); return cm ? [].slice.call(cm.querySelectorAll('details.cards-pop[open], details.x-pop[open]')) : []; }
  function closePops(except) { openPops().forEach(function (d) { if (d !== except && !d.contains(except)) d.removeAttribute('open'); }); }
  function focusIn(d) {
    var f = d.querySelector('.cm-filter, input:not([type=hidden]):not([type=checkbox]), select, textarea, button:not([disabled]), a[href]');
    if (f) f.focus({ preventScroll: true });
  }
  function popByName(name) { var cm = root(); return cm ? cm.querySelector('details.cards-pop[data-pop="' + name + '"]') : null; }
  function openPop(name) {
    var d = popByName(name); if (!d) return false;
    closePops(d); d.open = true;
    if (d.scrollIntoView && window.matchMedia && matchMedia('(max-width:760px)').matches) d.scrollIntoView({ block: 'nearest', inline: 'center' });
    focusIn(d);
    return true;
  }
  function togglePop(name) { var d = popByName(name); if (!d) return; if (d.open) { d.open = false; d.querySelector('summary').focus(); } else openPop(name); }
  // exclusive popovers + focus the first control when one opens ('toggle' does not bubble: capture)
  document.addEventListener('toggle', function (e) {
    var d = e.target; if (!d || d.tagName !== 'DETAILS' || !d.open) return;
    if (!(d.classList.contains('cards-pop') || d.classList.contains('x-pop')) || !d.closest('#card-modal')) return;
    closePops(d);
    if (!d.contains(document.activeElement) || document.activeElement === d.querySelector('summary')) focusIn(d);
  }, true);

  document.addEventListener('click', function (e) {
    var cm = root();
    var m = modal();
    if (m && m.open && e.target === m) { // backdrop (the dialog element itself is only hit outside its box)
      var r = m.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) { m.close(); return; }
    }
    if (!cm || !sheetActive() || !cm.contains(e.target) && !e.target.closest('[data-card-nav]')) return;
    var t = e.target;
    var nav = t.closest('[data-card-nav]');
    if (nav && !nav.disabled) { go(nav.dataset.cardNav === 'next' ? 1 : -1); return; }
    var op = t.closest('[data-open-pop]');
    if (op) { closePops(); openPop(op.dataset.openPop); return; }
    if (t.closest('[data-open-help]')) { closePops(); showHelp(true); return; }
    var sc = t.closest('[data-qa-scroll]');
    if (sc) { closePops(); focusSection(sc.dataset.qaScroll); return; }
    var tg = t.closest('[data-sec-toggle]');
    if (tg) { toggleSection(tg.closest('.cm-sec')); return; }
    var tab = t.closest('[data-tab]');
    if (tab) { selectTab(tab.dataset.tab, true); return; }
    var dp = t.closest('[data-due-preset]');
    if (dp) { dueFromPreset(dp); return; }
    // menu items close the menu (the sheet re-renders when the action needs it to)
    var mi = t.closest('.cm-menu-panel .cm-mi');
    if (mi) { var mn = mi.closest('details'); setTimeout(function () { if (mn) mn.removeAttribute('open'); }, 0); return; }
    if (!t.closest('details.cards-pop, details.x-pop, #cm-help')) { closePops(); showHelp(false); }
  });
  // popovers stay open across the re-render that follows each multi-select action (members, labels)
  var KEEP = { members: 1, labels: 1 };
  document.body.addEventListener('htmx:beforeSwap', function (e) {
    var t = e.detail.target; if (!t || t.id !== 'card-modal') return;
    var o = t.querySelector('details.cards-pop[open]');
    var idx = -1;
    if (o && KEEP[o.dataset.pop]) {
      var opts = [].slice.call(o.querySelectorAll('.cm-opt, .cm-labopt'));
      idx = opts.indexOf(document.activeElement);
      st.reopen = { name: o.dataset.pop, idx: idx, filter: (o.querySelector('.cm-filter') || {}).value || '' };
    } else st.reopen = null;
    // keep the (already loaded) sections visible during the re-render so nothing flashes to a skeleton
    st.stash = { scroll: modal() && modal().open ? modal().scrollTop : window.scrollY, html: {} };
    st.stashCard = t.dataset.card;
    t.querySelectorAll('[hx-get]').forEach(function (el) {
      if (el.querySelector('.cards-skel') || !el.children.length) return;
      st.stash.html[el.getAttribute('hx-get')] = el.innerHTML;
    });
  });
  document.body.addEventListener('htmx:afterSwap', function (e) {
    var t = e.detail.target; if (!t || t.id !== 'card-modal') return;
    var cm = root(); if (!cm || !st.stash || cm.dataset.card !== st.stashCard) { st.stash = null; return; }
    cm.querySelectorAll('[hx-get]').forEach(function (el) {
      var h = st.stash.html[el.getAttribute('hx-get')];
      if (h && el.querySelector('.cards-skel')) { el.innerHTML = h; if (window.htmx) htmx.process(el); }
    });
    var m = modal(); if (m && m.open) m.scrollTop = st.stash.scroll;
    st.stash = null;
    if (st.reopen) {
      var d = popByName(st.reopen.name);
      if (d) {
        d.open = true;
        var f = d.querySelector('.cm-filter');
        if (f && st.reopen.filter) { f.value = st.reopen.filter; filterOpts(f); }
        var opts = [].slice.call(d.querySelectorAll('.cm-opt, .cm-labopt')).filter(function (o) { return !o.closest('[hidden]'); });
        var target = opts[st.reopen.idx];
        if (target) target.focus({ preventScroll: true });
      }
      st.reopen = null;
    }
    flashSaved('Saved');
    updateNav();
  });

  // ---- popover helpers: list filter, due presets ----
  function filterOpts(input) {
    var q = input.value.trim().toLowerCase();
    input.closest('.cards-popbody').querySelectorAll('[data-filter-text]').forEach(function (r) {
      r.hidden = !!q && r.dataset.filterText.toLowerCase().indexOf(q) < 0;
    });
  }
  document.addEventListener('input', function (e) { if (e.target.matches && e.target.matches('[data-filter]')) filterOpts(e.target); });
  function dueFromPreset(btn) {
    var form = btn.closest('form'), inp = form.querySelector('input[type=datetime-local]');
    var d = new Date(); d.setDate(d.getDate() + parseInt(btn.dataset.duePreset, 10));
    var cur = inp.value && inp.value.indexOf('T') > 0 ? inp.value.split('T')[1] : '17:00';
    var p = function (n) { return String(n).padStart(2, '0'); };
    inp.value = d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + cur;
    form.requestSubmit();
  }

  // ---- collapsible sections (state remembered per section name) ----
  function secKey(n) { return 'weave.card.sec.' + n; }
  function applySecState() {
    var cm = root(); if (!cm) return;
    cm.querySelectorAll('.cm-sec[data-section]').forEach(function (s) {
      var c = lsGet(secKey(s.dataset.section)) === '1';
      s.classList.toggle('is-collapsed', c);
      s.querySelectorAll('[data-sec-toggle]').forEach(function (b) {
        b.setAttribute('aria-expanded', c ? 'false' : 'true');
        if (b.classList.contains('cm-secbtn')) b.setAttribute('aria-label', (c ? 'Expand' : 'Collapse') + ' discussion');
      });
    });
  }
  function toggleSection(s) {
    if (!s) return;
    var c = !s.classList.contains('is-collapsed');
    lsSet(secKey(s.dataset.section), c ? '1' : '0');
    applySecState();
  }
  function expandSection(name) {
    var s = root() && root().querySelector('.cm-sec[data-section="' + name + '"]');
    if (s && s.classList.contains('is-collapsed')) { lsSet(secKey(name), '0'); applySecState(); }
    return s;
  }
  function focusSection(name) {
    var tab = name === 'comments' || name === 'activity';
    var s = expandSection(tab ? 'discussion' : name);
    if (name === 'tasks') { if (focusTaskInput()) return; }
    if (name === 'attachments') { var fi = root().querySelector('[data-attach-input]'); if (fi) { s && s.scrollIntoView({ block: 'center', behavior: 'smooth' }); return; } }
    if (s) s.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }

  // ---- discussion tabs ----
  function selectTab(name, remember) {
    var cm = root(); if (!cm) return;
    cm.querySelectorAll('[data-tab]').forEach(function (b) {
      var on = b.dataset.tab === name;
      b.setAttribute('aria-selected', on ? 'true' : 'false'); b.tabIndex = on ? 0 : -1;
      var p = document.getElementById('cm-panel-' + b.dataset.tab); if (p) p.hidden = !on;
    });
    if (remember) lsSet('weave.card.tab', name);
  }
  document.addEventListener('keydown', function (e) {
    var tab = e.target.closest && e.target.closest('[data-tab]'); if (!tab) return;
    if (e.key !== 'ArrowRight' && e.key !== 'ArrowLeft') return;
    var all = [].slice.call(tab.parentNode.querySelectorAll('[data-tab]'));
    var next = all[(all.indexOf(tab) + (e.key === 'ArrowRight' ? 1 : all.length - 1)) % all.length];
    e.preventDefault(); selectTab(next.dataset.tab, true); next.focus();
  });
  function updateCounts() {
    var c = document.querySelector('#x-comments[data-count]'), t = document.querySelector('[data-count-for="comments"]');
    if (c && t) t.textContent = c.dataset.count;
  }

  // everything that must be re-applied whenever (part of) the sheet is (re)loaded
  function onSheetLoad() {
    var cm = root(); if (!cm) return;
    applySecState();
    var tab = lsGet('weave.card.tab'); if (tab === 'activity') selectTab('activity');
    updateCounts();
    var cb = cm.querySelector('[data-enter-send]'); if (cb) cb.checked = lsGet('weave.card.enterSend') === '1';
    var ta = cm.querySelector('[data-comment-input]');
    if (ta && !ta.value && st.drafts[cm.dataset.card]) { ta.value = st.drafts[cm.dataset.card]; grow(ta); }
  }
  document.body.addEventListener('htmx:load', function (e) {
    var cm = root(); if (!cm || !e.target || !(cm.contains(e.target) || e.target.contains(cm))) return;
    onSheetLoad();
    if (e.target.id === 'x-tasks' && st.focusTask) { var f = focusTaskInput(st.focusTask); if (f) st.focusTask = ''; }
  });
  document.addEventListener('DOMContentLoaded', onSheetLoad);
  if (document.readyState !== 'loading') onSheetLoad();

  // ---- comment composer: grows, Enter / Ctrl+Enter sends, clears and keeps focus ----
  function grow(ta) { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight + 2, 160) + 'px'; }
  document.addEventListener('input', function (e) {
    var ta = e.target; if (!ta.matches || !ta.matches('[data-comment-input]')) return;
    grow(ta); ta.form.classList.toggle('has-text', !!ta.value.trim());
    st.drafts[cardInModal()] = ta.value;
  });
  document.addEventListener('change', function (e) {
    if (e.target.matches && e.target.matches('[data-enter-send]')) lsSet('weave.card.enterSend', e.target.checked ? '1' : '0');
  });
  document.addEventListener('keydown', function (e) {
    var ta = e.target; if (!ta.matches || !ta.matches('[data-comment-input]')) return;
    if (e.key !== 'Enter' || e.isComposing) return;
    var enterSends = lsGet('weave.card.enterSend') === '1';
    if ((e.ctrlKey || e.metaKey) || (enterSends && !e.shiftKey)) {
      e.preventDefault();
      if (ta.value.trim()) ta.form.requestSubmit();
    }
  });
  document.body.addEventListener('htmx:beforeRequest', function (e) {
    var f = e.detail.elt;
    if (f && f.id === 'cm-comment-form' && !f.querySelector('textarea').value.trim()) e.preventDefault();
    if (f && f.matches && f.matches('[data-add-task]')) st.focusTask = f.dataset.addTask;
    else if (f && f.matches && f.matches('.x-newlist')) st.focusTask = 'new';
  });
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var f = e.detail.elt;
    if (f && f.id === 'cm-comment-form' && e.detail.successful) {
      var ta = f.querySelector('textarea'); f.reset(); ta.style.height = ''; f.classList.remove('has-text');
      delete st.drafts[cardInModal()];
      selectTab('comments', true); expandSection('discussion');
      ta.focus({ preventScroll: true });
      // the activity feed gained an entry
      var act = document.getElementById('cm-panel-activity');
      if (act && window.htmx) htmx.ajax('GET', act.getAttribute('hx-get'), { target: act, swap: 'innerHTML' });
    }
  });

  // ---- checklists: Enter keeps focus on the add-item input; pasting several lines adds several items ----
  function focusTaskInput(which) {
    var cm = root(); if (!cm) return false;
    var inp;
    if (which && which !== 'new') inp = cm.querySelector('[data-add-task="' + which + '"] [data-task-input]');
    else if (which === 'new') { var all = cm.querySelectorAll('[data-add-task] [data-task-input]'); inp = all[all.length - 1]; }
    else inp = cm.querySelector('[data-add-task] [data-task-input]') || cm.querySelector('.x-newlist [data-task-input]');
    if (!inp) { var ad = cm.querySelector('.x-addlist'); if (ad) { ad.open = true; focusIn(ad); return true; } return false; }
    expandSection('tasks');
    inp.focus({ preventScroll: false });
    if (inp.scrollIntoView) inp.scrollIntoView({ block: 'center', behavior: 'smooth' });
    return true;
  }
  document.addEventListener('paste', function (e) {
    var inp = e.target; if (!inp.matches || !inp.matches('[data-add-task] [data-task-input]')) return;
    var txt = (e.clipboardData && e.clipboardData.getData('text')) || '';
    var lines = txt.split(/\r?\n/).map(function (l) { return l.replace(/^\s*(?:[-*•]|\d+[.)])\s+(?:\[[ xX]?\]\s*)?/, '').trim(); }).filter(Boolean);
    if (lines.length < 2) return;
    e.preventDefault();
    var form = inp.form, url = form.getAttribute('hx-post'), extra = {};
    form.querySelectorAll('select').forEach(function (s) { if (s.value) extra[s.name] = s.value; });
    st.focusTask = form.dataset.addTask;
    lines.reduce(function (p, line) {
      return p.then(function () {
        var v = { name: line }; for (var k in extra) v[k] = extra[k];
        return htmx.ajax('POST', url, { target: '#x-tasks', swap: 'outerHTML', values: v });
      });
    }, Promise.resolve()).then(function () { toast(lines.length + ' items added'); });
  });

  // ---- attachments: file picker (several files), drop on the sheet, paste an image ----
  function postFile(id, f) {
    var fd = new FormData(); fd.append('file', f, f.name);
    return fetch('/ui/cards/' + id + '/attachments', { method: 'POST', body: fd, credentials: 'same-origin', headers: { 'HX-Request': 'true', 'X-Requested-With': 'weave' } })
      .then(function (r) { if (!r.ok) throw new Error('Could not attach ' + f.name); return r.text(); })
      .then(function (html) {
        var el = document.getElementById('x-attachments');
        if (el && window.htmx) htmx.swap(el, html, { swapStyle: 'outerHTML' });
      });
  }
  function upload(files) {
    var id = cardInModal(); if (!id || !files || !files.length) return;
    var list = [].slice.call(files), sv = document.getElementById('cards-saved');
    if (sv) sv.textContent = 'Uploading ' + list.length + (list.length > 1 ? ' files' : ' file') + '...';
    list.reduce(function (p, f) { return p.then(function () { return postFile(id, f); }); }, Promise.resolve())
      .then(function () { expandSection('attachments'); flashSaved(list.length > 1 ? 'Attached' : 'Attached'); })
      .catch(function (err) { if (sv) sv.textContent = ''; toast(err.message || 'Upload failed', true); });
  }
  document.addEventListener('change', function (e) {
    var inp = e.target; if (!inp.matches || !inp.matches('[data-attach-input]')) return;
    e.stopPropagation(); upload(inp.files); inp.value = '';
  }, true);
  function hasFiles(e) { var t = e.dataTransfer && e.dataTransfer.types; return !!t && [].indexOf.call(t, 'Files') >= 0; }
  var dropDepth = 0;
  function dropBox() { var cm = root(); return cm && cm.querySelector('.cm-drop'); }
  document.addEventListener('dragenter', function (e) {
    if (!sheetActive() || !hasFiles(e)) return;
    dropDepth++; var b = dropBox(); if (b && root().querySelector('[data-attach-input]')) b.hidden = false;
  });
  document.addEventListener('dragover', function (e) { if (sheetActive() && hasFiles(e)) e.preventDefault(); });
  document.addEventListener('dragleave', function (e) {
    if (!sheetActive() || !hasFiles(e)) return;
    dropDepth = Math.max(0, dropDepth - 1); if (!dropDepth) { var b = dropBox(); if (b) b.hidden = true; }
  });
  document.addEventListener('drop', function (e) {
    if (!sheetActive() || !hasFiles(e)) return;
    e.preventDefault(); dropDepth = 0; var b = dropBox(); if (b) b.hidden = true;
    if (root().querySelector('[data-attach-input]')) upload(e.dataTransfer.files);
  });
  document.addEventListener('paste', function (e) {
    if (!sheetActive() || !e.clipboardData || !e.clipboardData.files || !e.clipboardData.files.length) return;
    if (typing(e.target) && e.clipboardData.getData('text')) return;
    if (!root().querySelector('[data-attach-input]')) return;
    e.preventDefault();
    var stamp = new Date().toISOString().replace(/[:T]/g, '-').slice(0, 19);
    var files = [].map.call(e.clipboardData.files, function (f, i) {
      return /^image\//.test(f.type) && (!f.name || /^image\.\w+$/.test(f.name)) ? new File([f], 'pasted-' + stamp + (i ? '-' + i : '') + '.' + (f.type.split('/')[1] || 'png'), { type: f.type }) : f;
    });
    upload(files);
  });

  // ---- description: Ctrl+Enter saves; focus the editor when it opens; save when focus leaves it ----
  document.addEventListener('keydown', function (e) {
    var ta = e.target; if (!ta.matches || !ta.matches('#card-desc textarea')) return;
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); ta.form.requestSubmit(); }
  });
  document.body.addEventListener('htmx:afterSettle', function (e) {
    var d = document.getElementById('card-desc');
    if (!d || !e.target || (e.target !== d && !d.contains(e.target) && !e.target.contains(d))) return;
    var ta = d.querySelector('textarea');
    if (ta && document.activeElement !== ta && !ta.dataset.focused) { ta.dataset.focused = '1'; ta.focus(); ta.selectionStart = ta.selectionEnd = ta.value.length; }
  });
  document.addEventListener('focusout', function (e) {
    var ta = e.target; if (!ta.matches || !ta.matches('[data-autosave]')) return;
    var form = ta.closest('form'), id = cardInModal();
    setTimeout(function () {
      if (!document.contains(form) || form.contains(document.activeElement) || !window.htmx) return;
      if (ta.value !== ta.defaultValue) htmx.trigger(form, 'submit');
      else htmx.ajax('POST', '/ui/cards/' + id + '/editing/release', { target: '#card-desc', swap: 'outerHTML', values: { field: 'description' } });
    }, 0);
  });
  document.body.addEventListener('cards:saved', function () { flashSaved('Saved'); });
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var el = e.detail.elt;
    if (e.detail.successful && el && el.closest && (el.closest('#card-title') || (el.matches && el.matches('[data-autosave]')))) { if (!el.matches('[data-autosave]')) flashSaved('Saved'); }
  });
  // archiving / trashing from the sheet: close it and offer Undo instead of asking first
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var el = e.detail.elt, p = el && el.getAttribute && el.getAttribute('hx-post');
    var m = p && /^\/ui\/cards\/([^/]+)\/archive$/.exec(p);
    var B = window.Boards;
    if (!m || !e.detail.successful || !B) return;
    var id = m[1], trash = (el.getAttribute('hx-vals') || '').indexOf('true') > 0;
    CardSheet.close();
    B.toast(trash ? 'Card moved to trash' : 'Card archived', { action: 'Undo', onAction: function () {
      B.post('/ui/cards/' + id + '/quick/restore', {}).then(function () { B.refresh(true); });
    } });
  });

  // ---- shortcuts help ----
  function showHelp(on) {
    var h = document.getElementById('cm-help'); if (!h) return;
    var show = on === undefined ? h.hidden : on;
    h.hidden = !show; if (show) h.focus({ preventScroll: true });
  }

  // ---- Esc layers: help -> popover -> editor/title -> sheet ----
  function layerEsc() {
    var cm = root(); if (!cm) return false;
    var h = document.getElementById('cm-help');
    if (h && !h.hidden) { h.hidden = true; return true; }
    var open = openPops();
    if (open.length) {
      var last = open[open.length - 1]; last.removeAttribute('open');
      var sm = last.querySelector('summary'); if (sm) sm.focus({ preventScroll: true });
      return true;
    }
    var ae = document.activeElement;
    if (ae && ae.matches && ae.matches('#card-desc textarea')) {
      var form = ae.form;
      if (ae.value !== ae.defaultValue) htmx.trigger(form, 'submit');
      else htmx.ajax('POST', '/ui/cards/' + cardInModal() + '/editing/release', { target: '#card-desc', swap: 'outerHTML', values: { field: 'description' } });
      return true;
    }
    if (ae && ae.classList && ae.classList.contains('cards-title-input') && ae.value !== ae.defaultValue) { ae.value = ae.defaultValue; ae.blur(); return true; }
    return false;
  }
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape' || e.defaultPrevented || !sheetActive()) return;
    if (layerEsc()) { e.preventDefault(); e.stopPropagation(); st.escAt = Date.now(); }
  }, true);
  function onCancel(e) { if (Date.now() - st.escAt < 80 || layerEsc()) e.preventDefault(); }
  if (modal()) modal().addEventListener('cancel', onCancel);

  // ---- single-key shortcuts while the sheet is open (never while typing) ----
  document.addEventListener('keydown', function (e) {
    if (!sheetActive() || typing(e.target) || e.ctrlKey || e.metaKey || e.altKey || e.defaultPrevented) return;
    var tag = e.target && e.target.tagName; if (tag === 'SUMMARY' && (e.key === ' ' || e.key === 'Enter')) return;
    var k = e.key, cm = root();
    if (k === 'j' || k === 'ArrowRight') { if (k === 'ArrowRight' && e.target.closest && e.target.closest('[role=tablist]')) return; e.preventDefault(); go(1); }
    else if (k === 'k' || k === 'ArrowLeft') { if (k === 'ArrowLeft' && e.target.closest && e.target.closest('[role=tablist]')) return; e.preventDefault(); go(-1); }
    else if (k === 'm') { e.preventDefault(); togglePop('members'); }
    else if (k === 'l') { e.preventDefault(); togglePop('labels'); }
    else if (k === 'd') { e.preventDefault(); togglePop('due'); }
    else if (k === 'a') { e.preventDefault(); var fi = cm.querySelector('[data-attach-input]'); if (fi) fi.click(); }
    else if (k === 't') { e.preventDefault(); focusTaskInput(); }
    else if (k === 'c') { e.preventDefault(); expandSection('discussion'); var ci = cm.querySelector('[data-comment-input]'); if (ci) { selectTab('comments'); ci.focus(); } }
    else if (k === 'e') { var eb = cm.querySelector('[data-desc-edit]'); if (eb) { e.preventDefault(); eb.click(); } }
    else if (k === '?') { e.preventDefault(); showHelp(); }
  });
})();
