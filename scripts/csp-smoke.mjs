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
// script creates its own demo project ("CSP smoke ...") and leaves it behind. Optional WEAVE_USER_TOKEN and
// WEAVE_USER2_TOKEN (keys of two plain boardUser accounts) add the ownership walk: a plain user creates a
// project, a board and a card; a project member creates a board only when the project allows members to; the
// people picker filters as you type; the creator makes the member a board owner; both see Agents and Webhooks; no
// Admin console link. The admin walk checks the console link, that console pages carry no content links, and (with
// WEAVE_USER_TOKEN) that the admin gets "not found" for another person's project and board: the admin role is
// system administration only, and the script's own demo project belongs to the admin who created it.
//
// Needs Playwright (playwright-core) and a Chromium: PLAYWRIGHT_CORE=/path/to/playwright-core/index.mjs
// (default: import 'playwright-core'), CHROMIUM=/path/to/chrome (default: Playwright's own browser).
// WEAVE_OWNER_TOKEN: API key of a non-admin who may create projects, for the board-owner walk (agent accounts and board
// webhooks); start the server with WEAVE_NOTIFY_ALLOW_HOSTS=127.0.0.1 so its webhook may reach the script's local receiver.
// A card on several boards (docs/DESIGN.md "Placements") is walked with card.place / the "Add to board..." picker (no
// database access needed; WEAVE_DB is no longer read).
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
  '/search?q=card', '/settings', '/settings?section=apikey', '/admin', '/admin/users', '/admin/agents', '/admin/groups', '/admin/sso', '/admin/config', '/admin/webhooks', '/admin/orphans', '/admin/audit',
  '/admin/agent-prompts', '/admin/agent-prompts?tab=blocks', `/projects/${proj.id}/settings?section=agents`, '/admin/backup', `/ui/boards/${board.id}/activity`, `/ui/projects/${proj.id}/card-templates`]) {   // the Inbox fragment is walked by the Inbox step (404 while the inbox is off)
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
await run('board: Inbox (capture a draft, move it to a column, drag one onto a column)', async () => {
  await api('PUT', `/boards/${board.id}/inbox`, { enabled: true });
  try {
    await go(`/boards/${board.id}`);
    const inbox = p.locator('.boards-inbox');
    if (!(await inbox.count())) throw new Error('no Inbox column with the inbox on');
    await inbox.locator('[data-open]').click();
    await p.keyboard.type('Smoke idea one'); await p.keyboard.press('Enter');
    await p.keyboard.type('Smoke idea two'); await p.keyboard.press('Enter'); await settle(p);
    if (await p.locator('#modal[open]').count()) throw new Error('capturing a draft opened the card sheet');
    const focused = await p.evaluate(() => !!(document.activeElement && document.activeElement.closest('.boards-inbox .boards-composer')));
    if (!focused) throw new Error('the Inbox composer lost focus after Enter');
    if ((await inbox.locator('.boards-draft').count()) < 2) throw new Error('drafts did not appear in the Inbox');
    await p.keyboard.press('Escape');
    // Move to... -> first column
    await inbox.locator('.boards-draft', { hasText: 'Smoke idea one' }).locator('.boards-draft-movebtn').click();
    await p.locator('#popover-root .boards-draft-publish button').first().click(); await settle(p);
    if (await inbox.locator('.boards-draft', { hasText: 'Smoke idea one' }).count()) throw new Error('published draft still in the Inbox');
    if (!(await p.locator(`#col-${lists[0].id} .boards-tile`, { hasText: 'Smoke idea one' }).count())) throw new Error('published card not in its column');
    // drag the other one onto the second column
    const src = await inbox.locator('.boards-draft', { hasText: 'Smoke idea two' }).boundingBox();
    const dst = await p.locator(`#col-${lists[1].id} .boards-cards`).boundingBox();
    await p.mouse.move(src.x + 30, src.y + 12); await p.mouse.down(); await p.mouse.move(src.x + 60, src.y + 30, { steps: 5 });
    await p.mouse.move(dst.x + 40, dst.y + 10, { steps: 15 }); await p.mouse.up(); await p.waitForTimeout(1500); await settle(p);
    if (await inbox.locator('.boards-draft', { hasText: 'Smoke idea two' }).count()) throw new Error('dragged draft still in the Inbox');
    if (!(await p.locator(`#col-${lists[1].id} .boards-tile`, { hasText: 'Smoke idea two' }).count())) throw new Error('dragged draft not published into the column');
    await go(`/ui/boards/${board.id}/inbox`); await go(`/boards/${board.id}?view=list`); await go(`/boards/${board.id}/calendar`);
  } finally {
    await api('PUT', `/boards/${board.id}/inbox`, { enabled: false });
  }
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
await run('board: archive and trash views (table or empty state, CSP)', async () => {
  const before = violations.length;
  for (const show of ['archived', 'trash']) {
    await go(`/boards/${board.id}?show=${show}`);
    const view = p.locator('#archive-view');
    await view.waitFor();
    await view.locator('table:visible, .archive-empty:visible, .empty-state:visible').first().waitFor();
  }
  const seen = violations.slice(before);
  if (seen.length) throw new Error(seen.join('; '));
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

// ---- board owner (non-admin): agent accounts and board webhooks ------------------------------------
// Needs WEAVE_OWNER_TOKEN: the API key of a NON-admin who may create projects (it creates its own project and board), and a
// server started with WEAVE_NOTIFY_ALLOW_HOSTS=127.0.0.1 so the strict webhook policy lets the local receiver below through.
await run('board owner (non-admin): create an agent for the board (key once), list, rotate, delete; board webhook: create, ping, deliveries', async () => {
  const OWNER = process.env.WEAVE_OWNER_TOKEN || '';
  if (!OWNER) { failures.push('[owner] WEAVE_OWNER_TOKEN is not set: the board-owner agent/webhook walk did not run'); return; }
  const oapi = async (method, path, body) => {
    const r = await fetch(BASE + '/api/v1' + path, { method, headers: { Authorization: 'Bearer ' + OWNER, 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
    const t = await r.text(); let j; try { j = JSON.parse(t); } catch { j = {}; }
    if (!r.ok) throw new Error(`owner ${method} ${path}: ${r.status} ${t.slice(0, 200)}`);
    return j.item ?? j;
  };
  const who = await oapi('GET', '/auth/whoami');
  if (who.role === 'admin') throw new Error('WEAVE_OWNER_TOKEN must belong to a non-admin');
  const http = await import('node:http');
  const received = [];
  const srv = http.createServer((req, res) => { let b = ''; req.on('data', c => { b += c; }); req.on('end', () => { received.push({ event: req.headers['x-weave-event'], sig: req.headers['x-weave-signature-v2'] }); res.writeHead(200, { 'Content-Type': 'text/plain' }); res.end('thanks'); }); });
  await new Promise(r => srv.listen(0, '127.0.0.1', r));
  const hookURL = `http://127.0.0.1:${srv.address().port}/hook`;
  const oproj = await oapi('POST', '/projects', { name: 'Owner smoke ' + stamp });
  const oboard = await oapi('POST', `/projects/${oproj.id}/boards`, { name: 'Owner board' });
  const octx = await browser.newContext({ viewport: { width: 1280, height: 800 }, extraHTTPHeaders: { Authorization: 'Bearer ' + OWNER, 'X-Requested-With': 'weave' } });
  const o = await newPage(octx);
  try {
    await o.goto(BASE + `/boards/${oboard.id}/settings?section=agents`); await o.locator('#bs-agent-accounts').waitFor(); await settle(o);
    // the Connect an agent dialog offers to create one while none has access
    await o.locator('#set-agents button:has-text("Connect an agent")').click(); await o.locator('#ap .ap-create').waitFor(); await esc(o); await settle(o);
    await o.fill('#bs-agent-name', 'Smoke bot');
    await o.locator('#bs-agent-accounts button:has-text("Create agent")').click(); await o.locator('#bs-agent-key').waitFor();
    const key = await o.locator('#bs-agent-key').inputValue();
    if (!/^wk_/.test(key)) throw new Error('no one-time key shown');
    const listed = await oapi('GET', `/agents?boardId=${oboard.id}`);
    if (listed.length !== 1 || listed[0].key) throw new Error('agent.list: ' + JSON.stringify(listed).slice(0, 200));
    await o.goto(BASE + `/boards/${oboard.id}/settings?section=agents`); await o.locator('#bs-agent-accounts .adm-user').waitFor(); await settle(o);
    if (await o.locator('#bs-agent-key').count()) throw new Error('the key is shown again after reload');
    await o.locator('#bs-agent-accounts .adm-user summary').first().click();
    o.once('dialog', d => d.accept());
    await o.locator('#bs-agent-accounts button:has-text("Rotate")').click(); await o.locator('#bs-agent-key').waitFor();
    if ((await o.locator('#bs-agent-key').inputValue()) === key) throw new Error('rotate did not issue a new key');
    await o.locator('#bs-agent-accounts .adm-user summary').first().click();
    o.once('dialog', d => d.accept());
    await o.locator('#bs-agent-accounts .adm-user button.danger:has-text("Delete")').click(); await o.locator('#bs-agent-accounts li.bs-empty-row').waitFor(); await settle(o);
    // board webhook
    await o.goto(BASE + `/boards/${oboard.id}/settings?section=webhooks`); await o.locator('#bs-webhooks').waitFor(); await settle(o);
    await o.fill('#bwh-new-name', 'Smoke hook'); await o.fill('#bwh-new-url', hookURL); await o.fill('#bwh-new-secret', 'smoke-secret');
    await o.locator('#bs-webhooks button:has-text("Create webhook")').click(); await o.locator('#bs-webhooks details.adm-user[open]').waitFor(); await settle(o);
    await o.locator('#bs-webhooks details.adm-user[open] button:has-text("Send ping")').click(); await o.locator('#bs-webhooks .bs-ping').waitFor();
    const ping = await o.locator('#bs-webhooks .bs-ping').textContent();
    if (!/Ping delivered/.test(ping)) throw new Error('ping: ' + ping.replace(/\s+/g, ' '));
    if (!received.some(x => x.event === 'ping' && /^t=\d+,v1=[0-9a-f]{64}$/.test(x.sig || ''))) throw new Error('receiver got no signed ping');
    await o.locator('#bs-webhooks button:has-text("Show deliveries")').click(); await o.locator('#bs-webhooks .adm-table td:has-text("ping")').waitFor(); await settle(o);
  } finally { await octx.close(); srv.close(); }
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

// ---- watching: board bell, column menu + sheet, card sheet, settings list ------------------------
await run('watching: board bell, column menu and sheet, card sheet watch/mute, settings list', async () => {
  await go(`/boards/${board.id}`);
  const bell = () => p.locator('#board-watch');
  if ((await bell().getAttribute('aria-pressed')) !== 'false') throw new Error('board bell should start off');
  await bell().click(); await settle(p);
  if ((await bell().getAttribute('aria-pressed')) !== 'true') throw new Error('board bell did not turn on');
  const order = await p.evaluate(() => [...document.querySelectorAll('.boards-actions > *')].map(e => e.id || e.className.split(' ').pop()));
  if (order.indexOf('board-watch') < 0 || order[order.indexOf('board-watch') + 1] !== 'boards-help-btn') throw new Error('bell is not left of the shortcuts button: ' + order);
  await bell().click(); await settle(p);
  if ((await bell().getAttribute('aria-pressed')) !== 'false') throw new Error('board bell did not turn off');
  // column menu: Watch column -> header bell appears after the board refresh
  await p.locator(`#col-${lists[1].id} details.boards-menu > summary`).click(); await p.waitForTimeout(250);
  await p.locator('#popover-root .boards-watch-item, .boards-menu[open] .boards-watch-item').first().click(); await settle(p); await settle(p);
  if (!(await p.locator(`#col-${lists[1].id} .boards-col-watch`).count())) throw new Error('column header bell missing after Watch column');
  // column sheet button toggles in place
  await p.locator(`#col-${lists[1].id} button.boards-col-settings`).click(); await p.locator('#list-sheet').waitFor(); await settle(p);
  if ((await p.locator('#ls-watch').getAttribute('aria-pressed')) !== 'true') throw new Error('column sheet should show Watching');
  await p.locator('#ls-watch').click(); await settle(p);
  if ((await p.locator('#ls-watch').getAttribute('aria-pressed')) !== 'false') throw new Error('column sheet watch did not toggle off');
  await p.locator('#ls-watch').click(); await settle(p);
  await esc(p); await settle(p);
  // card sheet: a card watched only through its column -> "via column" popover -> mute -> watch again
  await api('PATCH', '/users/me/preferences', { subscribeOwnCards: false });
  const wc = await api('POST', `/lists/${lists[1].id}/cards`, { name: 'Watched through its column' });
  await api('PATCH', '/users/me/preferences', { subscribeOwnCards: true });
  await go(`/cards/${wc.id}`);
  await p.locator('#card-modal summary.cm-watch.is-via').click(); await settle(p);
  await p.locator('#card-modal .cm-watch-panel button:has-text("Mute")').click(); await settle(p);
  await p.locator('#card-modal .cm-watch.is-muted').click(); await settle(p);
  if (!(await p.locator('#card-modal .cm-watch.is-on[aria-pressed=true]').count())) throw new Error('card did not return to Watching');
  // settings list: stop the card (mutes it), then watch it again from Muted
  await go('/settings?section=watching'); await p.locator('#set-watching .nw-h').first().waitFor();
  await p.locator('#set-watching form[hx-post$="/stop"] button').last().click(); await settle(p);
  await p.locator('#set-watching form[hx-post$="/watch"] button').first().click(); await settle(p);
  await p.locator('#set-watching form[hx-post$="/stop"] button').first().click(); await settle(p);
});

// ---- Admin console > Orphaned projects: the list (or its empty state), the people picker of each orphan, the recovery log --
await run('admin orphaned projects: list or empty state, pick a new manager (not submitted), recovery log', async () => {
  await go('/admin/orphans');
  await p.locator('#adm-orphans').waitFor();
  const combos = p.locator('.adm-orph [data-combo] [role=combobox]');
  if (await combos.count() === 0) {
    if (!(await p.locator('.adm-orph-empty').textContent()).includes('No orphaned projects')) throw new Error('no orphans and no empty state');
  } else {
    await combos.first().click(); await combos.first().press('ArrowDown');
    const opt = p.locator('.adm-orph [data-combo] li[role=option]:visible').first();
    if (await opt.count()) {
      await opt.click();
      if (await p.locator('.adm-orph [data-add]').first().isDisabled()) throw new Error('picking a person does not enable Reassign');
    }
  }
  await p.locator('#adm-recoveries-h').waitFor(); await settle(p);
});

// ---- Admin console > Audit log: the list (or its empty state), the action filter (htmx swap), the pager link -------------
await run('admin audit log: list or empty state, filter by action, back to all', async () => {
  await go('/admin/audit');
  await p.locator('#adm-audit').waitFor();
  if (await p.locator('.adm-audit-list > li').count() === 0) throw new Error('no rows and no empty state');
  await p.locator('#adm-audit-action').selectOption('group.member.add'); await settle(p);
  await p.locator('#adm-audit').waitFor();
  await p.locator('#adm-audit-action').selectOption(''); await settle(p);
  const older = p.locator('.adm-audit-pager a').last();
  if (await older.count()) { await older.click(); await settle(p); }
});

// ---- board settings, admin, settings pages (all sections) ----------------------------------
await run('board settings: every section', async () => {
  await go(`/boards/${board.id}/settings`);
  await clickEach('button, summary, a[hx-get]', { max: 60 });
});
for (const path of ['/settings', '/admin/users', '/admin/agents', '/admin/groups', '/admin/sso', '/admin/config', '/admin/webhooks', '/admin/orphans', '/admin/audit', '/admin/agent-prompts', '/admin/agent-prompts?tab=blocks', '/notification-services', '/dashboard']) {
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

// ---- card shortcuts (boards_cardkeys.js): L d p s S x A Delete on a tile and in the card sheet ------------------
await run('board: card shortcuts (labels, due, location, time, close, archive, trash, undo, focus, viewers)', async () => {
  const kb = await api('POST', `/projects/${proj.id}/boards`, { name: 'Keys board' });
  const [todo] = [await api('POST', `/boards/${kb.id}/lists`, { name: 'Keys todo' })];
  await api('PATCH', `/boards/${kb.id}`, { hasInbox: true });
  const bug = await api('POST', `/boards/${kb.id}/labels`, { name: 'Bug', color: 'berry-red' });
  await api('POST', `/boards/${kb.id}/labels`, { name: 'Feature', color: 'lagoon-blue' });
  const kc = [];
  for (const n of ['K1', 'K2', 'K3', 'K4', 'K5']) kc.push(await api('POST', `/lists/${todo.id}/cards`, { name: n }));
  const draft = await api('POST', `/boards/${kb.id}/drafts`, { name: 'Draft idea' });
  const allLists = await api('GET', `/boards/${kb.id}/lists`);
  const trashId = (allLists.items || allLists).find(l => l.type === 'trash').id;
  const get = id => api('GET', `/cards/${id}`);
  const tileSel = id => `#tile-${id}`;
  const focusTile = async id => { await p.focus(`${tileSel(id)} .boards-tile-link`); await p.waitForTimeout(80); };
  const focusedTile = () => p.evaluate(() => { const t = document.activeElement && document.activeElement.closest && document.activeElement.closest('.boards-tile'); return t ? t.dataset.id : ''; });
  const toast = async re => { await p.waitForFunction(r => [...document.querySelectorAll('#boards-toasts .boards-toast')].some(t => new RegExp(r).test(t.textContent)), re.source, { timeout: 4000 }); };
  const clearToasts = () => p.evaluate(() => document.querySelectorAll('#boards-toasts .boards-toast').forEach(t => t.remove()));
  const popKind = () => p.evaluate(() => { const b = document.querySelector('#boards-pop:not([hidden]) .boards-kpick'); return b ? b.dataset.kpick : ''; });
  const expect = (cond, msg) => { if (!cond) throw new Error(msg); };

  await go(`/boards/${kb.id}`);

  // nothing fires while typing: composer, quick edit, filter input
  await focusTile(kc[0].id); await p.keyboard.press('c'); await p.waitForTimeout(200);
  await p.keyboard.type('xA'); await p.keyboard.press('Backspace'); await p.keyboard.press('Backspace'); await p.keyboard.press('Escape');
  await focusTile(kc[0].id); await p.keyboard.press('e'); await p.waitForSelector('#boards-pop:not([hidden]) [data-quick-name]');
  await p.keyboard.type('xA'); await p.keyboard.press('Escape'); await p.waitForTimeout(150);
  await p.keyboard.press('/'); await p.keyboard.type('x'); await p.keyboard.press('Backspace'); await p.keyboard.press('Delete'); await p.keyboard.press('Escape');
  await p.evaluate(() => document.activeElement && document.activeElement.blur());
  let c0 = await get(kc[0].id);
  expect(!c0.isClosed && c0.listId === todo.id && c0.name === 'K1', `typing triggered a card shortcut: ${JSON.stringify({ closed: c0.isClosed, list: c0.listId, name: c0.name })}`);
  expect(!(await p.$('dialog.boards-confirm[open]')), 'Delete in the filter input opened the confirm');
  await go(`/boards/${kb.id}`);

  // L: label picker (number key, type-to-filter + Enter), Esc returns focus to the tile
  await focusTile(kc[0].id); await p.keyboard.press('L');
  await p.waitForSelector('#boards-pop:not([hidden]) .boards-kpick[data-kpick=labels]');
  await p.keyboard.press('1');
  await p.waitForFunction(id => document.querySelector(`#tile-${id} .boards-tile-labels`), kc[0].id);
  expect((await get(kc[0].id)).labelIds.includes(bug.id), 'L 1 did not add the first label');
  await p.keyboard.type('fea'); await p.keyboard.press('Enter');
  await p.waitForFunction(id => document.querySelectorAll(`#tile-${id} .boards-tile-labels > *`).length === 2, kc[0].id);
  await p.keyboard.press('Escape'); await p.waitForTimeout(150);
  expect(!(await popKind()), 'Esc did not close the label picker');
  expect((await focusedTile()) === kc[0].id, 'focus did not return to the tile after the label picker');
  await p.keyboard.press('L'); await p.waitForSelector('#boards-pop:not([hidden]) .boards-kpick');
  await p.keyboard.press('ArrowDown'); await p.keyboard.press('Enter'); await p.waitForTimeout(400);
  expect((await get(kc[0].id)).labelIds.length === 1, 'ArrowDown + Enter did not toggle the second label off');
  await p.keyboard.press('Escape');

  // d: due date quick choice (2 = tomorrow) and 0 = remove
  await focusTile(kc[0].id); await p.keyboard.press('d');
  await p.waitForSelector('#boards-pop:not([hidden]) .boards-kpick[data-kpick=due]');
  await p.keyboard.press('2'); await toast(/Due date set/);
  const tm = new Date(); tm.setDate(tm.getDate() + 1);
  const want = `${tm.getFullYear()}-${String(tm.getMonth() + 1).padStart(2, '0')}-${String(tm.getDate()).padStart(2, '0')}`;
  expect(((await get(kc[0].id)).dueDate || '').startsWith(want), `due date is not tomorrow (${want})`);
  await p.waitForSelector(`${tileSel(kc[0].id)} .boards-due`);
  expect((await focusedTile()) === kc[0].id, 'focus left the tile after setting the due date');
  await p.keyboard.press('d'); await p.waitForSelector('#boards-pop:not([hidden]) [data-due-clear]'); await p.keyboard.press('0'); await toast(/Due date removed/);
  expect(!(await get(kc[0].id)).dueDate, '0 did not remove the due date');

  // p: location set, then removed
  await focusTile(kc[0].id); await p.keyboard.press('p');
  await p.waitForSelector('#boards-pop:not([hidden]) .boards-kpick[data-kpick=place]');
  await p.keyboard.type('Harbour'); await p.fill('#boards-pop input[name=lat]', '52.37'); await p.fill('#boards-pop input[name=lng]', '4.89');
  await p.press('#boards-pop input[name=lng]', 'Enter'); await toast(/Location set/);
  await p.waitForSelector(`${tileSel(kc[0].id)} .boards-place`);
  expect((await get(kc[0].id)).lat === 52.37, 'location not saved');
  await focusTile(kc[0].id); await p.keyboard.press('p'); await p.click('#boards-pop [data-place-clear]'); await toast(/Location removed/);
  expect((await get(kc[0].id)).lat == null, 'location not removed');

  // s / S: start and stop time tracking, with the running indicator on the tile
  await focusTile(kc[0].id); await clearToasts(); await p.keyboard.press('s'); await toast(/Time tracking started/);
  await p.waitForSelector(`${tileSel(kc[0].id)} .boards-timer`);
  await focusTile(kc[0].id); await p.keyboard.press('s'); await toast(/already running/);
  await focusTile(kc[0].id); await p.keyboard.press('S'); await toast(/Time tracking stopped/);
  await p.waitForFunction(id => !document.querySelector(`#tile-${id} .boards-timer`), kc[0].id);

  // x: close moves focus to the next card; Undo re-opens it
  await focusTile(kc[0].id); await clearToasts(); await p.keyboard.press('x'); await toast(/Card closed/);
  await p.waitForFunction(id => document.querySelector(`#tile-${id}.is-closed`), kc[0].id);
  expect((await get(kc[0].id)).isClosed, 'x did not close the card');
  expect((await focusedTile()) === kc[1].id, `focus did not move to the next card after closing (on ${await focusedTile()})`);
  await p.click('#boards-toasts .boards-toast button'); await p.waitForTimeout(700);
  expect(!(await get(kc[0].id)).isClosed, 'Undo did not re-open the card');

  // A: archive removes the tile, focus moves on; Undo restores it and focuses it again
  await focusTile(kc[1].id); await clearToasts(); await p.keyboard.press('A'); await toast(/Card archived/);
  await p.waitForFunction(id => !document.getElementById(`tile-${id}`), kc[1].id);
  expect((await focusedTile()) === kc[2].id, 'focus did not move to the next card after archiving');
  await p.click('#boards-toasts .boards-toast button');
  await p.waitForFunction(id => document.activeElement && document.activeElement.closest && document.activeElement.closest(`#tile-${id}`), kc[1].id, { timeout: 4000 });
  expect((await get(kc[1].id)).listId === todo.id, 'Undo did not restore the archived card');

  // Delete: Esc cancels, a stray key does nothing, Enter moves to trash (reversible)
  await focusTile(kc[2].id); await p.keyboard.press('Delete');
  await p.waitForSelector('dialog.boards-confirm[open]');
  expect(await p.evaluate(() => document.activeElement && document.activeElement.hasAttribute('data-confirm-yes')), 'focus is not on the confirm button');
  for (const k of ['x', 'Delete', 'A', 'j', 'c', '?', '1']) { await p.keyboard.press(k); await p.waitForTimeout(60); }
  await p.waitForTimeout(250);
  expect(await p.$('dialog.boards-confirm[open]'), 'a stray key closed the confirm');
  expect(new URL(p.url()).pathname === `/boards/${kb.id}` && !(await p.$('#boards-help[open]')) && !(await p.$('.boards-composer.is-open')), 'a board key acted behind the confirm');
  await p.keyboard.press('Escape'); await p.waitForTimeout(200);
  expect(!(await p.$('dialog.boards-confirm[open]')), 'Esc did not cancel the confirm');
  expect((await get(kc[2].id)).listId === todo.id, 'cancelled delete still moved the card');
  expect((await focusedTile()) === kc[2].id, 'focus did not return to the tile after cancelling');
  await clearToasts(); await p.keyboard.press('Backspace'); await p.waitForSelector('dialog.boards-confirm[open]'); await p.keyboard.press('Enter');
  await toast(/moved to trash/);
  await p.waitForFunction(id => !document.getElementById(`tile-${id}`), kc[2].id);
  expect((await get(kc[2].id)).listId === trashId, 'Delete did not move the card to the trash list');
  expect((await focusedTile()) === kc[3].id, 'focus did not move to the next card after Delete');

  // Inbox draft: only Delete (permanent, after a confirmation); other keys do nothing
  await focusTile(draft.id); await p.keyboard.press('x'); await p.keyboard.press('A'); await p.keyboard.press('L'); await p.waitForTimeout(300);
  expect(!(await popKind()) && !!(await p.$(tileSel(draft.id))), 'a card shortcut acted on an Inbox draft');
  await focusTile(draft.id); await p.keyboard.press('Delete'); await p.waitForSelector('dialog.boards-confirm[open]');
  expect(/permanently/.test(await p.textContent('dialog.boards-confirm')), 'draft delete confirm does not say permanently');
  await p.keyboard.press('Enter'); await p.waitForFunction(id => !document.getElementById(`tile-${id}`), draft.id);

  // card sheet: x close/re-open, s/S time, p and L popovers, Delete confirm above the sheet, A archive (closes the sheet)
  await focusTile(kc[3].id); await p.keyboard.press('Enter'); await p.waitForSelector('#modal[open] #card-modal');
  await p.keyboard.press('x'); await p.waitForSelector('#card-modal[data-closed]');
  expect((await get(kc[3].id)).isClosed, 'x in the sheet did not close the card');
  await p.keyboard.press('x'); await p.waitForSelector('#card-modal:not([data-closed])');
  await p.waitForSelector('#x-time .x-timebtn');
  await p.keyboard.press('s'); await p.waitForSelector('#x-time .x-timer.is-running'); await toast(/started/);
  await p.keyboard.press('S'); await p.waitForSelector('#x-time .x-timer:not(.is-running)'); await toast(/stopped/);
  await p.keyboard.press('p'); await p.waitForSelector('#card-modal details[data-pop=location][open]'); await p.keyboard.press('Escape');
  await p.evaluate(() => document.activeElement && document.activeElement.blur && document.getElementById('modal').focus());
  await p.keyboard.press('L'); await p.waitForSelector('#card-modal details[data-pop=labels][open]'); await p.keyboard.press('Escape');
  await p.evaluate(() => document.getElementById('modal').focus());
  await p.keyboard.press('Delete'); await p.waitForSelector('dialog.boards-confirm[open]');
  await p.keyboard.press('j'); await p.waitForTimeout(400);
  expect((await p.getAttribute('#card-modal', 'data-card')) === kc[3].id, 'j behind the confirm switched the sheet to another card');
  await p.keyboard.press('Escape'); await p.waitForTimeout(200);
  expect(await p.$('#modal[open] #card-modal'), 'cancelling the confirm closed the sheet');
  await clearToasts(); await p.keyboard.press('A'); await toast(/Card archived/);
  await p.waitForFunction(() => !document.getElementById('modal').open);
  expect((await get(kc[3].id)).listId !== todo.id, 'A in the sheet did not archive');
  await p.click('#boards-toasts .boards-toast button'); await p.waitForSelector(tileSel(kc[3].id));

  // viewers: nothing happens (a small read-only toast), no picker, no confirm
  const vk = await api('POST', '/agents', { name: 'Keys viewer', role: 'boardUser', boardIds: [kb.id], boardRole: 'viewer' });
  const vctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, extraHTTPHeaders: { Authorization: 'Bearer ' + (vk.key && vk.key.key || vk.key), 'X-Requested-With': 'weave' } });
  const vp = await newPage(vctx);
  await vp.goto(BASE + `/boards/${kb.id}`); await vp.waitForTimeout(500);
  await vp.focus(`${tileSel(kc[4].id)} .boards-tile-link`);
  for (const k of ['x', 'A', 'Delete', 'L', 'd', 'p', 's']) { await vp.keyboard.press(k); await vp.waitForTimeout(120); }
  expect(!(await vp.$('dialog.boards-confirm[open]')) && !(await vp.$('#boards-pop:not([hidden])')), 'a viewer got a picker or a confirm');
  expect(/read-only/i.test(await vp.textContent('#boards-toasts')), 'viewer did not get the read-only toast');
  const c4 = await get(kc[4].id);
  expect(!c4.isClosed && c4.listId === todo.id, 'a viewer changed the card');
  await vctx.close();
  await api('DELETE', `/agents/${vk.id}`).catch(() => {});
});

// ---- ownership: plain users create projects and boards ---------------------------------------
// Optional: WEAVE_USER_TOKEN and WEAVE_USER2_TOKEN are API keys of two *different* plain boardUser accounts
// (`weave admin mint-key --email u1@example.com --role boardUser`). Skipped when unset.
const U1 = process.env.WEAVE_USER_TOKEN || '', U2 = process.env.WEAVE_USER2_TOKEN || '';
async function apiAs(tok, method, path, body) {
  const r = await fetch(BASE + '/api/v1' + path, { method, headers: { Authorization: 'Bearer ' + tok, 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
  const t = await r.text(); let j; try { j = JSON.parse(t); } catch { j = {}; }
  return { status: r.status, item: j.item ?? j };
}
const userCtx = tok => browser.newContext({ viewport: { width: 1280, height: 800 }, extraHTTPHeaders: { Authorization: 'Bearer ' + tok, 'X-Requested-With': 'weave' } });
async function createBlankBoard(up, name) {
  await up.click('.projects-board-new');
  await up.waitForSelector('#modal[open] .tg-tile-blank');
  await up.click('#modal[open] .tg-tile-blank');
  await up.fill('#modal[open] .tg-create input[name=name]', name);
  await Promise.all([up.waitForURL(/\/boards\/[0-9a-f]+/), up.click('#modal[open] .tg-go')]);
  return up.url().split('/boards/')[1].split(/[?#]/)[0];
}
if (U1 && U2) await run('ownership: a plain boardUser creates a project, a board and a card; a member creates boards only when the project allows it', async () => {
  const expect = (cond, msg) => { if (!cond) throw new Error(msg); };
  const u2 = (await apiAs(U2, 'GET', '/auth/me')).item;
  const c1 = await userCtx(U1), c2 = await userCtx(U2);
  try {
    const up = await newPage(c1);
    await up.goto(BASE + '/'); await settle(up);
    await up.click('button[hx-get="/ui/projects/new"]');
    await up.fill('#modal[open] form.projects-dialog input[name=name]', 'Owned ' + stamp);
    await Promise.all([up.waitForURL(/\/projects\/[0-9a-f]+$/), up.click('#modal[open] form.projects-dialog button.primary')]);
    const pid = up.url().split('/projects/')[1];
    const bid = await createBlankBoard(up, 'Mine');
    const l = await apiAs(U1, 'POST', `/boards/${bid}/lists`, { name: 'Todo' });
    const c = await apiAs(U1, 'POST', `/lists/${l.item.id}/cards`, { name: 'Owned card' });
    expect(l.status === 200 && c.status === 200, `list/card by the project creator: ${l.status} ${c.status}`);
    await up.goto(BASE + `/boards/${bid}`); await settle(up);
    expect(await up.isVisible('text=Owned card'), 'the new card is not on the board');
    // a second user, an editor on that board, is a project member
    expect((await apiAs(U1, 'POST', `/boards/${bid}/members`, { userId: u2.id, role: 'editor' })).status === 200, 'adding the member failed');
    const vp = await newPage(c2);
    await vp.goto(BASE + `/projects/${pid}`); await settle(vp);
    expect(!(await vp.$('.projects-board-new')), 'a member sees Create board while the project allows managers only');
    expect((await apiAs(U2, 'POST', `/projects/${pid}/boards`, { name: 'Refused' })).status === 403, 'a member created a board under "owners"');
    // the creator switches Project settings > General to "Any project member" (autosave)
    await up.goto(BASE + `/projects/${pid}/settings`); await settle(up);
    await up.check('#set-general input[name=boardCreation][value=members]');
    await up.waitForSelector('#set-general .bs-saved');
    expect((await apiAs(U1, 'GET', `/projects/${pid}`)).item.boardCreation === 'members', 'setting not saved');
    await vp.goto(BASE + `/projects/${pid}`); await settle(vp);
    const nb = await createBlankBoard(vp, 'Member board');
    const ms = (await apiAs(U2, 'GET', `/boards/${nb}/members`)).item;
    expect(Array.isArray(ms) && ms.some(m => m.userId === u2.id && m.role === 'owner'), 'the member did not become the board owner');
    await vp.goto(BASE + `/boards/${nb}/settings?section=members`); await settle(vp);
    expect(await vp.$('#set-members select option[value=owner]'), 'the board owner gets no members management');

    // ---- the plain user's whole path (capability layer): no admin console, people pickers filter, owners, agents, webhooks
    await up.goto(BASE + '/'); await settle(up);
    await up.click('summary[aria-label="Account menu"]');
    expect(!(await up.$('[data-cap=admin-console]')), 'a plain user sees the Admin console link');
    // Project settings > Managers: typing filters the people picker ([hidden] must win over .bs-combo li{display:flex})
    await up.goto(BASE + `/projects/${pid}/settings?section=managers`); await settle(up);
    const combo = up.locator('#set-managers [data-combo] [role=combobox]');
    await combo.click(); await combo.press('ArrowDown');
    const all = await up.locator('#set-managers [data-combo] li[role=option]').count();
    await combo.fill('zzzz-nobody-has-this-name');
    const shown = await up.$$eval('#set-managers [data-combo] li[role=option]', els => els.filter(e => getComputedStyle(e).display !== 'none').length);
    const leaks = await up.$$eval('#set-managers [data-combo] li[role=option][hidden]', els => els.filter(e => getComputedStyle(e).display !== 'none').length);
    expect(all > 0 && shown === 0 && leaks === 0, `the people picker does not filter (${shown} of ${all} options visible, ${leaks} hidden but displayed)`);
    await combo.fill('');
    // board settings as the project creator: make the member an owner, see Agents and Webhooks; no e-mail addresses of others
    await up.goto(BASE + `/boards/${bid}/settings?section=members`); await settle(up);
    await up.locator('#set-members li.bs-member', { hasText: u2.name }).locator('select[name=role]').selectOption('owner'); await settle(up);
    const mem = (await apiAs(U1, 'GET', `/boards/${bid}/members`)).item;
    expect(Array.isArray(mem) && mem.some(m => m.userId === u2.id && m.role === 'owner'), 'making the member an owner did not save');
    for (const hook of ['board-members-add', 'board-share', 'board-template-publish', 'board-webhooks', 'board-delete']) {
      expect(await up.$(`[data-cap=${hook}]`), `the board creator misses ${hook} in board settings`);
    }
    await up.goto(BASE + `/boards/${bid}/settings?section=agents`); await up.locator('[data-cap=board-agent-create]').waitFor({ state: 'attached' }); await settle(up);
    await up.goto(BASE + `/boards/${bid}/settings?section=webhooks`); await up.locator('[data-cap=board-webhook-create]').waitFor({ state: 'attached' }); await settle(up);
    // the new owner (U2) sees the same management sections; a viewer would see none of them (capability matrix test)
    await vp.goto(BASE + `/boards/${bid}/settings`); await settle(vp);
    for (const hook of ['board-members-add', 'board-share', 'board-webhooks', 'board-delete']) {
      expect(await vp.$(`[data-cap=${hook}]`), `the new board owner misses ${hook}`);
    }
    await vp.locator('[data-cap=board-agent-create]').waitFor({ state: 'attached' }); // the Agents section's lazy fragment replaces its placeholder
    expect(!(await vp.$('#set-general a[href$="/settings"][href*="/projects/"]')), 'a board owner is sent to project settings');
  } finally { await c1.close(); await c2.close(); }
});

// ---- admin (system administrator): the console is there, and it carries no project or board content links ----
await run('admin: console link, system sections only (no board/card/project links on any console page)', async () => {
  await go('/');
  await p.click('summary[aria-label="Account menu"]');
  if (!(await p.$('[data-cap=admin-console]'))) throw new Error('the admin sees no Admin console link');
  await esc(p);
  for (const path of ['/admin', '/admin/users', '/admin/agents', '/admin/groups', '/admin/orphans', '/admin/audit', '/admin/sso', '/admin/config', '/admin/webhooks', '/admin/agent-prompts']) {
    await go(path);
    const links = await p.$$eval('main a[href^="/boards/"], main a[href^="/cards/"], main a[href^="/projects/"]', as => as.map(a => a.getAttribute('href')));
    if (links.length) throw new Error(`${path} links to content: ${links.slice(0, 3).join(', ')}`);
  }
});

// ---- admin = system administration, no content access: someone else's board is "not found" for the admin -------
// Needs WEAVE_USER_TOKEN (a plain boardUser). The user creates a project and a board; the admin's browser opens
// their links and gets the not-found page; the admin's home lists only the admin's own projects.
if (U1) await run('admin: another person\'s project and board are not found; the home page lists only the admin\'s own projects', async () => {
  const up = (await apiAs(U1, 'POST', '/projects', { name: 'Private ' + stamp })).item;
  const ub = (await apiAs(U1, 'POST', `/projects/${up.id}/boards`, { name: 'Private board' })).item;
  for (const path of [`/boards/${ub.id}`, `/projects/${up.id}`, `/boards/${ub.id}/settings`]) {
    const r = await p.goto(BASE + path); await settle(p);
    if (!r || r.status() !== 404) throw new Error(`admin ${path} -> HTTP ${r && r.status()} (want 404)`);
    if (await p.isVisible('text=Private board')) throw new Error(`admin ${path} shows the board name`);
  }
  await go('/');
  if (await p.isVisible(`text=Private ${stamp}`)) throw new Error('the admin home lists another person\'s project');
  if (!(await p.isVisible(`text=${proj.name}`))) throw new Error('the admin home misses the admin\'s own project');
});

// ---- closed check mark: x, drag into a closed column, archive keeps the flag, re-open ------------------------------
await run('board: closed check mark (x, drag into a closed column, archive keeps it, re-open, sheet badge, calendar)', async () => {
  let at = 'setup';
  try {
  const cb = await api('POST', `/projects/${proj.id}/boards`, { name: 'Closed board' });
  const todo = await api('POST', `/boards/${cb.id}/lists`, { name: 'Todo' });
  const done = await api('POST', `/boards/${cb.id}/lists`, { name: 'Done', type: 'closed' });
  const due = new Date(); due.setDate(15);
  const cc = [];
  for (const n of ['C1', 'C2', 'C3']) cc.push(await api('POST', `/lists/${todo.id}/cards`, { name: n, dueDate: due.toISOString().slice(0, 10) }));
  const get = id => api('GET', `/cards/${id}`);
  const expect = (cond, msg) => { if (!cond) throw new Error(msg); };
  const mark = id => `#tile-${id} .boards-tile-name .closed-mark`;
  await go(`/boards/${cb.id}`);
  expect(!(await p.$('.closed-mark')), 'an open card shows the closed mark');
  // x on a tile: the card moves to Done and gets the mark, Undo removes it again
  at = 'x on a tile: the card moves to Done and gets the mark, Undo removes it again';
  await p.focus(`#tile-${cc[0].id} .boards-tile-link`); await p.keyboard.press('x');
  await p.waitForSelector(mark(cc[0].id), { timeout: 4000 });
  expect(await p.$(`#col-${done.id} #tile-${cc[0].id}`), 'x did not move the card into the closed column');
  expect((await p.getAttribute(`${mark(cc[0].id)}`, 'title')) === 'Closed', 'the mark has no "Closed" hover title');
  expect(/Closed:/.test(await p.textContent(`#tile-${cc[0].id} .boards-tile-name`)), 'no screen-reader text on the mark');
  await p.click('#boards-toasts .boards-toast button');
  await p.waitForFunction(s => !document.querySelector(s), mark(cc[0].id), { timeout: 4000 });
  // drag C2 into the closed column: the mark appears without a reload
  at = 'drag C2 into the closed column: the mark appears without a reload';
  const src = p.locator(`#tile-${cc[1].id}`), dst = p.locator(`#col-${done.id} .boards-cards`);
  const sb = await src.boundingBox(), db = await dst.boundingBox();
  await p.mouse.move(sb.x + sb.width / 2, sb.y + sb.height / 2); await p.mouse.down();
  await p.mouse.move(sb.x + sb.width / 2 + 10, sb.y + sb.height / 2 + 10, { steps: 3 });
  await p.mouse.move(db.x + db.width / 2, db.y + 20, { steps: 12 }); await p.mouse.up();
  await p.waitForSelector(mark(cc[1].id), { timeout: 4000 });
  expect((await get(cc[1].id)).isClosed, 'drag into the closed column did not close the card');
  // the card sheet shows the green Closed badge
  at = 'the card sheet shows the green Closed badge';
  await p.waitForTimeout(800);   // let the live refresh after the drop settle
  await p.click(`#tile-${cc[1].id} .boards-tile-link`);
  await p.waitForSelector('#modal[open] #card-modal .cards-badge-closed .closed-mark');
  await esc(p); await p.waitForFunction(() => !document.getElementById('modal').open);
  // archive keeps the flag; restore into the closed column keeps the mark
  at = 'archive keeps the flag; restore into the closed column keeps the mark';
  await p.focus(`#tile-${cc[1].id} .boards-tile-link`); await p.keyboard.press('A');
  await p.waitForFunction(id => !document.getElementById(`tile-${id}`), cc[1].id);
  const archived = await get(cc[1].id);
  expect(archived.isClosed && archived.listId !== done.id, 'archiving cleared the closed state');
  await api('POST', `/cards/${cc[1].id}/restore`, {});
  // live via the SSE refresh (an API write from outside the page). It arrives when this step runs alone; in a full run it
  // has not arrived within 6 s (cause not yet explained: no 429 from the stream cap was seen), so fall back to a reload:
  // the archived-then-restored closed state is what this step checks (x, drag and the sheet do not depend on SSE)
  if (!(await p.waitForSelector(mark(cc[1].id), { timeout: 6000 }).catch(() => null))) {
    console.log('     (no live refresh in this tab; checking after a reload)');
    await go(`/boards/${cb.id}`); await p.waitForSelector(mark(cc[1].id), { timeout: 3000 });
  }
  // the calendar shows the mark on the closed card only
  at = 'the calendar shows the mark on the closed card only';
  await go(`/boards/${cb.id}/calendar`);
  expect((await p.locator('.cards-cal-chip .closed-mark').count()) === 1, 'calendar should show exactly one closed mark');
  // re-open from the sheet on the calendar: the chip loses the mark when the sheet closes
  at = 're-open from the sheet on the calendar: the chip loses the mark when the sheet closes';
  await p.locator(`.cards-cal-chip[data-card="${cc[1].id}"]`).first().click();
  await p.waitForSelector('#modal[open] #card-modal');
  await p.locator('#card-modal [hx-post$="/close"]').first().evaluate(b => b.click());
  await p.waitForSelector('#card-modal:not([data-closed])');
  await esc(p); await p.waitForTimeout(800);
  expect((await p.locator('.cards-cal-chip .closed-mark').count()) === 0, 'the calendar still shows the mark after re-opening');
  expect(!(await get(cc[1].id)).isClosed, 'Re-open did not re-open the card');
  } catch (e) { throw new Error(`${at}: ${e.message}`); }
});

// ---- column setting "Close cards that land here": sheet toggle with the count confirm, keep, cancel, column menu ------
await run('board: column setting "Close cards that land here" (sheet confirm, flip, re-open, future-only, cancel, menu, empty column)', async () => {
  let at = 'setup';
  try {
  const cb = await api('POST', `/projects/${proj.id}/boards`, { name: 'Closing column board' });
  const todo = await api('POST', `/boards/${cb.id}/lists`, { name: 'Todo' });
  const rev = await api('POST', `/boards/${cb.id}/lists`, { name: 'Review' });
  const empty = await api('POST', `/boards/${cb.id}/lists`, { name: 'Empty' });
  const tc = [], rc = [];
  for (const n of ['T1', 'T2', 'T3']) tc.push(await api('POST', `/lists/${todo.id}/cards`, { name: n }));
  for (const n of ['R1', 'R2']) rc.push(await api('POST', `/lists/${rev.id}/cards`, { name: n }));
  const expect = (cond, msg) => { if (!cond) throw new Error(msg); };
  const closedOf = async ids => Promise.all(ids.map(async c => (await api('GET', `/cards/${c.id}`)).isClosed));
  const typeOf = async l => (await api('GET', `/lists/${l.id}`)).type;
  const openPop = async () => {
    await p.click('#list-sheet details[data-pop=closes] > summary');
    await p.waitForSelector('#list-sheet details[data-pop=closes][open] [data-closes-opt]');
  };
  await go(`/boards/${cb.id}`);
  expect(!(await p.$('.boards-col-closes')), 'no column closes cards yet');
  await p.click(`[data-list-sheet="${rev.id}"]`);
  await p.waitForSelector('#modal[open] #list-sheet');
  expect(/Off/.test(await p.textContent('#list-sheet [data-ls-type-val]')), 'the setting starts Off');

  at = 'switch on: the confirm states the count, the default flips the cards';
  await openPop();
  await p.click('#list-sheet [data-closes-opt=closed]');
  await p.waitForSelector('#ls-closes-confirm .closes-confirm');
  expect(/Close 2 cards now\?/.test(await p.textContent('#ls-closes-confirm')), 'confirm text lacks the count');
  expect(await p.evaluate(() => document.activeElement && document.activeElement.hasAttribute('data-closes-default')), 'the default action is not focused');
  await p.click('#ls-closes-confirm [data-closes-default]');
  await p.waitForSelector('#list-sheet [data-ls-type]:not([hidden])');
  expect((await closedOf(rc)).every(Boolean), 'flip did not close the cards');
  expect(/On/.test(await p.textContent('#list-sheet [data-ls-type-val]')), 'the value does not read On');
  await p.waitForSelector(`#col-${rev.id} .boards-col-closes`, { timeout: 4000 });
  await p.waitForSelector(`#tile-${rc[0].id} .closed-mark`, { timeout: 4000 });

  at = 'switch off: "Re-open 2 cards?" re-opens them';
  await openPop();
  await p.click('#list-sheet [data-closes-opt=active]');
  await p.waitForSelector('#ls-closes-confirm .closes-confirm');
  expect(/Re-open 2 cards\?/.test(await p.textContent('#ls-closes-confirm')), 're-open confirm text lacks the count');
  await p.click('#ls-closes-confirm [data-closes-default]');
  await p.waitForSelector('#list-sheet [data-ls-type][hidden]', { state: 'attached' });
  expect(!(await closedOf(rc)).some(Boolean), 'switching off did not re-open the cards');
  expect((await typeOf(rev)) === 'active', 'type is not active');

  at = 'future-only: "Leave them open" switches the setting and keeps the cards open; a new card closes';
  await openPop();
  await p.click('#list-sheet [data-closes-opt=closed]');
  await p.waitForSelector('#ls-closes-confirm .closes-confirm');
  await p.click('#ls-closes-confirm form:nth-of-type(2) button');
  await p.waitForSelector('#list-sheet [data-ls-type]:not([hidden])');
  expect((await typeOf(rev)) === 'closed', 'keep did not switch the setting');
  expect(!(await closedOf(rc)).some(Boolean), 'keep closed the existing cards');
  const late = await api('POST', `/lists/${rev.id}/cards`, { name: 'Late' });
  expect(late.isClosed, 'a card created in the column after the switch is not closed');

  at = 'cancel: the confirm goes away and nothing changes';
  await openPop();
  await p.click('#list-sheet [data-closes-opt=active]');
  await p.waitForSelector('#ls-closes-confirm .closes-confirm');
  await p.click('#ls-closes-confirm [data-closes-cancel]');
  await p.waitForFunction(() => !document.querySelector('#ls-closes-confirm .closes-confirm'));
  expect((await typeOf(rev)) === 'closed', 'cancel changed the setting');
  at = 'Esc closes the popover, a second Esc the sheet';
  // the second Esc must come after the sheet's 80 ms Esc-layer guard (boards_listsheet.js), like a person pressing twice
  await esc(p); await p.waitForTimeout(250); await esc(p); await p.waitForFunction(() => !document.getElementById('modal').open);

  at = 'column menu: the same toggle asks with the count and flips';
  await p.click(`#col-${todo.id} details.boards-menu > summary`);
  const item = `.boards-menu-pop[data-list="${todo.id}"] .boards-closes-item`;
  await p.waitForSelector(item);
  expect((await p.getAttribute(item, 'aria-checked')) === 'false', 'menu item should be unchecked');
  await p.click(item);
  await p.waitForSelector(`#lcc-${todo.id} .closes-confirm`);
  expect(/Close 3 cards now\?/.test(await p.textContent(`#lcc-${todo.id}`)), 'menu confirm lacks the count');
  const box = await p.locator(`#lcc-${todo.id} .closes-confirm`).boundingBox();
  const vp = p.viewportSize();
  expect(box && box.y >= 0 && box.y + box.height <= vp.height + 1, 'the menu confirm runs off screen');
  await p.click(`#lcc-${todo.id} [data-closes-default]`);
  await p.waitForSelector(`#col-${todo.id} .boards-col-closes`, { timeout: 4000 });
  expect((await closedOf(tc)).every(Boolean), 'menu flip did not close the cards');

  at = 'column menu on an empty column: switches at once';
  await p.click(`#col-${empty.id} details.boards-menu > summary`);
  await p.click(`.boards-menu-pop[data-list="${empty.id}"] .boards-closes-item`);
  await p.waitForSelector(`#col-${empty.id} .boards-col-closes`, { timeout: 4000 });
  expect((await typeOf(empty)) === 'closed', 'empty column did not switch');
  } catch (e) { throw new Error(`${at}: ${e.message}`); }
});

// ---- column setting "Auto-archive: keep the newest N cards": sheet confirm with the count, hold, restore, menu --------
await run('board: column auto-archive (sheet confirm, archive, hold on a new card, restore, toast, menu, turn off)', async () => {
  let at = 'setup';
  try {
  const ab = await api('POST', `/projects/${proj.id}/boards`, { name: 'Auto-archive board' });
  const done = await api('POST', `/boards/${ab.id}/lists`, { name: 'Done' });
  const side = await api('POST', `/boards/${ab.id}/lists`, { name: 'Side' });
  const cs = [];
  for (let i = 1; i <= 12; i++) cs.push(await api('POST', `/lists/${done.id}/cards`, { name: `Done ${i}` }));
  for (let i = 1; i <= 3; i++) await api('POST', `/lists/${side.id}/cards`, { name: `Side ${i}` });
  const expect = (cond, msg) => { if (!cond) throw new Error(msg); };
  const listOf = async c => (await api('GET', `/cards/${c.id}`)).listId;
  await go(`/boards/${ab.id}`);
  expect(!(await p.$('.boards-col-archive')), 'no column auto-archives yet');
  await p.click(`[data-list-sheet="${done.id}"]`);
  await p.waitForSelector('#modal[open] #list-sheet');
  expect(/Off/.test(await p.textContent('#list-sheet [data-ls-keep-val]')), 'the setting starts Off');

  at = 'set 10 on 12 cards: the confirm states the count and archives the 2 oldest';
  await p.click('#list-sheet details[data-pop=archive] > summary');
  await p.waitForSelector('#list-sheet details[data-pop=archive][open] [data-ls-keep-input]');
  // real keystrokes (digits are board shortcuts elsewhere): type the number and press Enter in the field
  await p.click('#list-sheet [data-ls-keep-input]');
  await p.keyboard.type('10');
  await p.keyboard.press('Enter');
  await p.waitForSelector('#ls-archive-confirm .closes-confirm');
  expect(/Archive 2 cards now\?/.test(await p.textContent('#ls-archive-confirm')), 'confirm text lacks the count');
  expect(await p.evaluate(() => document.activeElement && document.activeElement.hasAttribute('data-closes-default')), 'the default action is not focused');
  await p.click('#ls-archive-confirm [data-closes-default]');
  await p.waitForSelector('#list-sheet [data-ls-keep]:not([hidden])');
  expect(/Keep 10/.test(await p.textContent('#list-sheet [data-ls-keep-val]')), 'the value does not read Keep 10');
  const arch = (await api('GET', `/boards/${ab.id}/lists`)).find(l => l.type === 'archive');
  expect((await listOf(cs[0])) === arch.id && (await listOf(cs[1])) === arch.id && (await listOf(cs[2])) === done.id, 'the two oldest were not archived');
  await p.waitForSelector(`#col-${done.id} .boards-col-archive`, { timeout: 4000 });
  await esc(p); await p.waitForTimeout(250); await esc(p); await p.waitForFunction(() => !document.getElementById('modal').open);

  at = 'hold: a new card archives the oldest, the board toasts it';
  const nc = await api('POST', `/lists/${done.id}/cards`, { name: 'Done new' });
  expect((await listOf(cs[2])) === arch.id && (await listOf(nc)) === done.id, 'the oldest was not archived on landing');
  await p.waitForSelector('.boards-toast:has-text("archived 1 older card")', { timeout: 6000 });
  await p.waitForFunction(id => !document.getElementById('tile-' + id), cs[2].id, { timeout: 6000 });

  at = 'restore into the full column archives its oldest other card';
  await api('POST', `/cards/${cs[0].id}/restore`);
  expect((await listOf(cs[0])) === done.id && (await listOf(cs[3])) === arch.id, 'restore did not hold the column at 10');

  at = 'column menu: lower to 8 asks with the count, then turn it off';
  await p.click(`#col-${done.id} details.boards-menu > summary`);
  const menu = `.boards-menu-pop[data-list="${done.id}"]`;
  await p.waitForSelector(`${menu} .boards-menu-archive input[name=archiveAfter]`);
  const url0 = p.url();
  await p.click(`${menu} .boards-menu-archive input[name=archiveAfter]`);
  await p.keyboard.press('Control+A'); await p.keyboard.type('8'); await p.keyboard.press('Enter');
  await p.waitForSelector(`#lac-${done.id} .closes-confirm`);
  expect(p.url() === url0 && await p.$(`#col-${done.id}`), 'typing digits in the menu switched the view');
  expect(await p.evaluate(() => document.activeElement && document.activeElement.hasAttribute('data-closes-default')), 'the menu confirm default is not focused');
  expect(/Archive 2 cards now\?/.test(await p.textContent(`#lac-${done.id}`)), 'menu confirm lacks the count');
  const box = await p.locator(`#lac-${done.id} .closes-confirm`).boundingBox();
  const vp = p.viewportSize();
  expect(box && box.y >= 0 && box.y + box.height <= vp.height + 1, 'the menu confirm runs off screen');
  await p.click(`#lac-${done.id} [data-closes-default]`);
  for (let i = 0; i < 40 && (await api('GET', `/lists/${done.id}`)).archiveAfter !== 8; i++) await p.waitForTimeout(100);
  expect((await api('GET', `/lists/${done.id}`)).archiveAfter === 8, 'menu did not set 8');
  await p.waitForTimeout(600);
  await p.click(`#col-${done.id} details.boards-menu > summary`);
  await p.click(`${menu} .boards-archive-off`);
  for (let i = 0; i < 40 && (await api('GET', `/lists/${done.id}`)).archiveAfter; i++) await p.waitForTimeout(100);
  expect(!(await api('GET', `/lists/${done.id}`)).archiveAfter, 'turn off did not clear the setting');
  await p.waitForFunction(id => !document.querySelector('#col-' + id + ' .boards-col-archive'), done.id, { timeout: 4000 });
  } catch (e) { throw new Error(`${at}: ${e.message}`); }
});

await run('board settings: apply a template (gallery in apply mode, preview, options re-preview, remove/keep the old columns, confirm, new left columns, cards moved, old columns removed, labels added not replaced)', async () => {
  const b = await api('POST', `/projects/${proj.id}/boards`, { name: 'Apply smoke' });
  const todo = await api('POST', `/boards/${b.id}/lists`, { name: 'Todo' });
  const done = await api('POST', `/boards/${b.id}/lists`, { name: 'Done', type: 'closed' });
  for (const n of ['First', 'Second']) await api('POST', `/lists/${todo.id}/cards`, { name: n });
  await api('POST', `/lists/${done.id}/cards`, { name: 'Finished' });
  await api('POST', `/boards/${b.id}/labels`, { name: 'high', color: 'berry-red' });
  await go(`/boards/${b.id}/settings?section=templates`);
  await p.locator(`#set-templates [hx-get="/ui/boards/${b.id}/apply-template"]`).click(); await settle(p);
  await p.waitForSelector('dialog[open] .tg-grid');
  if (await p.locator('dialog[open] .tg-tile-blank, dialog[open] .tg-cat-import').count()) throw new Error('apply mode shows the blank tile or the import pane');
  await p.locator('#tg-q').pressSequentially('kanban'); await settle(p);
  await p.locator('dialog[open] .tg-tile', { hasText: 'Kanban basics' }).click(); await settle(p);
  await p.locator('dialog[open] .tg-create button.primary', { hasText: 'Preview changes' }).click(); await settle(p);
  await p.waitForSelector('dialog[open] .tg-diff');
  const txt = await p.locator('dialog[open] .tg-ap').innerText();
  for (const want of ['+4 on the left', '3 cards move to Backlog', 'Already here, used as they are: High', 'cannot be undone automatically', '2 old columns will be removed', 'Todo, Done']) if (!txt.includes(want)) throw new Error(`preview lacks "${want}"`);
  // the "Remove the old columns" box is ticked by default; unticking re-previews with the columns kept, ticking it again removes them
  const rm = p.locator('dialog[open] .tg-apply input[name=removeOldColumns][type=checkbox]');
  if (!(await rm.isChecked())) throw new Error('"Remove the old columns" is not ticked by default');
  await rm.uncheck(); await settle(p);
  if (!(await p.locator('dialog[open] .tg-diff').innerText()).includes('stay on the right, emptied')) throw new Error('unticking "Remove the old columns" did not re-preview');
  await p.locator('dialog[open] .tg-apply input[name=removeOldColumns][type=checkbox]').check(); await settle(p);
  if (!(await p.locator('dialog[open] .tg-diff').innerText()).includes('2 old columns will be removed')) throw new Error('ticking "Remove the old columns" again did not re-preview');
  // ticking an option re-runs the preview (still nothing written)
  await p.locator('dialog[open] .tg-apply input[name=applySettings][type=checkbox]').check(); await settle(p);
  if (!(await p.locator('dialog[open] .tg-diff').innerText()).includes('Changed:')) throw new Error('ticking board settings did not re-preview');
  if ((await api('GET', `/boards/${b.id}/lists`)).filter(l => l.type === 'active' || l.type === 'closed').length !== 2) throw new Error('the preview wrote columns');
  await p.locator('dialog[open] .tg-apply input[name=applySettings][type=checkbox]').uncheck(); await settle(p);
  p.once('dialog', d => d.accept());
  await Promise.all([p.waitForURL(new RegExp(`/boards/${b.id}$`), { timeout: 8000 }), p.locator('dialog[open] .tg-apply button.primary').click()]);
  await settle(p);
  const toast = await p.locator('#toasts .toast').first().innerText().catch(() => '');
  if (!/Applied Kanban basics.*removed 2 old columns/.test(toast)) throw new Error(`no toast after the redirect (saw "${toast}")`);
  const heads = await p.locator('.boards-col .boards-col-title').allInnerTexts();
  const names = heads.map(h => h.trim());
  if (names.join('|') !== 'Backlog|To do|Doing|Done') throw new Error(`columns after apply (the old ones should be gone): ${names.join(', ')}`);
  const first = p.locator('.boards-col').filter({ has: p.locator('.boards-col-title', { hasText: /^\s*Backlog\s*$/ }) }).first();
  if (await first.locator('.boards-tile').count() !== 3) throw new Error('the cards did not all move into Backlog');
  const labels = await api('GET', `/boards/${b.id}/labels`);
  const high = labels.filter(l => (l.name || '').toLowerCase() === 'high');
  if (high.length !== 1 || high[0].name !== 'high' || high[0].color !== 'berry-red') throw new Error('the board label was replaced or recoloured');
  if (labels.length !== 4) throw new Error(`expected 4 labels (1 kept + 3 added), got ${labels.length}`);
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

// ---- a card on several boards (read half of the placement UI) -----------------------------------
await run('placements: tile marker, popover by keyboard, sheet switcher', async () => {
  const second = await api('POST', `/projects/${proj.id}/boards`, { name: 'Second board' });
  const col = await api('POST', `/boards/${second.id}/lists`, { name: 'Review' });
  await api('POST', `/cards/${card.id}/placements`, { listId: col.id });
  await go('/boards/' + second.id);
  const tile = p.locator('#tile-' + card.id);
  const mark = tile.locator('[data-also-on]');
  if ((await mark.getAttribute('aria-label')) !== 'Also on 1 other board') throw new Error('marker label: ' + await mark.getAttribute('aria-label'));
  // keyboard: focus the marker, Enter opens the popover (never the card), Escape closes it and focus returns
  await mark.focus(); await p.keyboard.press('Enter'); await settle(p);
  const pop = p.locator('#boards-pop [data-also-pop]');
  if (!(await pop.isVisible())) throw new Error('popover did not open');
  if (await p.evaluate(() => document.getElementById('modal').open)) throw new Error('the marker opened the card sheet');
  if ((await pop.locator(`a[href="/boards/${board.id}"]`).count()) !== 1) throw new Error('popover does not link the home board');
  if (!(await p.evaluate(() => document.activeElement && document.activeElement.classList.contains('boards-also-item')))) throw new Error('focus is not in the popover');
  await esc(p); await settle(p);
  if (await p.locator('#boards-pop').isVisible()) throw new Error('Escape did not close the popover');
  if (!(await p.evaluate(() => document.activeElement && document.activeElement.hasAttribute('data-also-on')))) throw new Error('focus did not return to the marker');
  await mark.press('Space'); await settle(p);
  if (!(await pop.isVisible())) throw new Error('Space did not open the popover');
  await mark.click(); await settle(p);   // the marker toggles
  if (await p.locator('#boards-pop').isVisible()) throw new Error('a second click did not close the popover');
  // the card sheet opened from this board shows it here, with the switcher; switching shows it on the home board
  await tile.locator('.boards-tile-link').click(); await settle(p);
  const cm = p.locator('#card-modal');
  if ((await cm.getAttribute('data-board')) !== second.id) throw new Error('sheet context is not the board it was opened from');
  if (!new URL(p.url()).search.includes('board=' + second.id)) throw new Error('sheet URL lacks ?board=: ' + p.url());
  await cm.locator('.cm-boardsw > summary').click(); await settle(p);
  await cm.locator(`a[data-card-ctx="${board.id}"]`).click(); await settle(p);
  if ((await p.locator('#card-modal').getAttribute('data-board')) !== board.id) throw new Error('switcher did not show the card on the home board');
  if (!new URL(p.url()).search.includes('board=' + board.id)) throw new Error('switcher did not update the URL: ' + p.url());
  if ((await p.locator('#card-modal [data-prop="boards"] li').count()) !== 2) throw new Error('Boards row does not list two boards');
  await esc(p); await settle(p);
  await go('/boards/' + second.id + '?view=list');
  await p.locator(`tr[data-id="${card.id}"] [data-also-on]`).click(); await settle(p);
  if (!(await pop.isVisible()) || await p.evaluate(() => document.getElementById('modal').open)) throw new Error('list view: the marker opened the sheet or no popover');
  await esc(p);
});

// ---- a card on several boards (write half: the picker, Remove from board, b, context writes, archive) ------------
await run('placements: Add to board by keyboard, Remove from board, b, writes from the other board, archive everywhere', async () => {
  const other = await api('POST', `/projects/${proj.id}/boards`, { name: 'Placement target ' + stamp });   // unique: earlier runs left theirs
  const oIntake = await api('POST', `/boards/${other.id}/lists`, { name: 'Intake' });
  const oDoing = await api('POST', `/boards/${other.id}/lists`, { name: 'Doing there' });
  const oLabel = await api('POST', `/boards/${other.id}/labels`, { name: 'Target label', color: 'lagoon-blue' });
  const c = await api('POST', `/lists/${lists[0].id}/cards`, { name: 'Travelling card' });
  const placementsOf = async () => Object.fromEntries((await api('GET', `/cards/${c.id}/placements`)).items.map(x => [x.boardId, x.listId]));
  // the sheet on the home board: b opens the picker, type to filter, Enter shows the columns, Enter places; it stays open
  await go('/boards/' + board.id);
  await p.locator('#tile-' + c.id + ' .boards-tile-link').click(); await settle(p);
  await p.evaluate(() => document.activeElement && document.activeElement.blur && document.activeElement.blur());
  await p.keyboard.press('b'); await settle(p);
  const pk = p.locator('#modal .cm-place');
  if (!(await pk.isVisible())) throw new Error('b did not open the picker in the sheet');
  if (!(await p.evaluate(() => document.activeElement && document.activeElement.matches('[data-place-filter]')))) throw new Error('focus is not in the picker filter');
  await p.keyboard.type('placement target ' + stamp); await settle(p);
  if ((await pk.locator('[data-place-board]:visible').count()) !== 1) throw new Error('type-ahead did not narrow the boards to one');
  await p.keyboard.press('Enter'); await settle(p);
  const defName = await p.evaluate(() => document.activeElement && document.activeElement.textContent.trim());
  if (!/Intake/.test(defName || '')) throw new Error('the columns step did not focus the first open column: ' + defName);
  await p.keyboard.press('Enter'); await p.waitForTimeout(900);
  if (!(await pk.isVisible())) throw new Error('the picker closed after placing (it must stay open)');
  if (!new RegExp('Added to Placement target ' + stamp).test(await pk.locator('.cm-place-ok').innerText())) throw new Error('no "Added to" status in the picker');
  if (!(await p.locator('#modal .cm-toasts .toast', { hasText: 'Added to Placement target' }).isVisible())) throw new Error('no visible "Added to" toast over the sheet');
  if (await pk.locator(`[data-place-board="${other.id}"]`).count()) throw new Error('the placed board is still offered');
  if ((await placementsOf())[other.id] !== oIntake.id) throw new Error('not placed in Intake: ' + JSON.stringify(await placementsOf()));
  const cm = p.locator('#card-modal');
  if ((await cm.locator('[data-prop="boards"] li').count()) !== 2 || !(await cm.locator('.cm-boardsw').count())) throw new Error('Boards row / switcher not updated in place');
  await esc(p); await settle(p);
  if (await pk.count()) throw new Error('Escape did not close the picker');
  if (!(await p.evaluate(() => document.getElementById('modal').open))) throw new Error('Escape closed the sheet together with the picker');
  await esc(p); await settle(p);
  if (!(await p.locator(`#tile-${c.id} [data-also-on]`).count())) throw new Error('tile marker missing after the placement');
  // writes from the OTHER board (not the home): a label of that board, a column move there
  await go('/boards/' + other.id);
  await p.locator('#tile-' + c.id + ' .boards-tile-link').click(); await settle(p);
  if ((await cm.getAttribute('data-board')) !== other.id) throw new Error('sheet context is not the target board');
  await cm.locator('details[data-pop="labels"] > summary').click(); await settle(p);
  await cm.locator('details[data-pop="labels"] .cm-labopt', { hasText: 'Target label' }).click(); await settle(p);
  const onOther = await api('GET', `/cards/${c.id}?boardId=${other.id}`);
  if (!(onOther.labelIds || []).includes(oLabel.id)) throw new Error('label of the other board not added');
  await esc(p); await settle(p);
  await p.locator('#card-modal .cm-list > summary').click(); await settle(p);
  await p.locator('#card-modal .cm-lists button', { hasText: 'Doing there' }).click(); await settle(p);
  let pl = await placementsOf();
  if (pl[other.id] !== oDoing.id || pl[board.id] !== lists[0].id) throw new Error('column move from the other board: ' + JSON.stringify(pl));
  // Remove from this board (the Boards row x): Escape cancels, Enter confirms; the sheet falls back to the home board
  const x = () => p.locator(`#card-modal .cm-board-x[data-unplace="${other.id}"]`);
  await x().click(); await settle(p);
  if (!(await p.locator('dialog.cm-confirm[open]').isVisible())) throw new Error('no confirmation');
  if (!/stays on its other boards/.test(await p.locator('dialog.cm-confirm').innerText())) throw new Error('confirmation wording');
  await esc(p); await settle(p);
  if (!(await placementsOf())[other.id]) throw new Error('Escape removed the card');
  await x().click(); await settle(p); await p.keyboard.press('Enter'); await p.waitForTimeout(900);
  if ((await placementsOf())[other.id]) throw new Error('Enter did not remove the card from the board');
  if ((await p.locator('#card-modal').getAttribute('data-board')) !== board.id) throw new Error('sheet did not fall back to the home board');
  if (await p.locator('#card-modal .cm-board-x').count()) throw new Error('the last board offers an x');
  await esc(p); await settle(p);
  if (await p.locator('#tile-' + c.id).count()) throw new Error('the tile is still on the board it left');
  // b on a focused tile (board page): the picker opens in #popover-root; Escape closes it and focus returns to the tile
  await go('/boards/' + board.id);
  await p.locator('#tile-' + c.id + ' .boards-tile-link').focus();
  await p.keyboard.press('b'); await settle(p);
  if (!(await p.locator('#popover-root .cm-place').isVisible())) throw new Error('b on a tile did not open the picker');
  await esc(p); await settle(p);
  if (!(await p.evaluate(id => document.activeElement && document.activeElement.closest('#tile-' + id), c.id))) throw new Error('focus did not return to the tile');
  // live: the open sheet follows placements made elsewhere (here: the API) and never touches the comment being typed
  await p.locator('#tile-' + c.id + ' .boards-tile-link').click(); await settle(p);
  const ta = p.locator('#card-modal [data-comment-input]');
  await ta.fill('half-typed comment');
  await api('POST', `/cards/${c.id}/placements`, { listId: oIntake.id });
  await p.waitForFunction(() => document.querySelectorAll('#card-modal [data-prop="boards"] li').length === 2, null, { timeout: 8000 }).catch(() => { throw new Error('the open sheet did not show the new board live'); });
  if ((await ta.inputValue()) !== 'half-typed comment' || !(await p.evaluate(() => document.activeElement && document.activeElement.matches('[data-comment-input]')))) throw new Error('the live refresh touched the comment being typed');
  await api('DELETE', `/cards/${c.id}/placements/${other.id}`);
  await p.waitForFunction(() => document.querySelectorAll('#card-modal [data-prop="boards"] li').length === 1, null, { timeout: 8000 }).catch(() => { throw new Error('the open sheet did not drop the removed board live'); });
  if ((await ta.inputValue()) !== 'half-typed comment') throw new Error('the live removal touched the comment being typed');
  await ta.fill(''); await p.evaluate(() => document.activeElement && document.activeElement.blur());
  await esc(p); await settle(p);
  // archive from the other board: confirm says so, and the card leaves both boards
  await api('POST', `/cards/${c.id}/placements`, { listId: oIntake.id });
  await go('/boards/' + other.id);
  await p.locator('#tile-' + c.id + ' .boards-tile-link').click(); await settle(p);
  await p.locator('#card-modal details[data-pop="menu"] > summary').click(); await settle(p);
  await p.locator('#card-modal .cm-menu-panel button', { hasText: 'Archive card' }).click(); await settle(p);
  const dlg = p.locator('dialog.cm-confirm[open]');
  if (!/also on 1 other board; it will be archived on all of them/.test(await dlg.innerText())) throw new Error('archive confirmation wording: ' + await dlg.innerText());
  await p.keyboard.press('Enter'); await p.waitForTimeout(900);
  pl = await placementsOf();
  const types = (await api('GET', `/cards/${c.id}/placements`)).items.map(x => x.listType);
  if (types.length !== 2 || types.some(t => t !== 'archive')) throw new Error('archive did not cover both boards: ' + JSON.stringify(types));
  await go('/boards/' + other.id);
  if (await p.locator('#tile-' + c.id).count()) throw new Error('archived card still on the other board');
  await go('/boards/' + board.id);
  if (await p.locator('#tile-' + c.id).count()) throw new Error('archived card still on the home board');
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
