#!/usr/bin/env node
// Browser check for the live notification bell (static/notify_live.js); not run by `go test`.
//
// Two users in real Chromium with real session cookies: A sits idle on a page (dashboard, a board,
// /notifications) while B comments on a card A watches over the REST API. Asserts, with NO interaction
// from A: the badge count, lit bell and "(n) " tab title appear within a few seconds, exactly once per
// notification (one EventSource per tab, no duplicate pulses), the pulse class is applied and cleared,
// reduced motion runs no animation, the board page still live-refreshes through the shared stream, a
// dropped stream / server restart is followed by a catch-up refetch, a hidden tab parks its stream
// and polls, the /notifications page offers "New notifications" instead of moving the list, marking
// read lowers the count live, and there are zero CSP violations, page errors or console errors.
//
// Runs against a THROWAWAY instance seeded by scripts/demo/seed.sh (it reads $DEMO_DIR/keys.json and
// sessions.json; user A = avery, user B = jordan) and creates its own board:
//
//   DEMO_DIR=... WEAVE_BASE_URL=http://localhost:18120 PW_CORE=/path/playwright-core/index.mjs \
//   PW_CHROMIUM=/path/chrome-headless-shell [WEAVE_RESTART='cmd that restarts the server'] node scripts/notify-live-smoke.mjs
//
// WEAVE_RESTART (optional) is run with `sh -c` to test a real server restart; without it that step is skipped.
// Exit status 0 = every step passed.
import fs from 'fs';
import { execSync } from 'child_process';

const BASE = (process.env.WEAVE_BASE_URL || 'http://localhost:18120').replace(/\/$/, '');
const DIR = process.env.DEMO_DIR;
if (!DIR) { console.error('set DEMO_DIR (see scripts/demo/seed.sh)'); process.exit(2); }
const keys = JSON.parse(fs.readFileSync(DIR + '/keys.json', 'utf8'));
const sess = JSON.parse(fs.readFileSync(DIR + '/sessions.json', 'utf8'));
const pw = await import(process.env.PW_CORE || 'playwright-core');
const chromium = pw.chromium || pw.default.chromium;
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM || undefined, args: ['--disable-dev-shm-usage', '--disable-gpu'], env: process.env });

const failures = [], violations = [];
let step = '';
const ok = (c, m) => { if (!c) throw new Error(m); };

