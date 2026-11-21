#!/usr/bin/env node
// Optional browser smoke test for the Content-Security-Policy (not run by `go test`).
//
// Walks every UI page and fragment of a running Weave in real Chromium (home, project, board incl.
// composers / drag-drop / list menu / quick edit / card sheet with its popovers, shortcuts and uploads,
// board settings, admin pages, settings, search, notifications, dashboard, calendar/map/media,
// share + public pages, login, exports) and fails if the browser reports ANY CSP violation
// (console message or `securitypolicyviolation` event), a page error, or a broken interaction.
//
//   WEAVE_URL=http://localhost:8080 WEAVE_TOKEN=wk_... node scripts/csp-smoke.mjs
//
// WEAVE_TOKEN must be an admin API key (e.g. `weave admin mint-key --email you@example.com --role admin`).
// It is sent as `Authorization: Bearer` on every browser request, so no OIDC login is needed. The
// script creates its own demo project ("CSP smoke ...") and leaves it behind.
//
// Needs Playwright (playwright-core) and a Chromium: PLAYWRIGHT_CORE=/path/to/playwright-core/index.mjs
// (default: import 'playwright-core'), CHROMIUM=/path/to/chrome (default: Playwright's own browser).
// Exit status 0 = zero violations and every step passed.

const BASE = (process.env.WEAVE_URL || 'http://localhost:8080').replace(/\/$/, '');
const TOKEN = process.env.WEAVE_TOKEN || '';
if (!TOKEN) { console.error('set WEAVE_TOKEN to an admin API key'); process.exit(2); }

const pw = await import(process.env.PLAYWRIGHT_CORE || 'playwright-core');
const chromium = pw.chromium || pw.default.chromium;
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM || undefined });

const violations = [];
const failures = [];
let step = '';

async function api(method, path, body) {
  const r = await fetch(BASE + '/api/v1' + path, {
    method, headers: { Authorization: 'Bearer ' + TOKEN, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const t = await r.text();
  let j; try { j = JSON.parse(t); } catch { j = {}; }
  if (!r.ok) throw new Error(`${method} ${path}: ${r.status} ${t.slice(0, 200)}`);
  return j.item ?? j;
}

async function newPage(ctx) {
  const p = await ctx.newPage();
  await p.exposeFunction('__cspReport', d => violations.push(`[${step}] ${d}`));
  await p.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', e =>
      window.__cspReport(`${e.violatedDirective} blocked ${e.blockedURI || 'inline'} @ ${e.sourceFile || ''}:${e.lineNumber || ''} ${(e.sample || '').slice(0, 80)}`));
  });
  p.on('console', m => {
    const t = m.text();
    if (/Content[- ]Security[- ]Policy|Refused to (execute|load|apply|evaluate|connect|frame)/i.test(t)) violations.push(`[${step}] console: ${t.slice(0, 300)}`);
    else if (m.type() === 'error' && !/favicon|Failed to load resource.*40[134]/.test(t)) failures.push(`[${step}] console error: ${t.slice(0, 200)}`);
  });
  p.on('response', r => { if (r.status() === 429) failures.push(`[${step}] HTTP 429 (rate limit tripped): ${r.url()}`); });
  p.on('pageerror', e => failures.push(`[${step}] pageerror: ${e.message}`));
  p.setDefaultTimeout(6000);
  return p;
}

async function run(name, fn) {
  if (process.env.ONLY && !new RegExp(process.env.ONLY).test(name)) return;
  step = name;
  try { await fn(); console.log('ok   ', name); }
  catch (e) { failures.push(`[${name}] ${String(e.message).split('\n')[0]}`); console.log('FAIL ', name, '-', String(e.message).split('\n')[0]); }
}

const settle = p => p.waitForTimeout(500);
const esc = p => p.keyboard.press('Escape');

// ---- seed ----------------------------------------------------------------------------------
const stamp = Date.now().toString(36);
const proj = await api('POST', '/projects', { name: 'CSP smoke ' + stamp, description: 'demo' });
const board = await api('POST', `/projects/${proj.id}/boards`, { name: 'Smoke board' });
const lists = [];
for (const n of ['To do', 'Doing', 'Done']) lists.push(await api('POST', `/boards/${board.id}/lists`, { name: n }));
const label = await api('POST', `/boards/${board.id}/labels`, { name: 'Urgent', color: 'berry-red' }).catch(() => null);
const cards = [];
for (const n of ['First card', 'Second card', 'Third card']) cards.push(await api('POST', `/lists/${lists[0].id}/cards`, { name: n, description: 'Some **markdown** text [link](https://example.com)', dueDate: '2030-01-15' }));
await api('POST', `/lists/${lists[1].id}/cards`, { name: 'In flight' });
const card = cards[0];
await api('POST', `/cards/${card.id}/comments`, { text: 'a comment' }).catch(() => {});
await api('PUT', `/cards/${card.id}/location`, { name: 'Somewhere', lat: 52.5, lng: 13.4 }).catch(() => {});
const tl = await api('POST', `/cards/${card.id}/task-lists`, { name: 'Checklist' });
const task = await api('POST', `/task-lists/${tl.id}/tasks`, { name: 'one task' });
const share = await api('POST', `/cards/${card.id}/share`, {}).catch(() => null);
const pub = await api('POST', `/boards/${board.id}/public`, { isPublic: true }).catch(() => null);
const pubToken = pub && (pub.publicToken || pub.token);
const shareToken = share && (share.shareToken || share.token);
await api('POST', `/boards/${board.id}/filters`, { name: 'my filter', query: 'x' }).catch(() => {});

