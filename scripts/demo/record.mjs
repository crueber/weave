// Records the README demo against a seeded THROWAWAY instance (see seed.sh). Output: $DEMO_DIR/raw/main/*.webm
// (1280x720 desktop, light theme to dark) and $DEMO_DIR/raw/phone/*.webm (390x720). scripts/demo/encode.py stitches them
// into docs/demo.webm + docs/demo.webp (+ the site poster) and the page-text dumps land in $DEMO_DIR/text/*.txt for the PII scan.
//
//   DEMO_DIR=... WEAVE_BASE_URL=http://localhost:18120 PW_CORE=/path/to/playwright-core/index.mjs \
//   PW_CHROMIUM=/path/to/chrome-headless-shell node scripts/demo/record.mjs
//
// Run it on a FRESH seed (not after shots.mjs). It moves cards, places one on the Support Desk board, applies a template
// to Support Desk, posts a comment as Jordan (through his browser session, no API key) and switches Avery to dark.
// Beats are a straight sequence of beat(name, caption, fn); keep the whole thing under 60 seconds (encode.py checks).
// Set SNAP=1 to save a PNG after each beat in $DEMO_DIR/snap.
import fs from 'fs';
import {BASE, DEMO_DIR, state as S, launch, newCtx, overlay, maskKeys, driver} from './lib.mjs';

const RAW = DEMO_DIR + '/raw';
fs.rmSync(RAW, {recursive: true, force: true});
fs.mkdirSync(RAW + '/main', {recursive: true}); fs.mkdirSync(RAW + '/phone', {recursive: true});
if (process.env.SNAP) { fs.rmSync(DEMO_DIR + '/snap', {recursive: true, force: true}); fs.mkdirSync(DEMO_DIR + '/snap', {recursive: true}); }
const TEXT = DEMO_DIR + '/text'; fs.rmSync(TEXT, {recursive: true, force: true});

const b = await launch();
const log = (...a) => console.log(((Date.now() - T0) / 1000).toFixed(1).padStart(5) + 's', ...a);
let T0 = Date.now(), n = 0; const META = {beats: []};