async function api(who, method, path, body) {
  const r = await fetch(BASE + '/api/v1' + path, {
    method, headers: { Authorization: 'Bearer ' + keys[who], 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const t = await r.text();
  let j; try { j = JSON.parse(t); } catch { j = {}; }
  if (!r.ok) throw new Error(`${method} ${path}: ${r.status} ${t.slice(0, 200)}`);
  return j.item ?? j;
}
const A = 'avery', B = 'jordan';

// ---- seed: a board both can use, a card A watches --------------------------------------------
const stamp = Date.now().toString(36);
const proj = await api(A, 'POST', '/projects', { name: 'Live bell ' + stamp });
const board = await api(A, 'POST', `/projects/${proj.id}/boards`, { name: 'Live bell' });
const list = await api(A, 'POST', `/boards/${board.id}/lists`, { name: 'Inbox', type: 'active' });
await api(A, 'POST', `/boards/${board.id}/members`, { userId: sess[B].id, role: 'editor' });
const card = await api(A, 'POST', `/lists/${list.id}/cards`, { name: 'Watch me' });
await api(A, 'PUT', `/cards/${card.id}/subscription`);
let n = 0;
const ping = () => api(B, 'POST', `/cards/${card.id}/comments`, { text: 'Ping ' + (++n) }); // one commentCard for A
const readAll = () => api(A, 'POST', '/notifications/read-all');
const unread = async () => (await api(A, 'GET', '/notifications/unread-count')).count;

// ---- browser helpers ----------------------------------------------------------------------------
async function newCtx(opts = {}) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, locale: 'en-US', ...opts });
  await ctx.addCookies([{ name: 'weave_session', value: sess[A].session, url: BASE }]);
  await ctx.addInitScript(() => {
    // count EventSources made by the page and how many are live
    const O = window.EventSource, made = [];
    window.__es = { made, live: () => made.filter(s => s.readyState !== 2).length, urls: () => made.map(s => s.url) };
    window.EventSource = class extends O { constructor(u, o) { super(u, o); made.push(this); } };
    window.__pulses = 0; window.__notifEvents = 0; window.__titles = [];
    document.addEventListener('DOMContentLoaded', () => {
      const bell = document.getElementById('notif-bell');
      // records are batched: a record's new value is the next record's old value (or the current class)
      const has = v => /(^|\s)is-new(\s|$)/.test(v || '');
      if (bell) new MutationObserver(ms => ms.forEach((m, i) => { const now = i + 1 < ms.length ? ms[i + 1].oldValue : bell.className; if (has(now) && !has(m.oldValue)) window.__pulses++; }))
        .observe(bell, { attributes: true, attributeFilter: ['class'], attributeOldValue: true });
      const t = document.querySelector('title');
      if (t) new MutationObserver(() => window.__titles.push(document.title)).observe(t, { childList: true, characterData: true, subtree: true });
      setTimeout(() => window.WeaveLive && window.WeaveLive.on('notification', () => window.__notifEvents++), 0);
    });
  });
  return ctx;
}
async function newPage(ctx) {
  const p = await ctx.newPage();
  await p.exposeFunction('__cspReport', d => violations.push(`[${step}] ${d}`)).catch(() => {});
  await p.addInitScript(() => document.addEventListener('securitypolicyviolation', e => window.__cspReport(`${e.violatedDirective} ${e.blockedURI || 'inline'}`)));
  p.on('console', m => {
    const t = m.text();
    if (/Content[- ]Security[- ]Policy|Refused to/i.test(t)) violations.push(`[${step}] ${t.slice(0, 200)}`);
    else if (m.type() === 'error' && !/favicon|ERR_INTERNET_DISCONNECTED|ERR_CONNECTION_REFUSED|ERR_FAILED|net::ERR|EventSource|htmx:sendError|htmx:afterRequest/.test(t)) failures.push(`[${step}] console error: ${t.slice(0, 200)}`);
  });
  p.on('pageerror', e => failures.push(`[${step}] pageerror: ${e.message}`));
  // every badge refetch the bell makes on its own is background traffic (never extends an idle session)
  let firstBadge = true;
  p.on('request', r => {
    if (!r.url().includes('/ui/notifications/badge')) return;
    if (firstBadge) { firstBadge = false; return; } // the page's own hx-trigger="load"
    if (!r.headers()['x-weave-background']) failures.push(`[${step}] badge refetch without X-Weave-Background`);
  });
  p.setDefaultTimeout(8000);
  return p;
}
const state = p => p.evaluate(() => {
  const c = document.querySelector('#notif-badge .notify-count'), bell = document.getElementById('notif-bell');
  return { count: c ? +c.textContent : 0, lit: bell.classList.contains('is-lit'), isNew: bell.classList.contains('is-new'), title: document.title,
    label: bell.getAttribute('aria-label'), live: (document.getElementById('notif-live') || {}).textContent || '',
    esMade: window.__es.made.length, esLive: window.__es.live(), urls: window.__es.urls(), pulses: window.__pulses, events: window.__notifEvents };
});
const waitCount = (p, want, ms = 5000) => p.waitForFunction(w => {
  const c = document.querySelector('#notif-badge .notify-count'); return (c ? +c.textContent : 0) === w;
}, want, { timeout: ms });
const waitStream = p => p.waitForFunction(() => window.__es && window.__es.made.some(s => s.readyState === 1), null, { timeout: 5000 });

async function run(name, fn) {
  if (process.env.ONLY && !new RegExp(process.env.ONLY).test(name)) return;
  step = name;
  try { await fn(); console.log('ok   ', name); }
  catch (e) { failures.push(`[${name}] ${String(e.message).split('\n')[0]}`); console.log('FAIL ', name, '-', String(e.message).split('\n')[0]); }
}

