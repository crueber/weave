// Live stopwatch display for the card time-tracking fragment.
if (!window._xClock) {
  window._xClock = setInterval(() => {
    document.querySelectorAll('.x-clock[data-started]').forEach(el => {
      const t = Date.parse(el.dataset.started);
      if (isNaN(t)) return;
      const s = Math.max(0, Math.floor((Date.now() - t) / 1000)) + (parseInt(el.dataset.base, 10) || 0);
      const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), sec = s % 60, p = n => String(n).padStart(2, '0');
      el.textContent = h > 0 ? `${h}h ${p(m)}m ${p(sec)}s` : m > 0 ? `${m}m ${p(sec)}s` : `${sec}s`;
    });
  }, 1000);
}
