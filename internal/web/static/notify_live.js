// Live bell: the page's one realtime stream (GET /events) and the notification badge.
//
// Every signed-in page loads this (layout.html). It owns the tab's single EventSource: the server
// always joins the caller's private room "user:<me>", so `notification` events arrive here; page
// scripts add their rooms and listeners through window.WeaveLive instead of opening a second stream:
//
//   WeaveLive.setRooms('board', ['board:<id>'])   // replaces this owner's rooms (reconnects once)
//   WeaveLive.on('card.updated', fn)              // named SSE event; 'message' = unnamed events;
//                                                 // 'reconnect' = the stream came back after a drop
//
// The badge (#notif-badge, server-rendered fragment) is the source of truth: whenever its content
// changes (first load, a refetch, the out-of-band swap after "Mark read") the bell's lit state,
// aria-label and the "(n) " tab-title prefix follow it. Rooms have no replay, so the badge is
// refetched after a reconnect, when the tab becomes visible again and every few minutes. If the
// stream cannot be kept open the badge is polled every 60 s while the tab is visible, and the stream
// is retried with exponential backoff. A tab hidden for a while with no page rooms parks its stream
// (freeing the connection and the per-user stream slot) and polls once a minute until it is visible.
// Every request made here is background traffic (X-Weave-Background): it never extends an idle session.
(function () {
  if (window.WeaveLive) return;

  var POLL_MS = 60000, SAFETY_MS = 5 * 60000, PARK_AFTER_MS = 2 * 60000;
  var RETRY_MIN_MS = 5000, RETRY_MAX_MS = 10 * 60000, MAX_SOFT_ERRORS = 3, PULSE_MS = 1600, WATCHDOG_MS = 12000;

  var handlers = {}, roomsBy = {}, es = null, started = false, healthy = false, dropped = false;
  var failures = 0, softErrors = 0, parked = false;
  var watchdogTimer = null, retryTimer = null, pollTimer = null, parkTimer = null, refetchTimer = null, pulseTimer = null;
  var badge = null, bell = null, live = null, last = null;

  function rooms() {
    var seen = {}, out = [];
    Object.keys(roomsBy).sort().forEach(function (k) { roomsBy[k].forEach(function (r) { if (r && !seen[r]) { seen[r] = 1; out.push(r); } }); });
    return out;
  }
  function streamURL() {
    var r = rooms();
    return '/events' + (r.length ? '?rooms=' + r.map(encodeURIComponent).join(',') : '');
  }

  function dispatch(name, ev) {
    (handlers[name] || []).slice().forEach(function (fn) {
      try { fn(ev); } catch (err) { if (window.console) console.error('live handler', name, err); }
    });
  }
  function attach(src, name) {
    if (name === 'reconnect' || src.__names[name]) return;
    src.__names[name] = 1;
    var cb = function (ev) { if (src === es) dispatch(name, ev); };
    if (name === 'message') src.onmessage = cb; else src.addEventListener(name, cb);
  }

  function close() {
    if (es) { es.close(); es = null; }
    healthy = false;
  }
  function connect() {
    clearTimeout(retryTimer); retryTimer = null;
    close();
    if (!window.EventSource) { startPolling(); return; }
    var src = es = new EventSource(streamURL());
    src.__names = {};
    Object.keys(handlers).forEach(function (n) { attach(src, n); });
    src.onopen = function () {
      if (src !== es) return;
      clearTimeout(watchdogTimer);
      healthy = true; failures = 0; softErrors = 0;
      stopPolling();
      if (dropped) { dropped = false; refreshBadge(); dispatch('reconnect'); }
    };
    src.onerror = function () {
      if (src !== es) return;
      healthy = false; dropped = true;
      // The browser may go quiet in CONNECTING (no further errors, no open) after the network flaps or the machine
      // sleeps: if it has not reopened within WATCHDOG_MS we take over ourselves.
      clearTimeout(watchdogTimer);
      watchdogTimer = setTimeout(function () {
        if (src !== es || healthy) return;
        close(); failures++; startPolling(); scheduleRetry();
      }, WATCHDOG_MS);
      // CLOSED: an HTTP failure (signed out, too many streams, server error): the browser gives up.
      // CONNECTING: the browser retries on its own; after a few misses without an open, we take over
      // with a slower backoff so a server that stays down is not hit every few seconds.
      if (src.readyState === 2 || ++softErrors >= MAX_SOFT_ERRORS) {
        close(); failures++; startPolling(); scheduleRetry();
      }
    };
  }
  function scheduleRetry() {
    clearTimeout(retryTimer);
    var d = Math.min(RETRY_MAX_MS, RETRY_MIN_MS * Math.pow(2, Math.max(0, failures - 1)));
    retryTimer = setTimeout(function () {
      retryTimer = null;
      if (parked || (document.hidden && !rooms().length)) return; // reconnects on visibility instead
      connect();
    }, d * (0.8 + Math.random() * 0.4));
  }

  function startPolling() {
    if (pollTimer) return;
    pollTimer = setInterval(function () { if (parked || !document.hidden) refreshBadge(); }, POLL_MS);
  }
  function stopPolling() { clearInterval(pollTimer); pollTimer = null; }

  // refreshBadge refetches the server-rendered badge (debounced so bursts and reconnects coalesce).
  function refreshBadge() {
    clearTimeout(refetchTimer);
    refetchTimer = setTimeout(function () {
      if (!badge || !window.htmx) return;
      // offline or the server down: htmx rejects the promise; the next reconnect/poll refetches, nothing to report
      var req = htmx.ajax('GET', '/ui/notifications/badge', { source: badge, target: badge, swap: 'innerHTML', headers: { 'X-Weave-Background': '1' } });
      if (req && req.catch) req.catch(function () { /* retried on reconnect, visibility and the poll */ });
    }, 250);
  }

  function count() {
    var c = badge && badge.querySelector('.notify-count'), n = c ? parseInt(c.textContent, 10) : 0;
    return isNaN(n) || n < 0 ? 0 : n;
  }
  var titleRe = /^\(\d+\+?\) /;
  function applyTitle(n) {
    var base = document.title.replace(titleRe, ''), want = n > 0 ? '(' + (n > 99 ? '99+' : n) + ') ' + base : base;
    if (document.title !== want) document.title = want;
  }
  function announce(msg) {
    if (!live) return;
    live.textContent = '';
    setTimeout(function () { live.textContent = msg; }, 60);
  }
  function pulse() {
    bell.classList.remove('is-new');
    void bell.offsetWidth; // restart the animation if one is still running
    bell.classList.add('is-new');
    clearTimeout(pulseTimer);
    pulseTimer = setTimeout(function () { bell.classList.remove('is-new'); }, PULSE_MS);
  }
  // sync makes the bell, its label and the tab title match the badge.
  function sync() {
    var n = count();
    bell.classList.toggle('is-lit', n > 0);
    bell.setAttribute('aria-label', n > 0 ? 'Notifications, ' + n + ' unread' : 'Notifications');
    applyTitle(n);
    var hc = document.getElementById('notify-head-count'); // the /notifications heading shows the same global count
    if (hc && (hc.textContent.trim() || '0') !== String(n)) {
      if (n > 0) { var pill = document.createElement('span'); pill.className = 'notify-count'; pill.textContent = n; hc.replaceChildren(pill); }
      else hc.replaceChildren();
    }
    if (last !== null && n > last) {
      pulse();
      announce(n - last === 1 ? 'New notification' : (n - last) + ' new notifications');
    }
    last = n;
  }

  function onNotification(ev) {
    var action = '';
    try { action = (JSON.parse(ev.data).data || {}).action || ''; } catch (err) { /* refetch anyway */ }
    refreshBadge();
    if (action === 'create') {
      var fresh = document.getElementById('notify-fresh');
      if (fresh) fresh.hidden = false;
    }
  }

  function onVisibility() {
    if (document.hidden) {
      clearTimeout(parkTimer);
      if (!rooms().length) parkTimer = setTimeout(park, PARK_AFTER_MS);
      return;
    }
    clearTimeout(parkTimer);
    refreshBadge();
    if (parked) { parked = false; stopPolling(); connect(); }
    else if (!es && !retryTimer && started) connect();
  }
  function park() {
    if (!document.hidden || rooms().length) return;
    parked = true; dropped = true;
    clearTimeout(retryTimer); retryTimer = null;
    close();
    startPolling();
  }

  function start() {
    if (started) return;
    started = true;
    badge = document.getElementById('notif-badge');
    bell = document.getElementById('notif-bell');
    live = document.getElementById('notif-live');
    if (badge && bell) {
      new MutationObserver(sync).observe(badge, { childList: true, subtree: true, characterData: true });
      if (badge.querySelector('.notify-count') || badge.textContent) sync();
      var t = document.querySelector('title'); // htmx may replace the title: keep the prefix on it
      if (t) new MutationObserver(function () { if (last !== null) applyTitle(last); }).observe(t, { childList: true, characterData: true, subtree: true });
    }
    document.addEventListener('visibilitychange', onVisibility);
    setInterval(function () { if (!document.hidden && !parked) refreshBadge(); }, SAFETY_MS);
    // the "New notifications" button on /notifications hides once the list is fresh again
    document.addEventListener('htmx:afterSwap', function (e) {
      var fresh = document.getElementById('notify-fresh');
      if (fresh && e.detail && e.detail.target && e.detail.target.id === 'notify-list') fresh.hidden = true;
    });
    if (document.hidden && !rooms().length) { parked = true; dropped = true; startPolling(); return; }
    connect();
  }

  // The network came back (wifi roaming, wake from sleep, offline mode off): do not wait for the browser's own retry
  // timer or our backoff; reconnect now and catch up on what was missed.
  window.addEventListener('online', function () {
    if (!started || parked) return;
    if (!healthy) { failures = 0; connect(); }
    refreshBadge();
  });

  window.WeaveLive = {
    on: function (name, fn) {
      (handlers[name] = handlers[name] || []).push(fn);
      if (es) attach(es, name);
    },
    setRooms: function (owner, list) {
      var before = streamURL();
      roomsBy[owner] = (list || []).filter(Boolean);
      if (!roomsBy[owner].length) delete roomsBy[owner];
      if (started && streamURL() !== before) {
        if (rooms().length && parked) { parked = false; stopPolling(); }
        if (!parked) connect();
      }
    },
    refresh: refreshBadge
  };
  WeaveLive.on('notification', onNotification);

  // Start after the deferred page scripts ran (they register their rooms synchronously), so the
  // first connection already carries them: one stream per tab, no reconnect on page load.
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { setTimeout(start, 0); });
  else setTimeout(start, 0);
})();