// ---------------------------------------------------------------- desktop
{
  const ctx = await newCtx(b, {record: RAW + '/main'}); await overlay(ctx); await maskKeys(ctx);
  const p = await ctx.newPage(); const tPage = Date.now(); const d = driver(p, TEXT);
  p.on('dialog', dlg => dlg.accept());   // the template apply asks with a native confirm (not visible in the recording)
  p.on('pageerror', e => console.log('PAGEERROR', e.message));
  const board = `${BASE}/boards/${S.board}`;
  const snap = async name => { if (process.env.SNAP) await p.screenshot({path: `${DEMO_DIR}/snap/${String(++n).padStart(2, '0')}-${name}.png`}); };
  const beat = async (name, cap, fn) => { const t = Date.now(); log('beat', name); await d.caption(cap); await fn(); await d.dump(name); await snap(name); META.beats.push([name, (Date.now() - t) / 1000]); };
  const tile = t => p.locator('.boards-tile', {hasText: t}).first();
  const col = id => p.locator(`#col-${id}`);
  // Jordan, in his own browser session: the comment that lights up Avery's bell
  const jordan = await newCtx(b, {user: 'jordan'});

  await p.goto(board); await p.waitForSelector('.boards-tile'); await d.sleep(900); T0 = Date.now(); META.mainOffset = (T0 - tPage) / 1000; log('start');

  await beat('board', 'A calm board: an Inbox for ideas, WIP limits, columns that close cards', async () => {
    await d.hover('.boards-inbox-head').catch(() => {}); await d.sleep(350);
    await d.hover(col(S.lists['In progress']).locator('.boards-wip').first()).catch(() => {}); await d.sleep(450);
    await d.hover(tile('Launch checklist').locator('.boards-also')); await d.sleep(1100);   // the also-on marker
  });

  await beat('inbox', 'Drag an idea from the Inbox onto the board', async () => {
    await d.drag(p.locator('.boards-draft').first(), col(S.lists['Backlog']).locator('.boards-tile').first());
    await d.sleep(700);
  });

  await beat('composer', 'Enter adds a card and keeps you typing', async () => {
    await d.click(col(S.lists['Review']).getByText('Add a card'));
    await d.sleep(120); await d.type('Draft the release notes', 26); await p.keyboard.press('Enter'); await d.sleep(220);
    await d.type('Check the accessibility audit', 26); await p.keyboard.press('Enter'); await d.sleep(350);
    await p.keyboard.press('Escape'); await d.sleep(150);
  });

  await beat('card', 'The card sheet: checklists, labels, due dates, comments', async () => {
    await d.click(tile('Launch checklist')); await p.waitForSelector('#card-modal'); await d.sleep(700);
    const modal = p.locator('#modal');
    await modal.evaluate(m => m.scrollTo({top: 300, behavior: 'smooth'})); await d.sleep(400);
    const box = modal.locator('.x-task:not(.done) input[type=checkbox]').first();
    if (await box.count()) { await d.click(box); await d.sleep(300); }
    await d.click(p.getByPlaceholder(/Write a comment/)); await d.type('Merging the notes now.', 22); await d.sleep(150);
    await p.keyboard.press('Control+Enter'); await d.sleep(550);
  });
  await p.keyboard.press('Escape'); await d.sleep(300);
  await p.evaluate(() => document.activeElement && document.activeElement.blur()); // card keys act on the focused tile first

  await beat('keys', 'Keyboard first: ? lists every shortcut, d sets a due date', async () => {
    await p.keyboard.press('?'); await d.sleep(1300); await p.keyboard.press('Escape'); await d.sleep(250);
    await d.hover(tile('Fix timezone in digest emails')); await d.sleep(250);
    await p.keyboard.press('d'); await d.sleep(800); await p.keyboard.press('2'); await d.sleep(700);
  });

  await beat('multi', 'One card, several boards: b adds it to another team’s board', async () => {
    await d.hover(tile('Fix timezone in digest emails')); await d.sleep(150);
    await p.keyboard.press('b'); await p.waitForSelector('.cm-place'); await d.sleep(500);
    await d.type('Supp', 70); await d.sleep(300); await p.keyboard.press('Enter'); await d.sleep(600);
    await p.keyboard.press('Enter'); await d.sleep(750); await p.keyboard.press('Escape'); await d.sleep(300);
    const mark = tile('Fix timezone in digest emails').locator('.boards-also');
    if (await mark.count()) { await d.click(mark); await d.sleep(850); await p.keyboard.press('Escape'); }
    await d.sleep(200);
  });

  await beat('column', 'Column rules: WIP limit, auto-close, keep the newest N', async () => {
    await d.click(`[data-list-sheet="${S.lists['Done']}"]`); await p.waitForSelector('#list-sheet'); await d.sleep(700);
    await d.hover('[data-prop=closes]'); await d.sleep(250);
    await d.click('details[data-pop=archive] > summary'); await d.sleep(1050);
    await p.keyboard.press('Escape'); await d.sleep(150);
  });
  await p.keyboard.press('Escape'); await d.sleep(300);

  await beat('bell', 'Live: the bell lights up when a teammate comments', async () => {
    await d.hover('#notif-bell'); await d.sleep(200);
    await jordan.request.post(`${BASE}/api/v1/cards/${S.launch}/comments`, {data: {text: 'Release notes look good to me. Ship it!'}});
    await p.waitForFunction(() => /\d/.test(document.getElementById('notif-badge')?.textContent || ''), null, {timeout: 8000}).catch(() => log('WARN no badge'));
    await d.sleep(1100);
    await d.click('#notif-bell'); await p.waitForLoadState('load'); await d.sleep(1300);
  });

  await beat('pulse', 'Pulse: every board’s health on the project page', async () => {
    await p.goto(`${BASE}/projects/${S.project}`); await p.waitForLoadState('load'); await d.sleep(300);
    await d.hover(p.locator('a, article', {hasText: 'Platform Sprint'}).last()); await d.sleep(900);
  });

  await beat('apply', 'Apply a starter template to a board you already have', async () => {
    await d.click(p.locator('a, article', {hasText: 'Support Desk'}).last()); await p.waitForSelector('.boards-tile'); await d.sleep(350);
    await p.goto(`${BASE}/boards/${S.desk}/settings?section=templates`); await p.waitForLoadState('load'); await d.sleep(250);
    await d.click(p.locator('button, a', {hasText: /^Apply a template/}).first()); await p.waitForSelector('#tg .tg-tile'); await d.sleep(400);
    await d.click(p.locator('.tg-tile', {hasText: 'Bug tracker'}).first()); await d.sleep(500);
    await d.click(p.getByText('Preview changes')); await d.sleep(950);
    await d.click(p.locator('.tg-go', {hasText: 'Apply template'})); await p.waitForURL(/\/boards\/[^/]+$/, {timeout: 10000}).catch(() => {}); await p.waitForSelector('.boards-tile'); await d.sleep(1000);
  });

  await beat('agent', 'Connect an agent: a ready-made prompt, never a key', async () => {
    await d.click('.boards-agent-btn'); await p.waitForSelector('#ap'); await d.sleep(350);
    await d.click(p.locator('.ap-tab', {hasText: 'Board'}).first()); await p.waitForSelector('#ap'); await d.sleep(300);
    await d.click('.ap-copy'); await d.sleep(600);
  });
  await p.keyboard.press('Escape'); await d.sleep(200);

  await beat('settings', 'Board owners add their own agents and webhooks', async () => {
    await p.goto(`${board}/settings?section=agents`); await p.waitForLoadState('load'); await d.sleep(300);
    await p.evaluate(() => { const h = [...document.querySelectorAll('h3,h2,strong,b,div')].find(e => e.textContent.trim() === 'Agent accounts'); if (h) h.scrollIntoView({block: 'start', behavior: 'smooth'}); }); await d.sleep(850);
    await d.click(p.locator('a[data-section=webhooks]').first()); await d.sleep(150);
    await p.evaluate(() => window.scrollTo({top: 0, behavior: 'smooth'})); await d.sleep(850);
  });

  await beat('theme', 'Light and dark, from the account menu', async () => {
    await d.click('#user-menu > summary'); await d.sleep(450);
    await d.click('#user-menu [data-theme-set=dark]'); await d.sleep(700);
    await d.move(60, 640); await p.mouse.click(60, 640); await d.sleep(1100);   // close the menu, stay on the page (no reload flash)
  });
  META.mainDur = (Date.now() - T0) / 1000;
  await jordan.close();
  await ctx.close();
  log('desktop done');
}

// ---------------------------------------------------------------- phone glimpse (dark, same account)
{
  const ctx = await newCtx(b, {width: 390, height: 720, theme: 'dark', record: RAW + '/phone', extra: {hasTouch: false}}); await overlay(ctx); await maskKeys(ctx);
  const p = await ctx.newPage(); const tPage = Date.now(); const d = driver(p, TEXT);
  await p.goto(`${BASE}/boards/${S.board}`); await p.waitForSelector('.boards-tile'); await d.caption('Works on a phone too'); META.phoneOffset = (Date.now() - tPage) / 1000; await d.sleep(900);
  await d.dump('phone');
  await p.evaluate(id => document.getElementById('col-' + id)?.scrollIntoView({inline: 'start', block: 'nearest', behavior: 'smooth'}), S.lists['In progress']); await d.sleep(1900);
  await ctx.close();
}
await b.close();
fs.writeFileSync(RAW + '/meta.json', JSON.stringify(META));
console.log('beats', META.beats.map(([k, v]) => k + ' ' + v.toFixed(1)).join(', '));
console.log('raw videos in', RAW);