const authed = await browser.newContext({ viewport: { width: 1280, height: 800 }, extraHTTPHeaders: { Authorization: 'Bearer ' + TOKEN, 'X-Requested-With': 'weave' }, acceptDownloads: true });
const anon = await browser.newContext({ viewport: { width: 1280, height: 800 } });
const p = await newPage(authed);

const go = async path => { const r = await p.goto(BASE + path); if (r && r.status() >= 400) throw new Error(`${path} -> HTTP ${r.status()}`); await settle(p); };

// Click every matching element once (re-querying after each click), pressing Escape in between.
async function clickEach(sel, { skip = /delete|remove|archive|sign out|logout|revoke|deactivate|reindex|clear|rotate|reset/i, max = 40 } = {}) {
  const n = Math.min(await p.locator(sel).count(), max);
  for (let i = 0; i < n; i++) {
    const el = p.locator(sel).nth(i);
    if (!(await el.isVisible().catch(() => false))) continue;
    const label = ((await el.innerText().catch(() => '')) + ' ' + ((await el.getAttribute('aria-label').catch(() => '')) || '') + ' ' + ((await el.getAttribute('hx-post').catch(() => '')) || '') + ((await el.getAttribute('hx-delete').catch(() => '')) || '')).trim();
    if (skip.test(label)) continue;
    await el.click({ timeout: 2500 }).catch(() => {});
    await p.waitForTimeout(250);
  }
}

// ---- canary: prove the detector works (an inline script must be blocked and reported) ------
await run('canary: an injected inline script is blocked and detected', async () => {
  await go('/login');
  const before = violations.length;
  await p.evaluate(() => { const s = document.createElement('script'); s.textContent = 'window.__canary = 1'; document.head.appendChild(s); });
  await settle(p);
  const ran = await p.evaluate(() => window.__canary === 1);
  const seen = violations.length - before;
  violations.length = before; // expected violation: not counted
  if (process.env.WEAVE_CSP_EXPECT === 'off') return; // escape hatch mode: nothing is enforced
  if (ran || seen === 0) throw new Error(`inline script ran=${ran}, violations reported=${seen}`);
});

// ---- anonymous pages -----------------------------------------------------------------------
await run('anon: login + terms + healthz', async () => {
  const a = await newPage(anon);
  await a.goto(BASE + '/login'); await a.waitForTimeout(400);
  await a.goto(BASE + '/login?next=%2F'); await a.waitForTimeout(300);
  await a.goto(BASE + '/healthz');
  if (pubToken) { await a.goto(BASE + '/public/' + pubToken); await a.waitForTimeout(600); }
  if (shareToken) { await a.goto(BASE + '/share/' + shareToken); await a.waitForTimeout(600); }
  await a.goto(BASE + '/share/nope'); await a.goto(BASE + '/public/nope');
});

// ---- plain pages ---------------------------------------------------------------------------
for (const path of ['/', `/projects/${proj.id}`, `/boards/${board.id}`, `/boards/${board.id}/calendar`, `/boards/${board.id}/map`,
  `/boards/${board.id}/media`, `/boards/${board.id}/settings`, `/cards/${card.id}`, `/lists/${lists[0].id}`, `/ui/lists/${lists[0].id}`, '/dashboard', '/notifications', '/notification-services',
  '/search?q=card', '/settings', '/settings?section=apikey', '/admin', '/admin/users', '/admin/agents', '/admin/groups', '/admin/sso', '/admin/config', '/admin/webhooks',
  '/admin/agent-prompts', '/admin/agent-prompts?tab=blocks', `/projects/${proj.id}/settings?section=agents`, '/admin/backup', `/ui/boards/${board.id}/activity`, `/ui/projects/${proj.id}/card-templates`, `/ui/boards/${board.id}/inbox`]) {
  await run('page ' + path, async () => {
    if (path === '/admin/backup') { await p.request.get(BASE + path); return; } // a download, not a page
    await go(path);
    await clickEach('summary, [role=tab], button[hx-get], a[hx-get]');
    await esc(p);
  });
}

