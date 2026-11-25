// Archive / Trash: Show-state, exact-count bulk selection and confirmations.
// The server owns the markup and URLs; this only coordinates delegated HTMX swaps.
(function () {
  if (window.BoardsArchive) return;
  var selected = new Set();
  var all = false;
  var pendingAction = '';

  function archive() { return document.getElementById('archive-view'); }
  function boardRoot() { return document.getElementById('board-shell') || document.getElementById('board-body'); }
  function bulkForm() { return archive() && archive().querySelector('#archive-bulk'); }
  function rows() {
    var a = archive(), out = [];
    if (a) a.querySelectorAll('#archive-rows tr.archive-row[data-card-id]').forEach(function (r) { out.push(r); });
    return out;
  }
  function rowID(row) { return row && row.dataset ? row.dataset.cardId : ''; }
  function cardCheck(row) { return row && row.querySelector('input[data-archive-check="card"]'); }
  function pageCheck() { return archive() && archive().querySelector('input[data-archive-check="page"]'); }
  function total() {
    var a = archive(); if (!a) return 0;
    var t = document.getElementById('archive-total');
    var m = t && /(\d[\d,]*)/.exec(t.textContent || '');
    var n = m ? parseInt(m[1].replace(/,/g, ''), 10) : parseInt(a.dataset.total, 10);
    if (!isFinite(n)) n = 0;
    n = Math.max(0, n);
    a.dataset.total = String(n);
    return n;
  }
  function cardWord(n) { return n === 1 ? 'card' : 'cards'; }

  function hidden(form, name, value) {
    var list = form.querySelectorAll('input[type="hidden"][name="' + name + '"]'), field = list[0];
    if (!field) { field = document.createElement('input'); field.type = 'hidden'; field.name = name; form.appendChild(field); }
    else for (var i = 1; i < list.length; i++) list[i].remove();
    field.value = value;
    return field;
  }
  function setArchiveCheck(box, checked, locked) {
    if (!box) return;
    box.checked = checked;
    box.disabled = locked;
    if (locked) {
      if (box.__archiveTitle === undefined) box.__archiveTitle = box.title;
      box.title = 'All matching cards are selected. Clear selection to choose individual cards.';
    } else if (box.__archiveTitle !== undefined) {
      box.title = box.__archiveTitle;
      delete box.__archiveTitle;
    }
  }
  function count() { return all ? total() : selected.size; }

  function syncSelection() {
    var a = archive(), form = bulkForm(); if (!a) return;
    initForm(form);
    a.querySelectorAll('.archive-row-action').forEach(function (f) {
      if ((f.getAttribute('hx-post') || '').indexOf('/archive/delete') < 0) return;
      f.setAttribute('hx-confirm', 'Permanently delete 1 card? This cannot be undone. Cards that are only partially in Trash may be skipped.');
      f.setAttribute('data-confirm-title', 'Permanently delete 1 card?');
      f.setAttribute('data-confirm-ok', 'Delete forever');
    });
    var loaded = rows();
    loaded.forEach(function (r) {
      var c = cardCheck(r), on = !!c && (all || selected.has(rowID(r)));
      setArchiveCheck(c, on, all);
      r.classList.toggle('is-selected', on);
    });
    var p = pageCheck();
    if (p) {
      var checked = loaded.filter(function (r) { var c = cardCheck(r); return c && c.checked; }).length;
      var pageOn = all || (loaded.length > 0 && checked === loaded.length);
      p.indeterminate = !all && checked > 0 && checked < loaded.length;
      setArchiveCheck(p, pageOn, all);
    }
    var bar = a.querySelector('#archive-bulk-bar');
    if (bar) bar.hidden = !(all || selected.size > 0);
    var n = count(), matching = total(), tooMany = !all && selected.size > 500;
    var countEl = a.querySelector('#archive-selection-count');
    if (countEl) {
      if (tooMany) {
        countEl.textContent = n + ' selected — use Select all ' + matching + ' matching';
        countEl.title = 'Explicit selections are limited to 500 cards. Use Select all ' + matching + ' matching this filter.';
      } else {
        var label = countEl.__archiveLabel !== undefined ? countEl.__archiveLabel : '0 selected';
        countEl.textContent = label.replace(/\d[\d,]*/, String(n));
        countEl.removeAttribute('title');
      }
    }
    var selectAll = a.querySelector('[data-archive-select-all]');
    if (selectAll) {
      if (all) {
        selectAll.textContent = 'Clear selection';
        selectAll.title = 'Clear selection';
      } else {
        var selectLabel = selectAll.__archiveLabel !== undefined ? selectAll.__archiveLabel : 'Select all ' + matching + ' matching this filter';
        selectAll.textContent = selectLabel.replace(/\d[\d,]*/, String(matching));
        selectAll.removeAttribute('title');
      }
      if (selectAll.tagName === 'BUTTON') selectAll.disabled = !all && matching === 0;
    }
    if (!form) return;
    hidden(form, 'all', all ? 'true' : 'false');
    hidden(form, 'confirmCount', all ? String(matching) : '');
    hidden(form, 'cardIds', JSON.stringify(all ? [] : Array.from(selected)));
    form.querySelectorAll('[data-archive-bulk]').forEach(function (b) { b.disabled = tooMany; });
  }
  function resetSelection() {
    selected.clear(); all = false; pendingAction = '';
    syncSelection();
  }

  function showFromURL() {
    var s = new URL(location.href).searchParams.get('show');
    return s === 'archived' || s === 'trash' ? s : 'active';
  }
  function showFromDOM() {
    var a = archive();
    if (a && (a.dataset.kind === 'archived' || a.dataset.kind === 'trash')) return a.dataset.kind;
    // A full Archive page lazy-loads its fragment into an otherwise empty #board-body.
    var root = boardRoot(), body = document.getElementById('board-body');
    if (root && body && /\/archive(?:[?]|$)/.test(body.dataset.bodyUrl || '') &&
        (root.dataset.show === 'archived' || root.dataset.show === 'trash')) return root.dataset.show;
    return 'active';
  }
  function syncShow() {
    var show = showFromDOM(), root = boardRoot(), hiddenViews = show !== 'active';
    if (root) root.dataset.show = show;
    document.querySelectorAll('.view-switch, .view-pick').forEach(function (v) { v.hidden = hiddenViews; });
    document.querySelectorAll('[data-show]').forEach(function (c) {
      if (c === root) return;
      var on = c.dataset.show === show;
      if (c.tagName === 'SELECT') c.value = show;
      else if ((c.tagName === 'INPUT' && c.type === 'radio') || c.type === 'checkbox') c.checked = on;
      else if (c.tagName === 'A') { if (on) c.setAttribute('aria-current', 'page'); else c.removeAttribute('aria-current'); }
      else if (c.tagName === 'OPTION' || c.getAttribute('role') === 'option') c.setAttribute('aria-selected', on ? 'true' : 'false');
      else c.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
  }
  function sync() { syncShow(); syncSelection(); }
  function syncArchiveURL() {
    var a = archive(), form = a && a.querySelector('#archive-filters');
    if (!a || !form || !a.dataset.boardId) return;
    if (a.dataset.kind !== 'archived' && a.dataset.kind !== 'trash') return;
    // A card / column sheet owns its history entry and URL while it is open.
    if (history.state && (history.state.card || history.state.list)) return;
    var params = new URLSearchParams(), data = new FormData(form);
    ['q', 'labelId', 'memberId', 'fromListId', 'since', 'until', 'sort', 'dir', 'view'].forEach(function (key) {
      var value = data.get(key);
      if (value) params.set(key, value);
    });
    params.set('show', a.dataset.kind);
    var boardPath = '/boards/' + encodeURIComponent(a.dataset.boardId);
    var archivePath = '/ui/boards/' + encodeURIComponent(a.dataset.boardId) + '/archive';
    var url = boardPath + '?' + params.toString();
    // The top-bar links outlive #board-body, so each filter swap must also retarget every Show control.
    document.querySelectorAll('a[data-show], a[data-show-link]').forEach(function (link) {
      var show = link.dataset.show || link.dataset.showLink;
      if (show !== 'active' && show !== 'archived' && show !== 'trash') return;
      var query = new URLSearchParams(params);
      if (show === 'active') query.delete('show');
      else query.set('show', show);
      var href = boardPath + '?' + query.toString();
      link.setAttribute('href', href);
      if (show === 'active') {
        Array.prototype.slice.call(link.attributes).forEach(function (attr) {
          if (attr.name.indexOf('hx-') === 0) link.removeAttribute(attr.name);
        });
      } else {
        link.setAttribute('hx-get', archivePath + '?' + query.toString());
        link.setAttribute('hx-push-url', href);
      }
      if (window.htmx) window.htmx.process(link);
    });
    try { history.replaceState(history.state, '', url); } catch (e) { /* history can be unavailable in embedded views */ }
  }

  // Cache both endpoints once; preparation may then safely change the form's hx-post.
  function endpoint(form, action, source) {
    var ep = form.__archiveEndpoints || (form.__archiveEndpoints = {});
    if (ep[action]) return ep[action];
    var btn = form.querySelector('[data-archive-bulk="' + action + '"]') || source;
    var names = action === 'restore' ?
      ['data-archive-restore-url', 'data-restore-url', 'data-restore-action'] :
      ['data-archive-delete-url', 'data-delete-url', 'data-delete-action'];
    var attrs = names.concat(['data-archive-url', 'data-url', 'formaction', 'hx-post']);
    var url = '';
    for (var i = 0; i < attrs.length && !url; i++) {
      url = (btn && btn.getAttribute(attrs[i])) || (source && source.getAttribute(attrs[i])) || '';
    }
    if (!url) for (var j = 0; j < names.length && !url; j++) url = form.getAttribute(names[j]) || '';
    // A bare form hx-post/action is the restore endpoint. Delete must expose its own URL.
    if (!url && action === 'restore') url = form.getAttribute('hx-post') || form.getAttribute('action') || '';
    ep[action] = url;
    return url;
  }
  function initForm(form) {
    if (!form || form.__archiveReady) return;
    form.__archiveReady = true;
    var countEl = form.querySelector('#archive-selection-count');
    var selectAll = form.querySelector('[data-archive-select-all]');
    if (countEl && countEl.__archiveLabel === undefined) countEl.__archiveLabel = countEl.textContent;
    if (selectAll && selectAll.__archiveLabel === undefined) selectAll.__archiveLabel = selectAll.textContent;
    endpoint(form, 'restore'); endpoint(form, 'delete');
    form.__archiveFormRequest = form.hasAttribute('hx-post') || !form.querySelector('[data-archive-bulk][hx-post]');
    form.querySelectorAll('[data-archive-bulk], [data-archive-select-all]').forEach(function (b) {
      if (b.tagName !== 'BUTTON') return;
      if (b.hasAttribute('data-archive-bulk') && b.type === 'button') b.type = 'submit';
      else if (b.hasAttribute('data-archive-select-all') && !b.hasAttribute('type')) b.type = 'button';
    });
  }
  function prepare(form, action, source) {
    initForm(form);
    if (!all && selected.size > 500) return false;
    var n = count(), subject = (all ? 'all ' + n + ' ' + cardWord(n) + ' matching this filter' : n + ' selected ' + cardWord(n));
    var url = endpoint(form, action, source), question, title, ok;
    if (action === 'delete') {
      question = 'Permanently delete ' + subject + '? This cannot be undone. Cards that are only partially in Trash may be skipped.';
      title = 'Permanently delete ' + n + ' ' + cardWord(n) + '?';
      ok = 'Delete ' + n + ' forever';
    } else {
      question = 'Restore ' + subject + '? Restoring also applies to every board each card is on.';
      title = 'Restore ' + n + ' ' + cardWord(n) + '?';
      ok = 'Restore ' + n;
    }
    [form, source].forEach(function (el) {
      if (!el) return;
      el.setAttribute('hx-confirm', question);
      el.setAttribute('data-confirm-title', title);
      el.setAttribute('data-confirm-ok', ok);
      if (action === 'delete') el.removeAttribute('data-confirm-danger');
      else el.setAttribute('data-confirm-danger', 'false');
    });
    if (!url) return false;
    form.action = url;
    if (form.__archiveFormRequest) form.setAttribute('hx-post', url);
    else if (source) { source.setAttribute('hx-post', url); source.formAction = url; }
    return true;
  }

  document.addEventListener('change', function (e) {
    var el = e.target.closest && e.target.closest('[data-archive-check]');
    if (!el || !archive()) return;
    if (all) { syncSelection(); return; }
    if (el.dataset.archiveCheck === 'page') {
      rows().forEach(function (r) {
        var id = rowID(r); if (!id) return;
        if (el.checked) selected.add(id); else selected.delete(id);
      });
    } else if (el.dataset.archiveCheck === 'card') {
      var row = el.closest('tr.archive-row'), id = rowID(row);
      if (id) { if (el.checked) selected.add(id); else selected.delete(id); }
    }
    syncSelection();
  });
  document.addEventListener('click', function (e) {
    // A full Archive load may omit the switchers rather than render them hidden. In that case a
    // normal navigation is the only way to bring the Active page's top bar / phone picker back.
    var active = e.target.closest && e.target.closest('[data-show-link="active"]');
    if (active && showFromDOM() !== 'active' && !document.querySelector('.view-switch, .view-pick')) {
      e.preventDefault(); location.assign(active.href); return;
    }
    var selectAll = e.target.closest && e.target.closest('[data-archive-select-all]');
    if (selectAll && archive()) {
      e.preventDefault();
      if (all) resetSelection();
      else { all = true; selected.clear(); syncSelection(); }
      return;
    }
    var button = e.target.closest && e.target.closest('[data-archive-bulk]');
    var form = button && button.closest('#archive-bulk');
    if (!form) return;
    var action = button.dataset.archiveBulk;
    pendingAction = action;
    if (!prepare(form, action, button)) { e.preventDefault(); pendingAction = ''; return; }
    setTimeout(function () { if (pendingAction === action) pendingAction = ''; }, 0);
  }, true);
  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (!form || form.id !== 'archive-bulk') return;
    var source = e.submitter || pendingAction && form.querySelector('[data-archive-bulk="' + pendingAction + '"]');
    var action = source && source.dataset ? source.dataset.archiveBulk : 'restore';
    if (!prepare(form, action, source)) e.preventDefault();
  }, true);

  document.addEventListener('boards:body-swap', function () { resetSelection(); sync(); syncArchiveURL(); });
  document.addEventListener('htmx:afterSwap', function (e) {
    var t = e.target;
    if (t && t.id === 'archive-rows') { sync(); return; }
    if (t && t.id === 'board-body') { resetSelection(); sync(); syncArchiveURL(); }
  });
  document.addEventListener('htmx:historyRestore', sync);
  document.addEventListener('htmx:afterRequest', function (e) {
    var el = e.detail && e.detail.elt;
    if (el && el.closest && el.closest('#archive-bulk')) pendingAction = '';
  });
  window.addEventListener('popstate', function (e) {
    // Card / column sheets own their history entries (cards_ui.js).
    if (e.state && (e.state.card || e.state.list)) return;
    if (showFromURL() !== showFromDOM()) { location.reload(); return; }
    sync();
  });

  function init() { initForm(bulkForm()); sync(); }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
  window.BoardsArchive = { sync: sync, reset: resetSelection };
})();
