// Boards core: shared helpers (window.Boards), live refresh over SSE that never clobbers
// what the user is doing (per-column merge), toasts with Undo, collapse state.
(function () {
  if (window.Boards) { window.Boards.setup && window.Boards.setup(); return; }
  var B = window.Boards = { dragging: false };
  var esc = window.CSS && CSS.escape ? CSS.escape : function (s) { return String(s).replace(/[^\w-]/g, '\\$&'); };
  B.esc = esc;

  B.body = function () { return document.getElementById('board-body'); };
  B.lists = function () { return document.getElementById('board-lists'); };
  B.boardId = function () { var el = document.getElementById('board-live'); return (el && el.dataset.board) || ''; };
  B.canEdit = function () { var l = B.lists(); return !!(l && l.dataset.canEdit); };
  B.reduced = function () { return window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches; };

  // ---- announcements + toasts ----
  B.announce = function (msg) {
    var r = document.getElementById('boards-announce'); if (!r) return;
    r.textContent = ''; setTimeout(function () { r.textContent = msg; }, 30);
  };
  B.toast = function (msg, o) {
    o = o || {};
    var host = document.getElementById('boards-toasts'); if (!host) return;
    var t = document.createElement('div'); t.className = 'boards-toast' + (o.error ? ' is-error' : '');
    if (o.error) t.setAttribute('role', 'alert');
    var s = document.createElement('span'); s.textContent = msg; t.appendChild(s);
    function done() { clearTimeout(timer); t.remove(); }
    if (o.action) {
      var b = document.createElement('button'); b.type = 'button'; b.textContent = o.action;
      b.addEventListener('click', function () { done(); o.onAction && o.onAction(); });
      t.appendChild(b);
    }
    host.appendChild(t);
    var timer = setTimeout(done, o.ms || (o.action ? 6000 : 4000));
    return t;
  };

  // ---- tiny fetch helper that speaks htmx (so handlers answer with fragments) ----
  B.post = function (url, data, method) {
    var body = new URLSearchParams();
    Object.keys(data || {}).forEach(function (k) {
      [].concat(data[k]).forEach(function (v) { body.append(k, v); });
    });
    return fetch(url, { method: method || 'POST', credentials: 'same-origin', body: body,
      headers: { 'HX-Request': 'true', 'X-Requested-With': 'weave', 'Content-Type': 'application/x-www-form-urlencoded' } })
      .then(function (r) {
        return r.text().then(function (html) {
          var trig = {}; try { trig = JSON.parse(r.headers.get('HX-Trigger') || '{}'); } catch (e) { trig = {}; }
          var err = '';
          if (!r.ok) { var m = /<div[^>]*toast[^>]*>([^<]*)</.exec(html); err = (m && m[1]) || 'Something went wrong'; }
          if (r.headers.get('HX-Redirect')) { location.href = r.headers.get('HX-Redirect'); }
          return { ok: r.ok, status: r.status, html: html, trigger: trig, error: err };
        });
      }, function () { return { ok: false, status: 0, html: '', trigger: {}, error: 'You appear to be offline' }; });
  };
  B.fragment = function (html) { var t = document.createElement('template'); t.innerHTML = html.trim(); return t.content; };
  // refetch one tile (after quick edit / closing the card sheet) and put focus back on it when asked
  B.refreshTile = function (id, focus) {
    return fetch('/ui/cards/' + id + '/tile', { credentials: 'same-origin', headers: { 'HX-Request': 'true', 'X-Requested-With': 'weave' } })
      .then(function (r) { if (!r.ok) { B.refresh(true); return null; } return r.text(); })
      .then(function (html) {
        var cur = document.getElementById('tile-' + id); if (!cur) return;
        if (html) { var t = B.fragment(html).firstElementChild; if (t) { cur.replaceWith(t); B.process(t); cur = t; } }
        if (focus) { var l = cur.querySelector('.boards-tile-link'); if (l) l.focus({ preventScroll: true }); }
      });
  };
  B.process = function (el) { if (window.htmx && el) htmx.process(el); };

  // ---- collapse state (per browser) ----
  function store(k, v) { try { if (v === undefined) return JSON.parse(localStorage.getItem(k) || 'null'); localStorage.setItem(k, JSON.stringify(v)); } catch (e) { return null; } }
  var collapsed = store('weave.boards.collapsed') || {};
  B.setCollapsed = function (col, on) {
    col.classList.toggle('is-collapsed', on);
    var b = col.querySelector('[data-collapse]'); if (b) { b.setAttribute('aria-expanded', on ? 'false' : 'true'); b.setAttribute('aria-label', (on ? 'Expand' : 'Collapse') + ' list'); }
    if (on) collapsed[col.dataset.id] = 1; else delete collapsed[col.dataset.id];
    store('weave.boards.collapsed', collapsed);
  };
  B.initCol = function (col) {
    if (collapsed[col.dataset.id]) col.classList.add('is-collapsed');
    var b = col.querySelector('[data-collapse]'); if (b && collapsed[col.dataset.id]) b.setAttribute('aria-expanded', 'false');
    col.__sig = col.__sig || null;
  };
  B.recount = function (col) {
    var n = col.querySelectorAll('.boards-tile:not(.is-pending)').length, c = col.querySelector('[data-count]');
    if (c) c.textContent = n;
  };

  // ---- applying server state without clobbering the user ----
  function busyCol(col) {
    // an open list menu lives in #popover-root while open: it is keyed by list id (WeavePop registry), not by DOM position
    return !!col.querySelector('.boards-col-title-input, .boards-menu[open], .boards-tile.is-pending, .boards-tile.is-grabbed, [data-busy]') || col.classList.contains('is-origin') ||
      !!(window.WeavePop && WeavePop.isOpen(col.dataset.id));
  }
  function activeComposer(col) {
    var c = col.querySelector('.boards-composer');
    if (!c) return null;
    var ta = c.querySelector('textarea');
    return (c.classList.contains('is-open') || (ta && ta.value)) ? c : null;
  }
  // Replace col with its fresh server rendering, but carry over scroll position and a
  // live composer (with focus and caret) so typing is never interrupted.
  function mergeCol(old, fresh) {
    var imp = document.importNode(fresh, true);
    var comp = activeComposer(old), ae = document.activeElement, sel = null;
    if (comp) {
      var nc = imp.querySelector('.boards-composer');
      var top = comp.parentNode && comp.parentNode.classList.contains('boards-cards');
      if (ae && comp.contains(ae) && typeof ae.selectionStart === 'number') sel = [ae.selectionStart, ae.selectionEnd];
      if (nc) { if (top) { nc.remove(); imp.querySelector('.boards-cards').prepend(comp); } else nc.replaceWith(comp); }
    }
    var oc = old.querySelector('.boards-cards'), st = oc ? oc.scrollTop : 0;
    var tileFocus = ae && ae.closest && ae.closest('.boards-tile') && old.contains(ae) ? ae.closest('.boards-tile').dataset.id : '';
    imp.__sig = fresh.outerHTML;
    old.replaceWith(imp);
    var nc2 = imp.querySelector('.boards-cards'); if (nc2) nc2.scrollTop = st;
    B.initCol(imp); B.process(imp);
    if (comp && ae && comp.contains(ae)) { ae.focus({ preventScroll: true }); try { if (sel) ae.setSelectionRange(sel[0], sel[1]); else if (ae.value !== undefined) ae.setSelectionRange(ae.value.length, ae.value.length); } catch (e) { /* */ } }
    else if (tileFocus) { var l = imp.querySelector('#tile-' + esc(tileFocus) + ' .boards-tile-link'); if (l) l.focus({ preventScroll: true }); }
    return imp;
  }

  function sweepPops() { if (window.WeavePop) WeavePop.sweep(); }
  // column / list-menu owner of any element, also for a portaled menu popover
  B.colOf = function (el) { return window.WeavePop ? WeavePop.colOf(el) : (el && el.closest && el.closest('.boards-col')); };
  B.menuOf = function (el) { return window.WeavePop ? WeavePop.menuOf(el) : (el && el.closest && el.closest('details.boards-menu')); };
  function colsOf(box) { return [].slice.call(box.children).filter(function (c) { return c.classList.contains('boards-col'); }); }
  B.applyBody = function (html) {
    var doc = new DOMParser().parseFromString(html, 'text/html');
    var nb = doc.getElementById('board-body'), cur = B.body();
    if (!nb || !cur) return false;
    var curLists = B.lists(), newLists = nb.querySelector('.boards-lists');
    // keep nav links in sync (they carry the filter query)
    var nav = doc.querySelector('.boards-views'), curNav = document.querySelector('.boards-views');
    if (nav && curNav && nav.innerHTML !== curNav.innerHTML) curNav.innerHTML = nav.innerHTML;
    // active-filter chips
    var nchips = nb.querySelector('.boards-chips'), cchips = cur.querySelector('.boards-chips');
    if (cchips && nchips) { if (cchips.outerHTML !== nchips.outerHTML) cchips.replaceWith(document.importNode(nchips, true)); }
    else if (cchips) cchips.remove();
    else if (nchips) cur.prepend(document.importNode(nchips, true));
    if (!curLists || !newLists) { // list/grid view (or a view switch): plain swap
      if (B.dragging) return true;
      var imp = document.importNode(nb, true); cur.replaceWith(imp); B.process(imp); setup(); sweepPops(); return true;
    }
    var scrollX = curLists.scrollLeft;
    var oldCols = {}; colsOf(curLists).forEach(function (c) { oldCols[c.dataset.id] = c; });
    var prev = null, seen = {}, dirty = false;
    colsOf(newLists).forEach(function (nc) {
      var id = nc.dataset.id, old = oldCols[id], node;
      seen[id] = 1;
      if (!old) {
        node = document.importNode(nc, true); node.__sig = nc.outerHTML; B.initCol(node);
        node.classList.add('is-new'); curLists.insertBefore(node, prev ? prev.nextElementSibling : curLists.firstElementChild); B.process(node);
      } else if (busyCol(old)) { node = old; dirty = true; }
      else if (old.__sig === nc.outerHTML) node = old;
      else node = mergeCol(old, nc);
      var want = prev ? prev.nextElementSibling : curLists.firstElementChild;
      if (want !== node && !node.classList.contains('is-origin')) curLists.insertBefore(node, want);
      prev = node;
    });
    Object.keys(oldCols).forEach(function (id) { if (!seen[id] && !oldCols[id].classList.contains('is-origin')) oldCols[id].remove(); });
    curLists.scrollLeft = scrollX;
    sweepPops();
    return dirty ? 'dirty' : true;
  };

  var es = null, esBoard = null, timer = null;
  B.refresh = function (force) {
    var id = B.boardId(); if (!id || !B.body()) return;
    if (B.dragging) { B.schedule(1200); return; }
    if (force) document.querySelectorAll('.boards-col').forEach(function (c) { c.__sig = null; });
    fetch('/ui/boards/' + id + '/body' + location.search, { credentials: 'same-origin', headers: { 'HX-Request': 'true', 'X-Requested-With': 'weave' } })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(); })
      .then(function (html) { if (B.dragging) { B.schedule(1200); return; } if (B.applyBody(html) === 'dirty') B.schedule(2500); })
      .catch(function (err) { if (err && window.console) console.error('boards refresh failed', err); /* offline: the next event retries */ });
  };
  B.schedule = function (ms) { clearTimeout(timer); timer = setTimeout(B.refresh, ms || 300); };

  var verbs = ['created', 'updated', 'deleted', 'moved', 'claimed', 'released'];
  var nouns = ['board', 'list', 'card', 'label', 'boardMembership', 'cardMembership', 'cardLabel', 'task', 'taskList', 'comment', 'attachment', 'customFieldValue'];
  function setup() {
    document.querySelectorAll('.boards-col').forEach(function (c) { if (!c.__init) { c.__init = 1; c.__sig = c.outerHTML; B.initCol(c); } });
    var id = B.boardId();
    if (id === esBoard) return;
    if (es) { es.close(); es = null; }
    esBoard = id;
    if (!id || !window.EventSource) return;
    es = new EventSource('/events?rooms=board:' + encodeURIComponent(id));
    nouns.forEach(function (n) { verbs.forEach(function (v) { es.addEventListener(n + '.' + v, function () { B.schedule(300); }); }); });
    es.onmessage = function () { B.schedule(300); };
    ['listener.joined', 'listener.left'].forEach(function (n) { es.addEventListener(n, function () { B.schedule(300); }); });
  }
  B.setup = setup;
  document.addEventListener('DOMContentLoaded', setup);
  if (document.readyState !== 'loading') setup();

  document.addEventListener('boards:refresh', function () { B.schedule(50); });
  document.addEventListener('htmx:afterRequest', function (e) {
    if (e.target && e.target.matches && e.target.matches('[data-boards-refresh]') && e.detail.successful) B.schedule(50);
  });

  // The filter form swaps #board-body: route that through the merge so open composers survive.
  document.body.addEventListener('htmx:beforeSwap', function (e) {
    var t = e.detail.target;
    if (!t || t.id !== 'board-body' || !e.detail.serverResponse) return;
    if (B.applyBody(e.detail.serverResponse)) {
      e.detail.shouldSwap = false;
      var p = e.detail.pathInfo && (e.detail.pathInfo.finalRequestPath || e.detail.pathInfo.requestPath);
      if (p) try { history.replaceState(history.state, '', p); } catch (x) { /* */ }
    }
  });
  // other mutations answered with 4xx: show the message as a toast (htmx would swap by default)
  document.body.addEventListener('htmx:responseError', function (e) {
    var m = /<div[^>]*toast[^>]*>([^<]*)</.exec(e.detail.xhr.responseText || '');
    if (m && B.toast) { B.toast(m[1], { error: true }); }
  });

  // Saved filters always capture the live filter form (minus the view).
  document.addEventListener('htmx:configRequest', function (e) {
    var el = e.detail.elt;
    if (!el || !el.matches || !el.matches('[data-boards-save]')) return;
    var f = document.getElementById('board-filter'); if (!f) return;
    var p = new URLSearchParams(new FormData(f)); p.delete('view');
    e.detail.parameters['query'] = p.toString();
  });

  // Undo toast for forgiving archive; the server hands us the reverse op's URL.
  document.addEventListener('boards:undo', function (e) {
    var d = e.detail || {};
    if (d.id) { var tile = document.getElementById('tile-' + d.id); if (tile) { var col = tile.closest('.boards-col'); tile.remove(); col && B.recount(col); } }
    B.closePop && B.closePop();
    B.announce((d.msg || 'Archived') + '. Press Undo to restore.');
    B.toast(d.msg || 'Archived', { action: 'Undo', onAction: function () {
      B.post(d.url, {}).then(function (r) { if (r.ok) { B.announce('Restored'); B.refresh(true); } else B.toast(r.error, { error: true }); });
    } });
  });
  // list menu: move-cards target list excludes the list itself
  document.addEventListener('htmx:afterSwap', function (e) {
    var s = e.target; if (s && s.dataset && s.dataset.excludeList) { var o = s.querySelector('option[value="' + s.dataset.excludeList + '"]'); if (o) o.remove(); }
  });
  // submitting a list-menu action closes the menu so the refreshed column can apply
  document.body.addEventListener('htmx:beforeRequest', function (e) {
    var el = e.detail.elt, v = e.detail.requestConfig && e.detail.requestConfig.verb;
    if (!el || !el.closest || v === 'get') return;
    var d = B.menuOf(el); if (d) d.removeAttribute('open');
  });
  // close open list menus / filter dropdowns when clicking elsewhere
  document.addEventListener('pointerdown', function (e) {
    document.querySelectorAll('details.boards-menu[open], details.boards-dd[open]').forEach(function (d) { if (!d.contains(e.target) && !(d._pop && d._pop.contains(e.target))) d.removeAttribute('open'); });
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') document.querySelectorAll('details.boards-menu[open], details.boards-dd[open]').forEach(function (d) { d.removeAttribute('open'); var s = d.querySelector('summary'); s && s.focus(); });
  });
})();