// ---- home / project dialogs ----------------------------------------------------------------
await run('home: new project dialog + cancel', async () => {
  await go('/');
  await p.locator('[hx-get="/ui/projects/new"]').first().click(); await settle(p);
  await p.locator('dialog[open] [data-close-dialog]').click(); await settle(p);
  if (await p.locator('dialog[open]').count()) throw new Error('Cancel did not close the dialog (data-close-dialog)');
  await p.fill('#projects-filter input[name=search]', 'CSP'); await settle(p);
});
await run('project: template gallery (search, preview, create from a built-in, blank and import panes)', async () => {
  await go(`/projects/${proj.id}`);
  await p.locator(`[hx-get="/ui/projects/${proj.id}/boards/new"]`).first().click(); await settle(p);
  await p.waitForSelector('dialog[open] .tg-grid');
  const tiles = await p.locator('dialog[open] .tg-tile').count();
  if (tiles < 15) throw new Error(`expected blank + 14 built-in tiles, saw ${tiles}`);
  await p.locator('#tg-q').pressSequentially('hiring'); await settle(p);
  if (await p.locator('dialog[open] .tg-tile').count() !== 1) throw new Error('search did not narrow the grid to one tile');
  await p.fill('#tg-q', ''); await settle(p);
  await p.locator('dialog[open] input[name=category][value=software]').check({ force: true }); await settle(p);
  if (await p.locator('dialog[open] .tg-tile').count() !== 3) throw new Error('software category should list 3 templates');
  await p.locator('dialog[open] .tg-tile', { hasText: 'Sprint (Scrum)' }).click(); await settle(p);
  await p.waitForSelector('dialog[open] .tg-create');
  const cols = await p.locator('dialog[open] .tg-col').count();
  if (cols !== 5) throw new Error(`sprint preview should show 5 columns, saw ${cols}`);
  await p.locator('dialog[open] .tg-back').click(); await settle(p);
  await p.locator('dialog[open] .tg-cat-import').click(); await settle(p);
  await p.waitForSelector('dialog[open] input[type=file]');
  await p.locator('dialog[open] .tg-back').click(); await settle(p);
  await p.locator('dialog[open] input[name=category][value=""]').check({ force: true }); await settle(p);
  await p.locator('dialog[open] .tg-tile-blank').click(); await settle(p);
  await p.waitForSelector('dialog[open] form[hx-post$="/boards"]');
  await p.locator('dialog[open] .tg-back').click(); await settle(p);
  await p.locator('dialog[open] .tg-tile', { hasText: 'Kanban basics' }).click(); await settle(p);
  await p.fill('dialog[open] input[name=name]', 'Smoke from template');
  await Promise.all([p.waitForURL(/\/boards\//, { timeout: 8000 }), p.locator('dialog[open] .tg-go').click()]);
  await settle(p);
  if (await p.locator('.boards-col').count() < 4) throw new Error('board created from the template is missing its columns');
  if (await p.locator('dialog[open]').count()) throw new Error('a card or dialog opened after creating the board');
  await go(`/projects/${proj.id}`);
  await p.locator(`[hx-get="/ui/projects/${proj.id}/boards/new"]`).first().click(); await settle(p);
  await esc(p); await settle(p);
  await p.locator(`a[href="/projects/${proj.id}/settings"]`).first().click(); await settle(p);
  await p.waitForSelector('.projects-settings .bs-nav');
  await clickEach('.projects-settings .bs-nav a[data-section]'); await settle(p);
  for (const sec of ['general', 'background', 'managers', 'fields', 'agents']) { await go(`/projects/${proj.id}/settings?section=${sec}`); await clickEach(`#set-${sec} [data-add], #set-${sec} .ps-swatch`, { max: 3 }); await settle(p); }
  await p.locator('.bs-nav a[data-section=danger]').click();
  await p.fill('#ps-del-in', 'x'); if (!(await p.locator('#set-danger [data-del]').isDisabled())) throw new Error('project delete enabled without the typed name');
});

// ---- board ---------------------------------------------------------------------------------
await run('board: composers (add list, add card)', async () => {
  await go(`/boards/${board.id}`);
  await p.locator('.boards-composer-open, .boards-composer > button, [data-composer-open]').first().click().catch(() => {});
  await clickEach('.boards-composer > button, .boards-composer summary, [data-open-composer]');
  const inp = p.locator('.boards-composer.is-open input[name=name], .boards-composer.is-open textarea[name=name]').first();
  if (await inp.count()) { await inp.fill('Added by smoke'); await inp.press('Enter'); await settle(p); }
});
await run('board: bulk paste of 100 cards with a second tab open (default rate limits must not trip)', async () => {
  const other = await newPage(authed); // second tab on the same board: its live updates refetch while we paste
  await other.goto(BASE + `/boards/${board.id}`); await other.waitForTimeout(500);
  await go(`/boards/${board.id}`);
  const text = Array.from({ length: 100 }, (_, i) => `Pasted card ${i + 1}`).join('\n');
  const comp = p.locator('.boards-composer[data-composer=card]').first();
  await comp.locator('[data-open]').first().click();
  await p.evaluate(t => {
    const ta = document.querySelector('.boards-composer.is-open [data-composer-input]');
    const dt = new DataTransfer(); dt.setData('text', t);
    ta.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  }, text);
  await p.waitForTimeout(2500);
  const n = await p.locator('.boards-tile', { hasText: 'Pasted card' }).count();
  if (n < 100) throw new Error(`only ${n}/100 pasted cards appeared`);
  await other.waitForTimeout(1500); await other.close();
});
await run('board: drag and drop a card to another list', async () => {
  await go(`/boards/${board.id}`);
  const src = p.locator('.boards-tile').first(); const dst = p.locator('.boards-list').nth(2);
  await src.dragTo(dst, { timeout: 5000 }).catch(() => {}); await settle(p);
  await p.locator('.boards-list').first().dragTo(p.locator('.boards-list').nth(2), { timeout: 5000 }).catch(() => {}); await settle(p);
});
await run('board: drag the empty background to pan sideways (mouse)', async () => {
  await go(`/boards/${board.id}`);
  // make the strip overflow with an inert spacer, then drag the bare background (past the last column's right edge)
  await p.evaluate(() => { const l = document.querySelector('.boards-lists'); const s = document.createElement('div'); s.id = 'pan-spacer'; s.style.cssText = 'flex:none;width:4000px;height:1px'; l.appendChild(s); });
  const box = await p.evaluate(() => { const l = document.querySelector('.boards-lists').getBoundingClientRect(); const c = document.querySelector('.boards-col').getBoundingClientRect(); return { x: l.left + 8, y: Math.max(c.bottom, l.top) + 20, bottom: l.bottom, hx: c.left + 60, hy: c.top + 14 }; });
  const left = () => p.evaluate(() => document.querySelector('.boards-lists').scrollLeft);
  const drag = async (x, y) => { await p.mouse.move(x, y); await p.mouse.down(); await p.mouse.move(x - 250, y, { steps: 10 }); await p.mouse.up(); await settle(p); };
  const gx = await p.evaluate(() => { const c = document.querySelectorAll('.boards-col'); return (c[0].getBoundingClientRect().right + c[1].getBoundingClientRect().left) / 2; });
  const gy = await p.evaluate(() => document.querySelector('.boards-col').getBoundingClientRect().top + 40);
  await drag(gx, gy);
  if ((await left()) < 100) throw new Error('dragging the gap between columns did not pan the board');
  await p.evaluate(() => { document.querySelector('.boards-lists').scrollLeft = 0; }); await settle(p);
  await drag(box.hx, box.hy);                                   // a column header must not pan (it drags the list)
  if ((await left()) > 5) throw new Error('dragging a column header panned the board');
});
await run('board: list menu, quick edit, tile hover buttons', async () => {
  await go(`/boards/${board.id}`);
  await clickEach('.boards-list details.boards-menu > summary, .boards-menu-btn, summary.boards-menu-btn');
  await esc(p);
  await clickEach('.boards-tile .boards-quick-btn, .boards-tile [aria-label*=Edit], .boards-tile [data-quick]');
  await esc(p);
  await p.keyboard.press('?'); await settle(p); await esc(p);
  for (const k of ['j', 'k', 'f', 'n', 'c', 'e', 'Escape', 'Enter', 'Escape']) { await p.keyboard.press(k).catch(() => {}); await p.waitForTimeout(120); }
});
await run('board: filter dropdowns', async () => {
  await go(`/boards/${board.id}`);
  await clickEach('.boards-filterbar summary', { max: 25 });
  await esc(p);
});
await run('board: view icons in the top bar, 1-5 keys, switcher stays on calendar/map/settings', async () => {
  await go(`/boards/${board.id}`);
  const cur = sel => p.locator(`.topbar .view-switch a[aria-current=page]${sel || ''}`).count();
  if ((await cur('[data-view=kanban]')) !== 1) throw new Error('kanban icon is not the current view');
  await p.locator('.topbar .view-switch a[data-view=grid]').click(); await p.waitForURL(/view=grid/); await settle(p);
  if ((await cur('[data-view=grid]')) !== 1) throw new Error('grid icon not current after click');
  await p.keyboard.press('1'); await p.waitForURL(/view=kanban/); await settle(p);
  await p.keyboard.press('2'); await p.waitForURL(/view=list/); await settle(p);
  await p.keyboard.press('3'); await p.waitForURL(/view=grid/); await settle(p);
  await p.keyboard.press('4'); await p.waitForURL(/\/calendar/); await settle(p);
  if ((await cur('[data-view=calendar]')) !== 1) throw new Error('calendar page lost the switcher');
  await go(`/boards/${board.id}`);                                  // the calendar page has no board shortcuts
  await p.keyboard.press('5'); await p.waitForURL(/\/map/); await settle(p);
  if ((await cur('[data-view=map]')) !== 1) throw new Error('map page lost the switcher');
  await go(`/boards/${board.id}/settings`);
  if ((await p.locator('.topbar .view-switch a[data-view]').count()) !== 5) throw new Error('settings page has no view switcher');
  if ((await p.locator('.topbar-crumbs a').count()) < 2) throw new Error('settings page has no breadcrumbs');
  await go(`/boards/${board.id}`);
  const h = await p.evaluate(() => ({ crumbs: document.querySelector('.topbar-crumbs h1')?.textContent, h1: document.querySelectorAll('h1').length }));
  if (h.crumbs !== 'Smoke board' || h.h1 !== 1) throw new Error('board name must be the single h1 in the breadcrumb: ' + JSON.stringify(h));
});

await run('template gallery @390px: full-height sheet, category chips, preview, no sideways scroll', async () => {
  const mctx = await browser.newContext({ viewport: { width: 390, height: 844 }, extraHTTPHeaders: { Authorization: 'Bearer ' + TOKEN, 'X-Requested-With': 'weave' } });
  const m = await newPage(mctx);
  try {
    await m.goto(BASE + `/projects/${proj.id}`); await settle(m);
    await m.locator(`[hx-get="/ui/projects/${proj.id}/boards/new"]`).first().click(); await settle(m);
    await m.waitForSelector('dialog[open] .tg-grid');
    const dim = async () => m.evaluate(() => ({ sw: document.documentElement.scrollWidth, w: innerWidth, h: document.querySelector('dialog#modal').getBoundingClientRect().height, ih: innerHeight }));
    let d = await dim();
    if (d.sw > d.w) throw new Error(`gallery scrolls sideways: ${d.sw} > ${d.w}`);
    if (Math.abs(d.h - d.ih) > 2) throw new Error(`gallery is not full height: ${d.h} vs ${d.ih}`);
    await m.locator('dialog[open] .tg-tile', { hasText: 'Hiring pipeline' }).click(); await settle(m);
    await m.waitForSelector('dialog[open] .tg-create');
    d = await dim();
    if (d.sw > d.w) throw new Error(`preview scrolls sideways: ${d.sw} > ${d.w}`);
  } finally { await mctx.close(); }
});

// ---- phone: one-row top bar, view picker in the filter row, theme switch in the account menu -------------
await run('board @390px: one-row top bar, view picker, account menu theme, no sideways scroll', async () => {
  const mctx = await browser.newContext({ viewport: { width: 390, height: 800 }, extraHTTPHeaders: { Authorization: 'Bearer ' + TOKEN, 'X-Requested-With': 'weave' } });
  const m = await newPage(mctx);
  try {
    await m.goto(BASE + `/boards/${board.id}`); await settle(m);
    const geo = () => m.evaluate(() => { const t = document.querySelector('.topbar'); return { h: Math.round(t.getBoundingClientRect().height), tsw: t.scrollWidth, tcw: t.clientWidth, dsw: document.documentElement.scrollWidth, iw: innerWidth }; });
    let g = await geo();
    if (g.h !== 56 || g.tsw > g.tcw || g.dsw > g.iw) throw new Error('top bar wraps or page scrolls sideways: ' + JSON.stringify(g));
    if (await m.locator('.topbar > .theme-toggle').isVisible()) throw new Error('theme toggle should be hidden in the top bar on phones');
    if (await m.locator('.topbar .view-switch').isVisible()) throw new Error('the top-bar view switcher should be hidden on phones');
    if (await m.locator('.boards-help-btn').isVisible()) throw new Error('the ? button should be hidden on phones');
    await m.locator('.view-pick > summary').click(); await m.waitForTimeout(250);
    const pop = m.locator('.view-pick-pop, #popover-root .view-pick-pop').first();
    if (!(await pop.isVisible())) throw new Error('view picker did not open');
    const bb = await pop.boundingBox(); if (!bb || bb.x < 0 || bb.x + bb.width > 390 || bb.y + bb.height > 800) throw new Error('view picker is clipped: ' + JSON.stringify(bb));
    await m.keyboard.press('Escape'); await m.waitForTimeout(150);
    await m.locator('#user-menu > summary').click(); await m.waitForTimeout(250);
    await m.locator('#user-menu [data-theme-set=dark]').click(); await m.waitForTimeout(250);
    if ((await m.evaluate(() => document.documentElement.dataset.theme)) !== 'dark') throw new Error('theme switch in the account menu did not apply');
    await m.locator('#user-menu [data-theme-set=system]').click();
    await m.keyboard.press('Escape');
    g = await geo();
    if (g.h !== 56 || g.tsw > g.tcw || g.dsw > g.iw) throw new Error('after the menus: ' + JSON.stringify(g));
  } finally { await mctx.close(); }
});

// ---- column sheet --------------------------------------------------------------------------
await run('column sheet: open from header, WIP popover, description editor, sort, menu, prev/next, close', async () => {
  await api('PATCH', `/lists/${lists[0].id}`, { description: 'Column brief', wipLimit: 1 });
  await go(`/boards/${board.id}`);
  await p.locator(`#col-${lists[0].id} button.boards-col-settings`).click(); await p.locator('#list-sheet').waitFor(); await settle(p);
  if (!/\/lists\//.test(p.url())) throw new Error('sheet did not push /lists/{id}');
  await p.locator('#list-sheet details[data-pop=wip] > summary').click();
  await p.locator('#list-sheet [data-ls-wip-input]').fill('2');
  await p.locator('#list-sheet form[data-ls-form=wip] button.primary').click(); await settle(p);
  await p.locator('#list-sheet [data-ls-desc-edit]').first().click();
  await p.locator('#list-sheet [data-ls-desc-input]').fill('Column brief, edited'); await p.keyboard.press('Control+Enter'); await settle(p);
  await p.locator('#list-sheet [data-ls-form=sort] button').click(); await settle(p);
  await clickEach('#list-sheet summary', { max: 8 });
  await p.locator('#list-sheet [data-ls-sec-toggle]').first().click();
  await p.locator('#list-sheet [data-ls-nav]:not([disabled])').first().click(); await settle(p);
  await esc(p); await settle(p);
  if (/\/lists\//.test(p.url())) throw new Error('closing the sheet did not restore the board URL');
  await go(`/lists/${lists[0].id}`); await p.locator('#list-sheet').waitFor(); await esc(p);
});

// ---- connect an agent ------------------------------------------------------------------------
await run('connect an agent: open from the board, tabs, toggles refetch, edit + reset, copy, column sheet and project entry points', async () => {
  await go(`/boards/${board.id}`);
  await p.locator('.boards-agent-btn').click(); await p.locator('#ap').waitFor(); await settle(p);
  const tabs = await p.locator('.ap-tab').allTextContents();
  if (tabs.join('|') !== 'About Weave|Project|Board|Runner') throw new Error('tabs: ' + tabs.join('|'));
  const board0 = await p.locator('#ap-text').inputValue();
  if (!/PLAN board/.test(board0) || /wk_[A-Za-z0-9]{8,}/.test(board0)) throw new Error('board prompt missing PLAN or contains a key');
  await p.locator('.ap-tab', { hasText: 'Runner' }).click(); await p.locator('#ap[data-scope=runner]').waitFor(); await settle(p);
  if (!/weave-handle\.sh/.test(await p.locator('#ap-text').inputValue())) throw new Error('runner prompt lacks the wrapper script');
  await p.locator('input[name=noinstall]').check(); await p.waitForFunction(() => document.querySelector('#ap input[name=noinstall]:checked') && !/install\.sh \| sh/.test(document.querySelector('#ap-text').value)); 
  await p.locator('#ap-text').fill('edited'); await p.locator('.ap-reset').click();
  if ((await p.locator('#ap-text').inputValue()) === 'edited') throw new Error('Reset did not restore the prompt');
  await p.locator('.ap-copy').click(); await p.waitForFunction(() => /Copied/.test(document.querySelector('.ap-copy').textContent), null, { timeout: 4000 });
  await p.locator('.ap-tab', { hasText: 'About' }).click(); await p.locator('#ap[data-scope=general]').waitFor();
  await esc(p); await settle(p);
  await go(`/lists/${lists[0].id}`); await p.locator('#list-sheet').waitFor();
  await p.locator('#list-sheet button:has-text("Get the prompt")').click(); await p.locator('#ap[data-scope=list]').waitFor(); await esc(p); await settle(p);
  await go(`/projects/${proj.id}`);
  await p.locator('button:has-text("Connect an agent")').first().click(); await p.locator('#ap[data-scope=project]').waitFor(); await esc(p); await settle(p);
  await go(`/boards/${board.id}/settings?section=agents`);
  await p.locator('#set-agents button:has-text("Runner prompt")').click(); await p.locator('#ap[data-scope=runner]').waitFor(); await esc(p); await settle(p);
});
await run('agent prompt texts: board override autosaves, the dialog notes it, reset; admin Prompts and Blocks tabs save, preview and reset', async () => {
  await go(`/boards/${board.id}/settings?section=agents`);
  const ta = p.locator('#bs-ap');
  if (!/\{questionnaire\}/.test(await ta.inputValue())) throw new Error('board prompt editor does not show the default text');
  await ta.fill('Smoke prompt for {board_name}\n{columns}'); await ta.blur();
  await p.locator('#set-agents .bs-prompt-badge').waitFor(); await settle(p);
  await p.locator('#set-agents details.bs-tokens summary').click(); await p.locator('#set-agents details.bs-tokens[open]').waitFor();
  await p.locator('#set-agents button:has-text("Preview")').click(); await p.locator('#ap[data-scope=board] .ap-custom').waitFor();
  if (!/^Smoke prompt for "Smoke board"/.test(await p.locator('#ap-text').inputValue())) throw new Error('dialog does not use the board text');
  await esc(p); await settle(p);
  p.once('dialog', d => d.accept());
  await p.locator('#set-agents button:has-text("Reset to default")').click();
  await p.waitForFunction(() => !document.querySelector('#set-agents .bs-prompt-badge')); await settle(p);
  // admin: Prompts tab (expand, edit, autosave, preview, reset)
  await go('/admin/agent-prompts');
  if (!(await p.locator('.adm-tabs a[aria-current]').textContent()).startsWith('Prompts')) throw new Error('prompts is not the default tab');
  await p.locator('#ap-general > summary').click();
  const g = p.locator('#ap-general-text');
  await g.fill('Smoke general {server}'); await g.blur(); await p.locator('#ap-general[open] .bs-prompt-badge').waitFor(); await settle(p);
  await p.locator('#ap-general .bs-tokens summary').click(); await p.locator('#ap-general .bs-tokens[open]').waitFor();
  await p.locator('#ap-general button:has-text("Preview")').click(); await p.locator('#ap-general .adm-ap-preview pre').waitFor();
  if (!/^Smoke general http/.test(await p.locator('#ap-general .adm-ap-preview pre').textContent())) throw new Error('prompt preview does not render the text');
  p.once('dialog', d => d.accept());
  await p.locator('#ap-general button:has-text("Reset to default")').click();
  await p.waitForFunction(() => !document.querySelector('#ap-general .bs-prompt-badge')); await settle(p);
  await p.locator('#ap-runner > summary').click(); await p.locator('#ap-runner button:has-text("Preview")').click(); await p.locator('#ap-runner .adm-ap-preview pre').waitFor(); await settle(p);
  // admin: Blocks tab (tab link keeps the URL, edit a block, the Connect an agent dialog notes it, reset)
  await p.locator('.adm-tabs a:has-text("Blocks")').click(); await p.waitForURL(/tab=blocks/); await settle(p);
  await p.locator('#ab-security > summary').click();
  const sb = p.locator('#ab-security-text');
  await sb.fill('Security\n  - Smoke rules for {server}. {connect} stays literal.'); await sb.blur();
  await p.locator('#ab-security[open] .bs-prompt-badge').waitFor(); await settle(p);
  await p.locator('#ab-security button:has-text("Preview")').click(); await p.locator('#ab-security .adm-ap-preview pre').waitFor();
  if (!/Smoke rules for http\S+\. \{connect\} stays literal/.test(await p.locator('#ab-security .adm-ap-preview pre').textContent())) throw new Error('block preview');
  await p.reload(); await settle(p);
  if (!/tab=blocks/.test(p.url()) || !(await p.locator('.adm-tabs a[aria-current]').textContent()).startsWith('Blocks')) throw new Error('tab not kept on reload');
  await go(`/boards/${board.id}`);
  await p.locator('.boards-agent-btn').click(); await p.locator('#ap .ap-custom').waitFor();
  if (!/Smoke rules for/.test(await p.locator('#ap-text').inputValue())) throw new Error('dialog does not use the block');
  await p.locator('#ap .ap-custom a[href*="tab=blocks"]').click(); await p.waitForURL(/tab=blocks/); await p.locator('#ab-security[open]').waitFor(); await settle(p);
  p.once('dialog', d => d.accept());
  await p.locator('#ab-security button:has-text("Reset to default")').click();
  await p.waitForFunction(() => !document.querySelector('#ab-security .bs-prompt-badge')); await settle(p);
});
await run('connect an agent @390px: full-height sheet, scrolling tabs, copy pinned, no sideways scroll', async () => {
  const mctx = await browser.newContext({ viewport: { width: 390, height: 800 }, extraHTTPHeaders: { Authorization: 'Bearer ' + TOKEN, 'X-Requested-With': 'weave' } });
  const m = await newPage(mctx);
  try {
    await m.goto(BASE + `/boards/${board.id}`); await settle(m);
    await m.locator('.boards-agent-btn').click(); await m.locator('#ap').waitFor(); await settle(m);
    const g = await m.evaluate(() => { const d = document.querySelector('dialog#modal').getBoundingClientRect(); const c = document.querySelector('.ap-copy').getBoundingClientRect(); return { dw: Math.round(d.width), dh: Math.round(d.height), cb: Math.round(c.bottom), iw: innerWidth, ih: innerHeight, sw: document.documentElement.scrollWidth }; });
    if (g.dw !== g.iw || g.dh < g.ih - 2 || g.cb > g.ih || g.sw > g.iw) throw new Error('phone layout: ' + JSON.stringify(g));
  } finally { await mctx.close(); }
});

// ---- card sheet ----------------------------------------------------------------------------
await run('card sheet: open, sections, popovers, shortcuts', async () => {
  await go(`/boards/${board.id}`);
  await p.locator(`#tile-${card.id}`).click(); await p.locator('#card-modal').waitFor(); await p.waitForTimeout(1200);
  await clickEach('#card-modal summary', { max: 30 });
  await esc(p);
  await clickEach('#card-modal [role=tab], #card-modal button[hx-get], #card-modal [data-section] > button, #card-modal .cm-sec-head button', { max: 30 });
  for (const k of ['m', 'l', 'd', 'a', 'Escape']) { await p.keyboard.press(k).catch(() => {}); await p.waitForTimeout(150); }
});
await run('card sheet: description edit by click (weave:edit trigger) and link click does not edit', async () => {
  await go(`/cards/${card.id}`);
  await p.waitForTimeout(800);
  const view = p.locator('.cm-desc-view.is-editable').first();
  await view.locator('a').first().click({ modifiers: [], trial: true }).catch(() => {});
  await view.click({ position: { x: 5, y: 5 } });
  await p.locator('#card-modal textarea[name=description], textarea[name=description]').first().waitFor({ timeout: 4000 });
  await esc(p);
});
await run('card sheet: share link select', async () => {
  await go(`/cards/${card.id}`); await p.waitForTimeout(800);
  const share = p.locator('input[data-select-on-click]').first();
  if (await share.count()) await share.click({ timeout: 1500 }).catch(() => {});
});
await run('card sheet: checklist toggle sends isCompleted (hx-vals js: replacement)', async () => {
  await go(`/cards/${card.id}`); await p.waitForTimeout(1200);
  const cb = p.locator('input[type=checkbox][data-bool-param]').first();
  await cb.check(); await settle(p);
  let t = await api('GET', `/task-lists/${tl.id}`).catch(() => null);
  const lists2 = await api('GET', `/cards/${card.id}/task-lists`);
  const arr = Array.isArray(lists2) ? lists2 : lists2.items || [];
  const done = JSON.stringify(arr).includes('"isCompleted":true');
  if (!done) throw new Error('checkbox did not mark the task completed');
  await p.locator('input[type=checkbox][data-bool-param]').first().uncheck(); await settle(p);
});
await run('card sheet: upload attachment + attachments/media pages', async () => {
  await go(`/cards/${card.id}`); await p.waitForTimeout(1200);
  const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64');
  const fi = p.locator('input[type=file]').first();
  await fi.setInputFiles({ name: 'dot.png', mimeType: 'image/png', buffer: png }); await p.waitForTimeout(1500);
  await go(`/boards/${board.id}/media`); await clickEach('a[hx-get]');
  await go(`/boards/${board.id}`);
});
await run('card sheet: paste / drop attachment handlers load without CSP issues', async () => {
  await go(`/boards/${board.id}`);
  await p.locator(`#tile-${card.id}`).click(); await p.waitForTimeout(1200);
  await p.evaluate(() => { const m = document.getElementById('card-modal'); m && m.dispatchEvent(new DragEvent('dragover', { bubbles: true })); });
});

// ---- board settings, admin, settings pages (all sections) ----------------------------------
await run('board settings: every section', async () => {
  await go(`/boards/${board.id}/settings`);
  await clickEach('button, summary, a[hx-get]', { max: 60 });
});
for (const path of ['/settings', '/admin/users', '/admin/agents', '/admin/groups', '/admin/sso', '/admin/config', '/admin/webhooks', '/admin/agent-prompts', '/admin/agent-prompts?tab=blocks', '/notification-services', '/dashboard']) {
  await run('forms on ' + path, async () => {
    await go(path);
    await clickEach('button:not([type=submit]), summary', { max: 25 });
    await p.locator('select[name=theme]').selectOption('dark').catch(() => {});
    await p.locator('[data-theme-set=dark]').first().click().catch(() => {}); await settle(p);
    await p.locator('[data-theme-set=light]').first().click().catch(() => {});
  });
}
await run('search + notifications', async () => {
  await go('/search?q=First'); await p.fill('input[name=q]', 'card').catch(() => {}); await p.keyboard.press('Enter'); await settle(p);
  await go('/notifications'); await clickEach('button[hx-post]', { max: 5, skip: /nothing/ });
});

// ---- exports (own sandbox CSP; the print button is the only script) ------------------------
await run('export: board + card print view (print button)', async () => {
  for (const u of [`/export/boards/${board.id}?format=pdf&inline=1`, `/export/cards/${card.id}?format=pdf&inline=1`, `/export/boards/${board.id}?inline=1`]) {
    await p.goto(BASE + u); await settle(p);
    const btn = p.locator('#weave-print');
    if (u.includes('pdf')) {
      await p.evaluate(() => { window.__printed = 0; window.print = () => { window.__printed++; }; }).catch(() => {});
      await btn.click();
      const printed = await p.evaluate(() => window.__printed).catch(() => -1);
      if (printed !== 1) throw new Error(`print button did not call window.print (count ${printed})`);
    }
  }
});

// ---- sign-out (last: ends the session of an API-key browser only if cookies were used) -----------
await run('sign-out form posts and redirects to /login on the same origin', async () => {
  await go('/');
  const [resp] = await Promise.all([p.waitForResponse(r => r.url().endsWith('/logout') && r.request().method() === 'POST'), p.locator('#user-menu summary').click().then(() => p.locator('#user-menu button:has-text("Sign out")').click())]);
  if (resp.status() !== 303 || !/\/login/.test(resp.headers().location || '')) throw new Error(`logout -> ${resp.status()} ${resp.headers().location}`);
  await settle(p);
});

// ---- theme init script / htmx config -------------------------------------------------------
await run('theme init script + htmx allowEval=false', async () => {
  await go('/');
  const cfg = await p.evaluate(() => ({ eval: htmx.config.allowEval, cls: document.documentElement.className }));
  if (cfg.eval !== false) throw new Error('htmx.config.allowEval should be false');
  if (!/\bjs\b/.test(cfg.cls)) throw new Error('theme-init.js did not run (no "js" class)');
});

await browser.close();
console.log(`\nCSP violations: ${violations.length}`);
violations.forEach(v => console.log('  VIOLATION', v));
console.log(`Failures: ${failures.length}`);
failures.forEach(f => console.log('  FAIL', f));
process.exit(violations.length || failures.length ? 1 : 0);