// ---- scenarios ----------------------------------------------------------------------------------
await run('dashboard: idle tab lights up, once per notification', async () => {
  await readAll();
  const ctx = await newCtx(), p = await newPage(ctx);
  await p.goto(BASE + '/dashboard'); await waitStream(p); await p.waitForTimeout(400);
  let s = await state(p);
  ok(s.count === 0 && !s.lit && !/^\(\d/.test(s.title), 'starts unlit: ' + JSON.stringify(s));
  ok(s.esMade === 1 && s.urls[0].endsWith('/events'), 'one stream, user room only: ' + s.urls);
  const t0 = Date.now();
  await ping();
  await waitCount(p, 1);
  const dt = Date.now() - t0;
  s = await state(p);
  ok(s.lit && s.isNew && s.title.startsWith('(1) ') && s.label === 'Notifications, 1 unread', 'lit + pulsing + title: ' + JSON.stringify(s));
  await p.waitForFunction(() => document.getElementById('notif-live').textContent === 'New notification', null, { timeout: 2000 });
  await p.waitForFunction(() => !document.getElementById('notif-bell').classList.contains('is-new'), null, { timeout: 3000 });
  await ping(); await waitCount(p, 2); await p.waitForTimeout(1500);
  s = await state(p);
  ok(s.title.startsWith('(2) ') && s.pulses === 2 && s.events === 2 && s.esMade === 1 && s.esLive === 1, 'exactly once each: ' + JSON.stringify(s));
  console.log(`      (badge updated ${dt} ms after the comment)`);
  // marking read elsewhere (REST, another device) lowers the count live
  await readAll(); await waitCount(p, 0);
  s = await state(p);
  ok(!s.lit && !/^\(\d/.test(s.title) && s.label === 'Notifications' && s.pulses === 2, 'unlit after read-all: ' + JSON.stringify(s));
  await ctx.close();
});

await run('board page: one shared stream, board still live, bell lights', async () => {
  await readAll();
  const ctx = await newCtx(), p = await newPage(ctx);
  await p.goto(`${BASE}/boards/${board.id}`); await waitStream(p); await p.waitForTimeout(400);
  let s = await state(p);
  ok(s.esMade === 1 && s.urls[0].includes('rooms=board') && s.urls[0].includes(board.id), 'one stream carrying the board room: ' + s.urls);
  await ping(); await waitCount(p, 1); await p.waitForTimeout(800);
  s = await state(p);
  ok(s.lit && s.title.startsWith('(1) ') && s.pulses === 1 && s.events === 1, 'board tab bell: ' + JSON.stringify(s));
  const c2 = await api(B, 'POST', `/lists/${list.id}/cards`, { name: 'Live tile ' + stamp });
  await p.locator('#tile-' + c2.id).waitFor({ timeout: 5000 });
  s = await state(p);
  ok(s.esMade === 1 && s.esLive === 1, 'still one stream: ' + JSON.stringify(s));
  await ctx.close();
});

await run('/notifications: new ones offered, list not moved; mark read lowers the bell', async () => {
  await readAll();
  await ping(); // one read-state notification already listed
  const ctx = await newCtx(), p = await newPage(ctx);
  await p.goto(BASE + '/notifications'); await waitStream(p); await waitCount(p, 1);
  const before = await p.locator('.notify-item').count();
  ok(await p.locator('#notify-fresh').isHidden(), 'refresh button hidden at first');
  await ping(); await waitCount(p, 2);
  await p.locator('#notify-fresh').waitFor({ state: 'visible', timeout: 3000 });
  ok(await p.locator('.notify-item').count() === before, 'list untouched until asked');
  await p.waitForFunction(() => document.getElementById('notify-head-count').textContent.trim() === '2', null, { timeout: 2000 }); // heading = global count, live
  await p.locator('#notify-fresh').click();
  await p.waitForFunction(b => document.querySelectorAll('.notify-item').length === b + 1, before);
  ok(await p.locator('#notify-fresh').isHidden(), 'button hides after refresh');
  ok((await p.locator('#notify-head-count').textContent()).trim() === '2', 'heading count 2');
  await p.locator('.notify-item.unread .notify-btn').first().click();
  await waitCount(p, 1);
  let s = await state(p);
  ok(s.title.startsWith('(1) ') && s.lit, 'title follows mark read: ' + JSON.stringify(s));
  ok((await p.locator('#notify-head-count').textContent()).trim() === '1', 'heading count follows mark read');
  await p.getByRole('button', { name: 'Mark all as read' }).click();
  await waitCount(p, 0); await p.waitForTimeout(400);
  s = await state(p);
  ok(!s.lit && !/^\(\d/.test(s.title) && s.esMade === 1, 'all read: ' + JSON.stringify(s));
  ok((await p.locator('#notify-head-count').textContent()).trim() === '', 'heading count cleared');
  await ctx.close();
});

await run('reduced motion: state change, no animation', async () => {
  await readAll();
  const ctx = await newCtx({ reducedMotion: 'reduce' }), p = await newPage(ctx);
  await p.goto(BASE + '/dashboard'); await waitStream(p); await p.waitForTimeout(300);
  await ping(); await waitCount(p, 1);
  const anim = await p.evaluate(() => {
    const b = document.getElementById('notif-bell');
    return { isNew: b.classList.contains('is-new'), svg: getComputedStyle(b.querySelector('svg')).animationName, ring: getComputedStyle(b, '::after').animationName, lit: b.classList.contains('is-lit') };
  });
  ok(anim.isNew && anim.lit && anim.svg === 'none' && anim.ring === 'none', 'no animation: ' + JSON.stringify(anim));
  await p.waitForFunction(() => !document.getElementById('notif-bell').classList.contains('is-new'), null, { timeout: 3000 });
  await ctx.close();
});

await run('motion allowed: the pulse animates', async () => {
  await readAll();
  const ctx = await newCtx({ reducedMotion: 'no-preference' }), p = await newPage(ctx);
  await p.goto(BASE + '/dashboard'); await waitStream(p); await p.waitForTimeout(300);
  await ping(); await waitCount(p, 1);
  const a = await p.evaluate(() => { const b = document.getElementById('notif-bell'); return [getComputedStyle(b.querySelector('svg')).animationName, getComputedStyle(b, '::after').animationName]; });
  ok(a[0] === 'notif-swing' && a[1] === 'notif-ring', 'animations: ' + a);
  await ctx.close();
});

await run('dropped stream: catch-up refetch after reconnect', async () => {
  await readAll();
  const ctx = await newCtx(), p = await newPage(ctx);
  await p.goto(BASE + '/dashboard'); await waitStream(p); await p.waitForTimeout(300);
  let badgeGets = 0;
  p.on('request', r => { if (r.url().includes('/ui/notifications/badge')) badgeGets++; });
  // the stream drops and cannot reconnect for a moment; B's notification is published to nobody
  await p.route('**/events*', r => r.abort());
  if (process.env.WEAVE_RESTART) execSync(process.env.WEAVE_RESTART, { stdio: 'inherit', shell: '/bin/sh' });
  else await ctx.setOffline(true);
  await p.waitForTimeout(1500);
  await ping();
  ok(await unread() === 1, 'notification exists server-side');
  await p.waitForTimeout(1000);
  ok((await state(p)).count === 0, 'not delivered while disconnected');
  if (!process.env.WEAVE_RESTART) await ctx.setOffline(false);
  await p.unroute('**/events*');
  await waitCount(p, 1, 20000); // browser retry (~3 s) or our backoff (5 s)
  const s = await state(p);
  ok(s.lit && s.title.startsWith('(1) ') && badgeGets >= 1, 'caught up: ' + JSON.stringify(s) + ' badge GETs ' + badgeGets);
  ok(s.esLive === 1, 'one live stream after reconnect: ' + JSON.stringify(s));
  await ctx.close();
});

await run('hidden tab parks its stream and keeps the title fresh by polling', async () => {
  await readAll();
  const ctx = await newCtx(), p = await newPage(ctx);
  await p.clock.install();
  await p.goto(BASE + '/dashboard'); await waitStream(p); await p.waitForTimeout(300);
  const hide = h => p.evaluate(h => {
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => h });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => h ? 'hidden' : 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  }, h);
  await hide(true);
  await p.clock.runFor(2 * 60000 + 1000);
  let s = await state(p);
  ok(s.esLive === 0, 'parked after 2 min hidden: ' + JSON.stringify(s));
  await ping();
  await p.clock.runFor(61000);
  await waitCount(p, 1);
  s = await state(p);
  ok(s.title.startsWith('(1) '), 'hidden tab title updated by polling: ' + JSON.stringify(s));
  await hide(false);
  await p.clock.runFor(1000);
  await p.waitForFunction(() => window.__es.live() === 1, null, { timeout: 5000 });
  await ctx.close();
});

await browser.close();
for (const v of violations) console.log('CSP  ', v);
for (const f of failures) console.log('FAIL ', f);
console.log(`\n${violations.length} CSP violation(s), ${failures.length} failure(s)`);
process.exit(violations.length || failures.length ? 1 : 0);
