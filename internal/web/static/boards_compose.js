// Composers: "+ Add a card" per list and "+ Add another list". Enter creates and keeps the
// composer open and focused so you can type the next one; nothing opens the card detail.
(function () {
  var B = window.Boards; if (!B || B.composeInit) return; B.composeInit = true;

  function compOf(el) { return el.closest('.boards-composer'); }
  function inputOf(c) { return c.querySelector('[data-composer-input]'); }
  function colOf(c) { return c.closest('.boards-col'); }
  function names(text) {
    return String(text).replace(/\r/g, '\n').split('\n').map(function (l) { return l.trim().replace(/^[-*•]\s*/, ''); }).filter(Boolean);
  }
  function autosize(ta) { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight + 2, 240) + 'px'; }

  function closeComposer(c, o) {
    if (!c.classList.contains('is-open')) return;
    c.classList.remove('is-open');
    var top = c.dataset.mode === 'top';
    if (top) { // return to the bottom slot
      delete c.dataset.mode; var col = colOf(c); if (col) col.appendChild(c);
    }
    var f = c.querySelector('form'); if (f) { var h = f.querySelector('input[name=top]'); h && h.remove(); }
    if (o && o.focus) { var btn = c.querySelector('[data-open]'); btn && btn.focus({ preventScroll: true }); }
  }
  function closeOthers(except) { document.querySelectorAll('.boards-composer.is-open').forEach(function (c) { if (c !== except) closeComposer(c); }); }

  function open(c, o) {
    o = o || {};
    closeOthers(c);
    var col = colOf(c);
    if (col && col.classList.contains('is-collapsed')) B.setCollapsed(col, false);
    if (c.dataset.composer === 'card') {
      var cards = col.querySelector('.boards-cards');
      var f = c.querySelector('form'), h = f.querySelector('input[name=top]');
      if (o.top) {
        c.dataset.mode = 'top'; c.classList.add('is-inline');
        cards.prepend(c); cards.scrollTop = 0;
        if (!h) { h = document.createElement('input'); h.type = 'hidden'; h.name = 'top'; h.value = '1'; f.appendChild(h); }
      } else {
        if (c.dataset.mode === 'top') { delete c.dataset.mode; col.appendChild(c); }
        c.classList.remove('is-inline'); if (h) h.remove();
      }
    }
    c.classList.add('is-open');
    var ta = inputOf(c); autosize(ta); ta.focus({ preventScroll: false });
    if (c.dataset.composer === 'list') { var l = B.lists(); l.scrollLeft = l.scrollWidth; }
    else if (c.dataset.mode !== 'top') { var cc = col.querySelector('.boards-cards'); cc.scrollTop = cc.scrollHeight; }
    c.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }
  B.openComposer = function (col, o) { var c = col.querySelector('.boards-composer'); if (c) open(c, o); return !!c; };
  B.openListComposer = function () { var c = document.querySelector('.boards-composer[data-composer=list]'); if (c) open(c); return !!c; };

  // ---- submit ----
  var queues = new WeakMap();
  function enqueue(c, fn) {
    var p = (queues.get(c) || Promise.resolve()).then(fn, fn);
    queues.set(c, p); return p;
  }

  function pendingTile(name) {
    var a = document.createElement('article'); a.className = 'boards-tile is-pending is-new'; a.setAttribute('role', 'listitem'); a.setAttribute('aria-busy', 'true');
    var d = document.createElement('div'); d.className = 'boards-tile-name'; var s = document.createElement('span'); s.className = 'boards-tile-link'; s.textContent = name;
    d.appendChild(s); a.appendChild(d); return a;
  }

  function submitCard(c, text) {
    var ns = names(text); if (!ns.length) return;
    var f = c.querySelector('form'), col = colOf(c), cards = col.querySelector('.boards-cards'), top = c.dataset.mode === 'top';
    var ref = top ? c.nextElementSibling : null;               // insertion point
    var pend = ns.map(function (n) {
      var p = pendingTile(n);
      if (top) cards.insertBefore(p, ref); else cards.appendChild(p);
      return p;
    });
    if (!top) cards.scrollTop = cards.scrollHeight;
    var data = { name: ns }; if (top) data.top = '1';
    var listName = (col.querySelector('.boards-col-title') || {}).textContent || 'list';
    enqueue(c, function () {
      return B.post(f.action, data).then(function (r) {
        if (!r.ok) {
          pend.forEach(function (p) { p.remove(); });
          var ta = inputOf(c); ta.value = ns.join('\n') + (ta.value ? '\n' + ta.value : ''); autosize(ta);
          B.toast(r.error || 'Could not add the card', { error: true });
          return;
        }
        var frag = B.fragment(r.html), tiles = [].slice.call(frag.children).filter(function (n) { return n.classList.contains('boards-tile'); });
        tiles.forEach(function (t, i) {
          t.classList.add('is-new');
          var p = pend[i];
          if (p && p.parentNode) p.replaceWith(t); else (top ? cards.insertBefore(t, c.nextElementSibling) : cards.appendChild(t));
        });
        pend.slice(tiles.length).forEach(function (p) { p.remove(); });
        tiles.forEach(function (t) { B.process(t); });
        B.recount(col);
        if (!top) cards.scrollTop = cards.scrollHeight;
        var m = (r.trigger['boards:added'] || {});
        if (m.error) B.toast(m.error, { error: true });
        B.announce(tiles.length === 1 ? 'Added card ' + ns[0] + ' to ' + listName : 'Added ' + tiles.length + ' cards to ' + listName);
      });
    });
  }

  function submitList(c, text) {
    var ns = names(text); if (!ns.length) return;
    var f = c.querySelector('form');
    enqueue(c, function () {
      return B.post(f.action, { name: ns }).then(function (r) {
        if (!r.ok) {
          var ta = inputOf(c); ta.value = ns.join('\n') + (ta.value ? '\n' + ta.value : ''); autosize(ta);
          B.toast(r.error || 'Could not add the list', { error: true }); return;
        }
        var frag = B.fragment(r.html), lists = B.lists(), cols = [].slice.call(frag.children).filter(function (n) { return n.classList.contains('boards-col'); });
        cols.forEach(function (col) {
          col.classList.add('is-new'); col.__sig = null; B.initCol(col);
          lists.insertBefore(col, c); B.process(col);
        });
        lists.scrollLeft = lists.scrollWidth;                   // keep the composer in view
        B.announce(cols.length === 1 ? 'Added list ' + ns[0] : 'Added ' + cols.length + ' lists');
      });
    });
  }

  function submit(c, text) {
    var ta = inputOf(c);
    var v = text !== undefined ? text : ta.value;
    if (!v.trim()) return;
    if (text === undefined) { ta.value = ''; autosize(ta); }
    (c.dataset.composer === 'list' ? submitList : submitCard)(c, v);
    ta.focus({ preventScroll: true });
  }

  // ---- events ----
  document.addEventListener('click', function (e) {
    var t = e.target.closest && e.target.closest('[data-open],[data-close],[data-add-top],[data-add-top-btn]');
    if (!t) return;
    if (t.matches('[data-open]')) { e.preventDefault(); open(compOf(t)); }
    else if (t.matches('[data-close]')) { e.preventDefault(); closeComposer(compOf(t), { focus: true }); }
    else { // add to the top of the list
      e.preventDefault();
      var col = B.colOf(t); var m = B.menuOf(t); if (m) m.removeAttribute('open');
      B.openComposer(col, { top: true });
    }
  });
  document.addEventListener('keydown', function (e) {
    var ta = e.target.closest && e.target.closest('[data-composer-input]'); if (!ta) return;
    var c = compOf(ta);
    if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); submit(c); }   // Enter and Shift+Enter both add; titles never contain newlines
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeComposer(c, { focus: true }); ta.value = ''; }
  });
  document.addEventListener('input', function (e) { if (e.target.matches && e.target.matches('[data-composer-input]')) { if (!e.target.value.trim()) e.target.value = ''; autosize(e.target); } });
  document.addEventListener('paste', function (e) {
    var ta = e.target.closest && e.target.closest('[data-composer-input]'); if (!ta) return;
    var txt = (e.clipboardData || window.clipboardData).getData('text');
    if (names(txt).length < 2) return;                          // a single line pastes normally
    e.preventDefault();
    var before = ta.value.slice(0, ta.selectionStart), after = ta.value.slice(ta.selectionEnd);
    ta.value = after; autosize(ta);
    submit(compOf(ta), before + txt);
  });
  document.addEventListener('submit', function (e) {
    var f = e.target; if (!f.matches || !f.matches('.boards-composer-form')) return;
    e.preventDefault(); submit(compOf(f));
  });
  // click-away closes (the draft text stays in the field)
  document.addEventListener('pointerdown', function (e) {
    document.querySelectorAll('.boards-composer.is-open').forEach(function (c) { if (!c.contains(e.target)) closeComposer(c); });
  });
})();
