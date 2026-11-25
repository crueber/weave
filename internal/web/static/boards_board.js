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
  // ?board= for the card fragments of this page: a card on several boards renders as it is on THIS board
  B.boardQ = function () { var b = B.boardId(); return b ? '?board=' + encodeURIComponent(b) : ''; };
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
    return fetch('/ui/cards/' + id + '/tile' + B.boardQ(), { credentials: 'same-origin', headers: { 'HX-Request': 'true', 'X-Requested-With': 'weave' } })
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
    var w = col.querySelector('[data-wip]');
    if (w) { // soft WIP badge: total = cards in the column (unfiltered); n is the number shown
      var tot = Math.max(0, (parseInt(w.dataset.total, 10) || 0) + n - (parseInt(w.dataset.visible, 10) || 0));
      w.dataset.total = tot; w.dataset.visible = n;
      var lim = parseInt(w.dataset.wip, 10) || 0, over = tot > lim, wn = w.querySelector('[data-wip-n]');
      if (wn) wn.textContent = tot;
      var flag = w.querySelector('.boards-wip-flag');
      if (over && !flag) { flag = document.createElement('b'); flag.className = 'boards-wip-flag'; flag.setAttribute('aria-hidden', 'true'); flag.textContent = '!'; w.insertBefore(flag, w.firstChild); }
      else if (!over && flag) flag.remove();
      w.classList.toggle('is-over', over);
      var txt = tot + ' of ' + lim + ' cards, soft WIP limit' + (over ? ', over the limit' : '');
      w.setAttribute('aria-label', txt); w.title = txt;
    }
  };

  // ---- applying server state without clobbering the user ----
  function busyCol(col) {
    // the Inbox (boards_inbox.js) never re-renders under a capture in progress, not even with the caret carried over
    if (col.classList.contains('boards-inbox') && activeComposer(col)) return true;
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
    // keep the view links in sync (they carry the filter query): hrefs only, so the top-bar switcher and the phone picker both follow
    doc.querySelectorAll('a[data-view]').forEach(function (a) {
      var h = a.getAttribute('href');
      document.querySelectorAll('a[data-view="' + a.dataset.view + '"]').forEach(function (c) { if (c.getAttribute('href') !== h) c.setAttribute('href', h); });
    });
    // active-filter chips
    var nchips = nb.querySelector('.boards-chips'), cchips = cur.querySelector('.boards-chips');
    if (cchips && nchips) { if (cchips.outerHTML !== nchips.outerHTML) cchips.replaceWith(document.importNode(nchips, true)); }
    else if (cchips) cchips.remove();
    else if (nchips) cur.prepend(document.importNode(nchips, true));
    if (!curLists || !newLists) { // list/grid view (or a view switch): plain swap
      if (B.dragging) return true;
      var imp = document.importNode(nb, true); cur.replaceWith(imp); B.process(imp); setup(); sweepPops();
      document.dispatchEvent(new CustomEvent('boards:body-swap'));
      return true;
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
  function archiveView() { return document.querySelector('#archive-view[data-kind="archived"], #archive-view[data-kind="trash"]'); }
  function archiveMode() {
    var root = document.getElementById('board-shell');
    var show = new URL(location.href).searchParams.get('show');
    return !!archiveView() || show === 'archived' || show === 'trash' ||
      !!(root && (root.dataset.show === 'archived' || root.dataset.show === 'trash'));
  }
  function archiveBusy() {
    var a = archiveView();
    if (!a || B.dragging || document.querySelector('.htmx-request')) return true;
    var bar = a.querySelector('#archive-bulk-bar');
    if ((bar && !bar.hidden) || a.querySelector('input[data-archive-check="card"]:checked')) return true;
    var active = document.activeElement;
    if (active && (active.isContentEditable || /^(INPUT|SELECT|TEXTAREA)$/.test(active.tagName))) return true;
    if (active && active.closest && active.closest('.boards-composer')) return true;
    return Array.prototype.some.call(document.querySelectorAll('.boards-composer'), function (c) {
      var input = c.querySelector('textarea');
      return c.classList.contains('is-open') || !!(input && input.value);
    });
  }
  function archiveQuery(a) {
    var form = a && a.querySelector('#archive-filters'), params = new URLSearchParams(location.search);
    // A card sheet owns location while it is open; its archive form still holds the board's current query.
    if (form) {
      var data = new FormData(form);
      params = new URLSearchParams();
      ['q', 'labelId', 'memberId', 'fromListId', 'since', 'until', 'sort', 'dir', 'view'].forEach(function (key) {
        var value = data.get(key);
        if (value) params.set(key, value);
      });
      params.set('show', a.dataset.kind);
    } else if (a) params.set('show', a.dataset.kind);
    params.delete('cursor');
    return params.toString();
  }
  function archiveRefreshValid(id, query, kind) {
    var current = archiveView(), body = B.body();
    return !!current && current.dataset.kind === kind && B.boardId() === id && !!body && body.contains(current) &&
      archiveQuery(current) === query && !archiveBusy();
  }
  function fetchArchive(url) {
    return fetch(url, { credentials: 'same-origin', headers: { 'HX-Request': 'true', 'X-Requested-With': 'weave' } })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(); });
  }
  function archiveCursorDoc(html) {
    // Cursor responses are bare table rows; the table context keeps DOMParser from discarding their tags.
    return new DOMParser().parseFromString('<table><tbody id="archive-cursor-rows">' + html + '</tbody></table>', 'text/html');
  }
  function archivePages(html, loaded, id, query, kind) {
    var doc = new DOMParser().parseFromString(html, 'text/html'), rows = doc.getElementById('archive-rows');
    if (!rows) return Promise.reject(new Error('archive refresh returned no rows'));
    function next() {
      if (rows.querySelectorAll('tr.archive-row[data-card-id]').length >= loaded) return Promise.resolve(doc);
      var more = doc.getElementById('archive-load-more'), url = more && more.getAttribute('hx-get');
      if (!url || url === 'false') return Promise.resolve(doc);
      return fetchArchive(url).then(function (cursorHTML) {
        if (!archiveRefreshValid(id, query, kind)) return null;
        var cursor = archiveCursorDoc(cursorHTML), source = cursor.getElementById('archive-cursor-rows');
        if (!source) return Promise.reject(new Error('archive cursor returned no rows'));
        source.querySelectorAll('tr').forEach(function (row) { rows.appendChild(doc.importNode(row, true)); });
        var pager = cursor.getElementById('archive-pager'), oldPager = doc.getElementById('archive-pager');
        if (pager && oldPager) oldPager.replaceWith(doc.importNode(pager, true));
        else if (oldPager) oldPager.remove();
        return next();
      });
    }
    return next().then(function (finalDoc) { return finalDoc ? finalDoc.body.innerHTML : null; });
  }
  B.refresh = function (force) {
    // Archive / Trash owns its table: SSE and scheduled refreshes must not replace rows or a selection.
    if (archiveMode() && !force) return;
    var id = B.boardId(); if (!id || !B.body()) return;
    if (archiveMode()) {
      var a = archiveView();
      if (!a || archiveBusy()) return;
      var kind = a.dataset.kind, query = archiveQuery(a);
      var loaded = a.querySelectorAll('#archive-rows tr.archive-row[data-card-id]').length;
      return fetchArchive('/ui/boards/' + id + '/archive' + (query ? '?' + query : ''))
        .then(function (html) {
          if (!archiveRefreshValid(id, query, kind)) return null;
          return archivePages(html, loaded, id, query, kind);
        })
        .then(function (html) {
          // A Show/filter request or new selection/typing may have won while the pages were in flight.
          if (!html || !archiveRefreshValid(id, query, kind)) return;
          var body = B.body();
          body.innerHTML = html;
          B.process(body); setup(); sweepPops();
          document.dispatchEvent(new CustomEvent('boards:body-swap'));
        })
        .catch(function (err) { if (err && window.console) console.error('boards archive refresh failed', err); /* offline: the next explicit refresh retries */ });
    }
    if (B.dragging) { B.schedule(1200); return; }
    if (force) document.querySelectorAll('.boards-col').forEach(function (c) { c.__sig = null; });
    return fetch('/ui/boards/' + id + '/body' + location.search, { credentials: 'same-origin', headers: { 'HX-Request': 'true', 'X-Requested-With': 'weave' } })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(); })
      .then(function (html) {
        if (archiveMode()) return; // the Show request won the race while this refresh was in flight
        if (B.dragging) { B.schedule(1200); return; }
        if (B.applyBody(html) === 'dirty') B.schedule(2500);
      })
      .catch(function (err) { if (err && window.console) console.error('boards refresh failed', err); /* offline: the next event retries */ });
  };
  B.schedule = function (ms) { if (archiveMode()) return; clearTimeout(timer); timer = setTimeout(B.refresh, ms || 300); };

  var verbs = ['created', 'updated', 'deleted', 'moved', 'claimed', 'released'];
  var nouns = ['board', 'list', 'card', 'label', 'boardMembership', 'cardMembership', 'cardLabel', 'task', 'taskList', 'comment', 'attachment', 'customFieldValue'];
  // card.placed / card.unplaced: a card was put on or taken off this board; card.placement.changed: a card on this
  // board changed on one of its OTHER boards (its "also on" marker may change)
  var liveNames = ['listener.joined', 'listener.left', 'time.updated', 'card.placement.changed', 'card.placed', 'card.unplaced'];
  nouns.forEach(function (n) { verbs.forEach(function (v) { liveNames.push(n + '.' + v); }); });
  // The tab's one realtime stream belongs to notify_live.js (layout); the board adds its room and
  // listeners to it. After a dropped connection comes back, refresh once (rooms have no replay).
  var live = window.WeaveLive;
  if (live) {
    liveNames.concat(['message', 'reconnect']).forEach(function (n) { live.on(n, function () { if (B.boardId()) B.schedule(300); }); });
    live.on('list.updated', autoArchived);
  }
  function setup() {
    document.querySelectorAll('.boards-col').forEach(function (c) { if (!c.__init) { c.__init = 1; c.__sig = c.outerHTML; B.initCol(c); } });
    var id = B.boardId();
    if (id === esBoard) return;
    esBoard = id;
    if (live) { live.setRooms('board', id ? ['board:' + id] : []); return; }
    if (es) { es.close(); es = null; }
    if (!id || !window.EventSource) return;
    es = new EventSource('/events?rooms=board:' + encodeURIComponent(id));
    liveNames.forEach(function (n) { es.addEventListener(n, function () { B.schedule(300); }); });
    es.onmessage = function () { B.schedule(300); };
    es.addEventListener('list.updated', autoArchived);
  }
  // A column's auto-archive rule ran (one summary event per pass; the per-card events are quiet): say so once. The
  // board itself redraws through the debounced refresh above.
  function autoArchived(ev) {
    var d; try { d = JSON.parse(ev.data); } catch (x) { return; }
    var data = (d && d.data) || {}, n = +data.autoArchived || 0; if (!n || !B.toast) return;
    var col = document.getElementById('col-' + (d.listId || data.id || '')), t = col && col.querySelector('.boards-col-title');
    var name = t ? t.textContent.trim() : 'A column';
    B.toast(name + ': archived ' + n + (n === 1 ? ' older card' : ' older cards') + ' (keeps the newest ' + (+data.keep || 0) + ')');
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
      var el = e.detail.elt, push = el && el.closest && el.closest('[hx-push-url]');
      var pushURL = push && push.getAttribute('hx-push-url');
      try {
        if (pushURL && pushURL !== 'false') {
          var target = pushURL === 'true' ? p : pushURL;
          if (target) {
            var absolute = new URL(target, location.href).href;
            if (absolute !== location.href) history.pushState(history.state, '', target);
          }
        } else if (p) history.replaceState(history.state, '', p);
      } catch (x) { /* history can be unavailable in embedded views */ }
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
      B.post(d.url, {}).then(function (r) {
        if (!r.ok) { B.toast(r.error, { error: true }); return; }
        B.announce('Restored');
        Promise.resolve(B.refresh(true)).then(function () {   // keyboard flow: put focus back on the restored card
          var l = d.refocus && document.querySelector('#tile-' + esc(d.id) + ' .boards-tile-link');
          if (l) { l.focus({ preventScroll: true }); l.closest('.boards-tile').scrollIntoView({ block: 'nearest', inline: 'nearest' }); }
        });
      });
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
