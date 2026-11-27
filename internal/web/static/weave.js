// Minimal client glue and shared window.Weave helpers: toasts, modal backdrop, confirm dialogs,
// popover placement, htmx fetch/error parsing, copy-to-clipboard, theme sync, sortable lists, help tips.
(function () {
  if (window.Weave) return;
  var HDR = { 'HX-Request': 'true', 'X-Requested-With': 'weave' };
  var phone = window.matchMedia ? matchMedia('(max-width:640px)') : { matches: false };

  function outsideBox(el, e) {
    var r = el.getBoundingClientRect();
    return e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom;
  }

  var W = window.Weave = {
    esc: window.CSS && CSS.escape ? function (s) { return CSS.escape(String(s)); } : function (s) { return String(s).replace(/[^\w-]/g, '\\$&'); },
    typing: function (el) { return !!(el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName))); },
    ls: {
      get: function (k) { try { return localStorage.getItem(k); } catch (x) { return null; } },
      set: function (k, v) { try { localStorage.setItem(k, v); } catch (x) { /* private mode */ } },
      getJSON: function (k) { try { return JSON.parse(localStorage.getItem(k) || 'null'); } catch (x) { return null; } },
      setJSON: function (k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch (x) { /* private mode */ } }
    },
    grow: function (ta, maxPx) {
      if (!ta) return;
      ta.style.height = 'auto';
      var want = ta.scrollHeight + 2;
      ta.style.height = (maxPx ? Math.min(want, maxPx) : want) + 'px';
    },
    trapTab: function (root, e, filter) {
      var foc = [].slice.call(root.querySelectorAll('button:not([disabled]),summary,input,select,textarea,[tabindex]')).filter(function (el) {
        if (el.disabled || el.hidden || !el.offsetParent || el.tabIndex < 0 || el.closest('[hidden]')) return false;
        return filter ? filter(el) : true;
      });
      if (!foc.length) return;
      var i = foc.indexOf(document.activeElement);
      if (!e.shiftKey && i === foc.length - 1) { e.preventDefault(); foc[0].focus(); }
      else if (e.shiftKey && i <= 0) { e.preventDefault(); foc[foc.length - 1].focus(); }
    },
    popRoot: function () {
      var r = document.getElementById('popover-root');
      if (!r) { r = document.createElement('div'); r.id = 'popover-root'; r.innerHTML = '<div class="boards-scrim" hidden></div>'; document.body.appendChild(r); }
      return r;
    },
    errorOf: function (html) {
      if (!html) return '';
      var t = document.createElement('template');
      t.innerHTML = String(html);
      var el = t.content.querySelector('[class*="toast"]');
      return el ? el.textContent.trim() : '';
    },
    post: function (url, data, method) {
      var body = new URLSearchParams();
      Object.keys(data || {}).forEach(function (k) {
        if (data[k] === undefined) return;
        [].concat(data[k]).forEach(function (v) { body.append(k, v); });
      });
      var h = { 'Content-Type': 'application/x-www-form-urlencoded' };
      for (var k in HDR) h[k] = HDR[k];
      return fetch(url, { method: method || 'POST', credentials: 'same-origin', body: body, headers: h })
        .then(function (r) {
          return r.text().then(function (html) {
            var trig = {}; try { trig = JSON.parse(r.headers.get('HX-Trigger') || '{}'); } catch (e) { trig = {}; }
            var err = r.ok ? '' : (W.errorOf(html) || 'Something went wrong');
            if (r.headers.get('HX-Redirect')) { location.href = r.headers.get('HX-Redirect'); }
            return { ok: r.ok, status: r.status, html: html, trigger: trig, error: err };
          });
        }, function () { return { ok: false, status: 0, html: '', trigger: {}, error: 'You appear to be offline' }; });
    },
    toast: function (msg, o) {
      o = o || {};
      var m = document.getElementById('modal');
      if (!o.board && m && m.open) {
        var mh = m.querySelector(':scope > .cm-toasts');
        if (!mh) { mh = document.createElement('div'); mh.className = 'cm-toasts'; mh.setAttribute('aria-live', 'polite'); m.appendChild(mh); }
        var mt = document.createElement('div'); mt.className = 'toast' + (o.error ? ' toast-error' : o.success ? ' toast-success' : '');
        if (o.error) mt.setAttribute('role', 'alert');
        mt.textContent = msg;
        mh.appendChild(mt);
        setTimeout(function () { mt.remove(); }, o.ms || 4000);
        return mt;
      }
      var bhost = document.getElementById('boards-toasts');
      if (bhost && (o.board || o.action || !document.getElementById('toasts'))) {
        var bt = document.createElement('div'); bt.className = 'boards-toast' + (o.error ? ' is-error' : '');
        if (o.error) bt.setAttribute('role', 'alert');
        var bs = document.createElement('span'); bs.textContent = msg; bt.appendChild(bs);
        function bdone() { clearTimeout(btimer); bt.remove(); }
        if (o.action) {
          var bb = document.createElement('button'); bb.type = 'button'; bb.textContent = o.action;
          bb.addEventListener('click', function () { bdone(); o.onAction && o.onAction(); });
          bt.appendChild(bb);
        }
        bhost.appendChild(bt);
        var btimer = setTimeout(bdone, o.ms || (o.action ? 6000 : 4000));
        return bt;
      }
      var host = document.getElementById('toasts'); if (!host) return null;
      var t = document.createElement('div'); t.className = 'toast' + (o.error ? ' toast-error' : o.success ? ' toast-success' : '');
      if (o.error) t.setAttribute('role', 'alert');
      t.textContent = msg;
      host.appendChild(t);
      setTimeout(function () { t.remove(); }, o.ms || 4000);
      return t;
    },
    /* place(pop, anchor, opts): anchor is an element or a {left,right,top,bottom} rect.
       opts.align 'right' (default) | 'left'; opts.cap max height; opts.cover align top; opts.minHeight default 120. */
    place: function (pop, anchor, opts) {
      opts = opts || {};
      if (!pop) return;
      if (opts.sheet !== false && phone.matches && (pop.dataset.sheet || opts.phoneSheet)) { pop.style.cssText = ''; return; }
      if (!anchor || (anchor.nodeType === 1 && !document.contains(anchor))) return;
      var r = anchor.getBoundingClientRect ? anchor.getBoundingClientRect() : anchor;
      var vw = document.documentElement.clientWidth, vh = window.innerHeight, M = 8, GAP = 4;
      pop.style.position = 'fixed'; pop.style.right = 'auto'; pop.style.bottom = 'auto';
      pop.style.left = '0px'; pop.style.top = '0px'; pop.style.maxHeight = opts.cap ? opts.cap + 'px' : 'none';
      var w = pop.offsetWidth, h = pop.offsetHeight;
      var left = opts.align === 'left' ? r.left : r.right - w;
      left = Math.max(M, Math.min(left, vw - w - M));
      var top, mh;
      if (opts.cover) {
        mh = vh - 2 * M; top = Math.max(M, Math.min(r.top, vh - M - Math.min(h, mh)));
      } else {
        var below = vh - r.bottom - GAP - M, above = r.top - GAP - M;
        if (h <= below || (below >= above && h > above)) { top = r.bottom + GAP; mh = below; }
        else { mh = above; top = Math.max(M, r.top - GAP - Math.min(h, above)); }
      }
      mh = Math.max(opts.minHeight || 120, opts.cap ? Math.min(mh, opts.cap) : mh);
      pop.style.left = left + 'px'; pop.style.top = top + 'px'; pop.style.maxHeight = mh + 'px';
    },
    confirmOpen: function () {
      return !!document.querySelector('dialog[data-weave-confirm][open], dialog.boards-confirm[open], dialog.cm-confirm[open]');
    },
    confirm: function (o) {
      var cls = o.className || 'cm-confirm';
      var d = document.createElement('dialog');
      d.className = 'weave-confirm ' + cls;
      d.dataset.weaveConfirm = '1';
      d.setAttribute('aria-labelledby', cls + '-h');
      if (o.text) d.setAttribute('aria-describedby', cls + '-p');
      var h = document.createElement('p'); h.className = cls + '-h'; h.id = cls + '-h'; h.textContent = o.title || '';
      var p = document.createElement('p'); p.className = cls + '-p'; p.id = cls + '-p'; p.textContent = o.text || '';
      var row = document.createElement('div'); row.className = cls + '-row';
      var no = document.createElement('button'); no.type = 'button'; no.className = 'btn btn-ghost ' + cls + '-no'; no.textContent = 'Cancel';
      var yes = document.createElement('button'); yes.type = 'button';
      yes.className = 'btn ' + (o.danger === false ? 'btn-primary' : 'btn-danger') + ' ' + cls + '-yes';
      yes.textContent = o.ok || 'OK'; yes.setAttribute('data-confirm-yes', '');
      row.appendChild(no); row.appendChild(yes); d.appendChild(h); if (o.text) d.appendChild(p); d.appendChild(row);
      var m = document.getElementById('modal');
      (m && m.open ? m : document.body).appendChild(d);
      var back = document.activeElement, done = false, bgDown = false;
      function finish(ok) {
        if (done) return; done = true;
        if (d.open) d.close();
        d.remove();
        if (!ok) {
          if (back && document.contains(back) && back.focus) back.focus({ preventScroll: true });
          if (o.onCancel) o.onCancel();
          return;
        }
        o.onOk && o.onOk();
      }
      yes.addEventListener('click', function () { finish(true); });
      no.addEventListener('click', function () { finish(false); });
      d.addEventListener('cancel', function (e) { e.preventDefault(); finish(false); });
      d.addEventListener('keydown', function (e) {
        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finish(false); }
        else if (e.key === 'Tab') { e.preventDefault(); (document.activeElement === yes ? no : yes).focus(); }
        else e.stopPropagation();
      });
      d.addEventListener('pointerdown', function (e) { bgDown = e.target === d && outsideBox(d, e); });
      d.addEventListener('click', function (e) { var ok = bgDown; bgDown = false; if (ok && e.target === d && outsideBox(d, e)) finish(false); });
      d.showModal();
      if (o.anchor && !phone.matches) { d.classList.add('is-placed'); W.place(d, o.anchor, { align: 'left' }); }
      yes.focus();
      return d;
    }
  };
  window.WeavePlace = W.place;

  // ---- Toasts ----
  document.body.addEventListener('toast', function (e) {
    W.toast(e.detail && e.detail.value !== undefined ? e.detail.value : e.detail);
  });
  // A toast for the next page (HX-Trigger "toastNext" sent with HX-Redirect): kept in sessionStorage across the
  // navigation and shown once when the next page loads.
  document.body.addEventListener('toastNext', function (e) {
    try { sessionStorage.setItem('weave-toast', String(e.detail.value || e.detail)); } catch (x) { /* storage off */ }
  });
  (function () {
    var msg = null;
    try { msg = sessionStorage.getItem('weave-toast'); sessionStorage.removeItem('weave-toast'); } catch (x) { return; }
    if (msg) document.body.dispatchEvent(new CustomEvent('toast', { detail: { value: msg } }));
  })();

  // ---- Modal open + safe backdrop click (dragging a text selection out of the dialog never closes it) ----
  document.body.addEventListener('htmx:afterSwap', function (e) {
    var m = document.getElementById('modal');
    if (e.detail.target === m && !m.open) m.showModal();
  });
  var modalBgDown = false;
  document.addEventListener('pointerdown', function (e) {
    var m = document.getElementById('modal');
    modalBgDown = !!(m && m.open && e.target === m && outsideBox(m, e));
  });
  document.addEventListener('click', function (e) {
    var m = document.getElementById('modal');
    var ok = modalBgDown; modalBgDown = false;
    if (ok && m && m.open && e.target === m && outsideBox(m, e)) m.close();
  });

  // ---- Theme: 'weave-theme' in localStorage = light|dark|system ----
  (function () {
    var root = document.documentElement;
    var get = function () { return W.ls.get('weave-theme') || 'system'; };
    function apply(mode) {
      if (mode === 'light' || mode === 'dark') root.setAttribute('data-theme', mode); else root.removeAttribute('data-theme');
      document.querySelectorAll('[data-theme-set]').forEach(function (b) { b.setAttribute('aria-pressed', String(b.dataset.themeSet === mode)); });
    }
    function set(mode, save) {
      W.ls.set('weave-theme', mode);
      apply(mode);
      if (save && document.getElementById('user-menu')) {
        fetch('/api/v1/users/me/preferences', { method: 'PATCH', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'weave' },
          body: JSON.stringify({ theme: mode === 'system' ? 'auto' : mode }) }).catch(function () {});
      }
    }
    apply(get());
    document.addEventListener('click', function (e) { var b = e.target.closest && e.target.closest('[data-theme-set]'); if (b) set(b.dataset.themeSet, true); });
    // Keep the localStorage choice in sync when the settings form saves a theme (radio buttons or select).
    document.body.addEventListener('htmx:afterSwap', function () {
      var s = document.querySelector('input[name=theme]:checked, select[name=theme]'); if (!s) return;
      var v = s.value === 'auto' ? 'system' : s.value; if (v && v !== get()) set(v, false);
    });
    // Close the account menu on outside click / Escape.
    document.addEventListener('click', function (e) { var m = document.getElementById('user-menu'); if (m && m.open && !m.contains(e.target)) m.open = false; });
    document.addEventListener('keydown', function (e) { var m = document.getElementById('user-menu'); if (e.key === 'Escape' && m && m.open) { m.open = false; m.querySelector('summary').focus(); } });
  })();

  // ---- Sortable: any container with data-sortable="<url>" and children with data-id ----
  document.addEventListener('dragstart', function (e) {
    var el = e.target.closest && e.target.closest('[data-sortable] > [data-id]');
    if (el) { window._drag = el; el.classList.add('dragging'); }
  });
  document.addEventListener('dragend', function () { if (window._drag) window._drag.classList.remove('dragging'); window._drag = null; });
  document.addEventListener('dragover', function (e) { if (window._drag && e.target.closest && e.target.closest('[data-sortable]')) e.preventDefault(); });
  document.addEventListener('drop', function (e) {
    var box = e.target.closest && e.target.closest('[data-sortable]'), d = window._drag; if (!box || !d) return;
    e.preventDefault();
    var kids = [].slice.call(box.querySelectorAll(':scope > [data-id]')).filter(function (k) { return k !== d; });
    var idx = kids.findIndex(function (k) {
      var r = k.getBoundingClientRect();
      return box.dataset.axis === 'x' ? e.clientX < r.left + r.width / 2 : e.clientY < r.top + r.height / 2;
    });
    if (idx < 0) idx = kids.length;
    var prev = (kids[idx - 1] && kids[idx - 1].dataset.id) || '', next = (kids[idx] && kids[idx].dataset.id) || '';
    htmx.ajax('POST', box.dataset.sortable, { target: box.dataset.target || '#main', swap: 'none',
      values: { id: d.dataset.id, container: box.dataset.container || '', index: idx, afterId: prev, beforeId: next } });
  });

  // Never persist page snapshots (they can contain e-mail addresses) in the browser's storage.
  if (window.htmx) htmx.config.historyCacheSize = 0;

  // Polling requests (hx-trigger="... every Ns") are background traffic: tell the server so they
  // do not count as user activity for idle auto-logout.
  document.addEventListener('htmx:configRequest', function (e) {
    var el = e.detail && e.detail.elt, t = el && el.getAttribute && el.getAttribute('hx-trigger');
    if (t && /\bevery\s/.test(t)) e.detail.headers['X-Weave-Background'] = '1';
  });

  // ---- Unified copy-to-clipboard: <button data-copy="#selector"> or <button data-copy="element-id"> ----
  function copyFallback(text) {
    try {
      var ta = document.createElement('textarea');
      ta.value = text; ta.setAttribute('readonly', ''); ta.style.position = 'fixed'; ta.style.top = '-1000px';
      document.body.appendChild(ta); ta.select();
      var ok = document.execCommand('copy'); ta.remove();
      return ok ? Promise.resolve() : Promise.reject(new Error('copy failed'));
    } catch (err) { return Promise.reject(err); }
  }
  W.copyText = function (text) {
    if (navigator.clipboard && navigator.clipboard.writeText) return navigator.clipboard.writeText(text).catch(function () { return copyFallback(text); });
    return copyFallback(text);
  };
  document.addEventListener('click', function (e) {
    var btn = e.target && e.target.closest && e.target.closest('[data-copy]');
    if (!btn) return;
    var spec = btn.getAttribute('data-copy') || '';
    if (!spec) return;
    var isSel = spec.charAt(0) === '#';
    var el = isSel ? document.querySelector(spec) : document.getElementById(spec);
    if (!el) return;
    var text = el.value != null ? el.value : el.textContent;
    if (el.select && !isSel) el.select();
    var label = btn.dataset.copyLabel || (btn.dataset.copyLabel = btn.textContent);
    W.copyText(text).then(function () {
      btn.textContent = isSel ? 'Copied' : 'Copied!';
      clearTimeout(btn._copyTimer);
      btn._copyTimer = setTimeout(function () { btn.textContent = label; }, isSel ? 2000 : 1800);
      if (isSel) W.toast('Copied to clipboard');
    }, function () {
      W.toast('Could not copy. Select the text and press Ctrl+C.', { error: true });
    });
  });

  // ---- Delegated behaviours (no inline handlers: the Content-Security-Policy forbids them) ----
  document.addEventListener('click', function (e) {
    var t = e.target; if (!t || !t.closest) return;
    var el = t.closest('[data-select-on-click]'); if (el && el.select) el.select();
    if (t.closest('[data-close-dialog]')) { var d = t.closest('dialog'); if (d) d.close(); }
    if (t.closest('[data-reload]')) location.reload();
    var ct = t.closest('[data-click-trigger]');
    if (ct && !t.closest('a') && window.htmx) htmx.trigger(ct, ct.getAttribute('data-click-trigger'));
  });
  document.addEventListener('htmx:configRequest', function (e) {
    var el = e.detail && e.detail.elt;
    if (el && el.getAttribute && el.hasAttribute('data-bool-param')) e.detail.parameters[el.getAttribute('data-bool-param')] = el.checked;
  });

  // ---- Help tips ("?" buttons, templates/_help_tip.html) ----
  (function () {
    function tipOf(b) { return b.querySelector('.help-tip-pop'); }
    function hide(b) { var p = tipOf(b); if (p) { p.hidden = true; } b.removeAttribute('aria-expanded'); }
    function hideAll(except) { document.querySelectorAll('[data-help-tip][aria-expanded]').forEach(function (b) { if (b !== except) hide(b); }); }
    function show(b) {
      var p = tipOf(b); if (!p) return;
      hideAll(b);
      p.hidden = false; p.style.left = '0px'; p.style.top = '0px';
      var r = b.getBoundingClientRect(), w = p.offsetWidth, h = p.offsetHeight, vw = document.documentElement.clientWidth, vh = window.innerHeight, M = 8;
      var left = Math.max(M, Math.min(r.left + r.width / 2 - w / 2, vw - w - M));
      var top = r.bottom + 6; if (top + h > vh - M) top = Math.max(M, r.top - 6 - h);
      p.style.left = left + 'px'; p.style.top = top + 'px';
      b.setAttribute('aria-expanded', 'true'); b.__shownAt = Date.now();
    }
    function btn(e) { return e.target && e.target.closest ? e.target.closest('[data-help-tip]') : null; }
    document.addEventListener('mouseover', function (e) { var b = btn(e); if (b) show(b); });
    document.addEventListener('mouseout', function (e) { var b = btn(e); if (b && !b.contains(e.relatedTarget)) hide(b); });
    document.addEventListener('focusin', function (e) { var b = btn(e); if (b) show(b); });
    document.addEventListener('focusout', function (e) { var b = btn(e); if (b) hide(b); });
    document.addEventListener('click', function (e) { var b = btn(e); if (b) { e.preventDefault(); if (!b.getAttribute('aria-expanded')) show(b); else if (Date.now() - (b.__shownAt || 0) > 400) hide(b); } else hideAll(null); });
    document.addEventListener('keydown', function (e) {
      if (e.key !== 'Escape' || !document.querySelector('[data-help-tip][aria-expanded]')) return;
      hideAll(null); e.preventDefault(); e.stopPropagation();
    }, true);
  })();
})();
