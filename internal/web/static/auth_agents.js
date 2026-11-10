// Copy-to-clipboard for one-time agent API keys.
document.addEventListener('click', function (e) {
  var b = e.target.closest && e.target.closest('[data-copy]');
  if (!b) return;
  var src = document.querySelector(b.getAttribute('data-copy'));
  if (!src) return;
  var text = src.textContent.trim();
  var done = function () { var old = b.textContent; b.textContent = 'Copied'; setTimeout(function () { b.textContent = old; }, 1500); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, function () { fallback(text, done); });
  } else { fallback(text, done); }
});
function fallback(text, done) {
  var r = document.createRange(), s = window.getSelection(), n = document.querySelector('#agent-key');
  if (!n) return;
  r.selectNodeContents(n); s.removeAllRanges(); s.addRange(r);
  try { document.execCommand('copy'); done(); } catch (x) { /* user can still select the key manually */ }
}
