/* Shared copy-to-clipboard: <button data-copy="#selector"> copies the matched field's value (or text). One delegated
   listener for the whole page; safe to load more than once. No inline handlers (CSP). */
(function () {
  'use strict';
  if (window.__weaveCopy) return;
  window.__weaveCopy = true;
  function fallback(text) {
    try {
      var t = document.createElement('textarea');
      t.value = text; t.setAttribute('readonly', ''); t.style.position = 'fixed'; t.style.top = '-1000px';
      document.body.appendChild(t); t.select();
      var ok = document.execCommand('copy'); t.remove();
      return ok ? Promise.resolve() : Promise.reject(new Error('copy failed'));
    } catch (err) { return Promise.reject(err); }
  }
  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) return navigator.clipboard.writeText(text).catch(function () { return fallback(text); });
    return fallback(text);
  }
  function announce(msg, isErr) {
    var B = window.Boards || window.B || window.WeaveBoards;
    if (B && B.toast) { try { B.toast(msg, isErr ? { error: true } : undefined); } catch (e) { /* ignore */ } }
  }
  document.addEventListener('click', function (e) {
    var btn = e.target.closest && e.target.closest('[data-copy^="#"]');
    if (!btn) return;
    var el = document.querySelector(btn.getAttribute('data-copy'));
    if (!el) return;
    var text = el.value != null ? el.value : el.textContent;
    var label = btn.getAttribute('data-copy-label') || btn.textContent;
    btn.setAttribute('data-copy-label', label);
    copyText(text).then(function () {
      btn.textContent = 'Copied';
      setTimeout(function () { btn.textContent = label; }, 2000);
      announce('Copied to clipboard');
    }, function () { announce('Could not copy. Select the text and press Ctrl+C.', true); });
  });
})();
