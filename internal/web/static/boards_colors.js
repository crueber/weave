/* Colour picker behaviour (boards_color_picker). Delegated, so it survives htmx swaps.
   The native <input type=color> and the hex field never carry a name; they only set the value of the
   "custom" radio and fire a normal change event from it, so forms (hx-trigger=change) submit one value. */
(function () {
  var HEX = /^#[0-9a-fA-F]{6}$/;
  function norm(v) { v = (v || '').trim(); if (v && v[0] !== '#') v = '#' + v; return v; }
  function apply(pick, hex) {
    var radio = pick.querySelector('[data-bcp-radio]');
    var native = pick.querySelector('[data-bcp-color]');
    var field = pick.querySelector('[data-bcp-hex]');
    if (!radio || !HEX.test(hex)) { if (field) field.setAttribute('aria-invalid', 'true'); return; }
    hex = hex.toLowerCase();
    if (field) { field.removeAttribute('aria-invalid'); field.value = hex; }
    if (native) native.value = hex;
    radio.value = hex;
    radio.parentNode.hidden = false;
    radio.parentNode.querySelector('i').style.setProperty('--sw', hex);
    radio.checked = true;
    radio.dispatchEvent(new Event('change', { bubbles: true }));
  }
  // Capture phase: keep the unnamed helper inputs from triggering form autosave themselves.
  document.addEventListener('change', function (e) {
    var t = e.target;
    if (!t || !t.matches) return;
    if (t.matches('[data-bcp-color]')) { e.stopPropagation(); apply(t.closest('[data-colorpick]'), t.value); }
    else if (t.matches('[data-bcp-hex]')) { e.stopPropagation(); if (t.value.trim()) apply(t.closest('[data-colorpick]'), norm(t.value)); }
  }, true);
  document.addEventListener('click', function (e) {
    var b = e.target.closest && e.target.closest('[data-bcp-apply]');
    if (!b) return;
    var pick = b.closest('[data-colorpick]'), f = pick.querySelector('[data-bcp-hex]');
    apply(pick, norm(f.value) || pick.querySelector('[data-bcp-color]').value);
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && e.target.matches && e.target.matches('[data-bcp-hex]')) {
      e.preventDefault();
      apply(e.target.closest('[data-colorpick]'), norm(e.target.value));
    }
  });
})();
