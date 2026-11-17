// Records the README demo against a seeded THROWAWAY instance (see seed.sh). Output: $DEMO_DIR/raw/main/*.webm
// (1280x720 desktop, light theme to dark) and $DEMO_DIR/raw/phone/*.webm (390x720). scripts/demo/encode.py stitches them
// into docs/demo.webm + docs/demo.webp and writes the page-text dumps used for the PII scan ($DEMO_DIR/text/*.txt).
//
//   DEMO_DIR=... WEAVE_BASE_URL=http://localhost:18120 PW_CORE=/path/to/playwright-core/index.mjs \
//   PW_CHROMIUM=/path/to/chrome-headless-shell node scripts/demo/record.mjs
//
// Beats are a straight sequence of beat(caption, fn). Set SNAP=1 to save a PNG after each beat in $DEMO_DIR/snap.
import fs from 'fs';
import {BASE, DEMO_DIR, state as S, launch, newCtx, overlay, driver} from './lib.mjs';

const RAW = DEMO_DIR + '/raw';
fs.rmSync(RAW, {recursive: true, force: true});
fs.mkdirSync(RAW + '/main', {recursive: true}); fs.mkdirSync(RAW + '/phone', {recursive: true});
if (process.env.SNAP) fs.mkdirSync(DEMO_DIR + '/snap', {recursive: true});
const TEXT = DEMO_DIR + '/text'; fs.rmSync(TEXT, {recursive: true, force: true});

const b = await launch();
const log = (...a) => console.log(((Date.now() - T0) / 1000).toFixed(1).padStart(5) + 's', ...a);
let T0 = Date.now(), n = 0; const META = {};

// ---------------------------------------------------------------- desktop
{
  const ctx = await newCtx(b, {record: RAW + '/main'}); await overlay(ctx);
  const p = await ctx.newPage(); const tPage = Date.now(); const d = driver(p, TEXT);
  const board = `${BASE}/boards/${S.board}`;
  const snap = async name => { if (process.env.SNAP) await p.screenshot({path: `${DEMO_DIR}/snap/${String(++n).padStart(2, '0')}-${name}.png`}); };
  const beat = async (name, cap, fn) => { log('beat', name); await d.caption(cap); await fn(); await d.dump(name); await snap(name); };
  const tile = t => p.locator('.boards-tile', {hasText: t}).first();

  await p.goto(board); await p.waitForSelector('.boards-tile'); await d.sleep(900); T0 = Date.now(); META.mainOffset = (T0 - tPage) / 1000; log('start');

  await beat('header', 'The board header: breadcrumbs, views, filters, agents, shortcuts', async () => {
    await d.hover('.crumb-proj a'); await d.sleep(200);
    await d.hover('.view-switch a[data-view=list]'); await d.sleep(200);
    await d.hover('.boards-filter input[name=q]'); await d.sleep(200);
    await d.hover('.boards-help-btn'); await d.sleep(200);
    await d.hover('.boards-agent-btn'); await d.sleep(400);
  });

  await beat('composer', 'Add cards fast: Enter creates one and keeps you typing', async () => {
    await d.click(p.locator(`#col-${S.lists['Review']}`).getByText('Add a card'));
    await d.sleep(150); await d.type('Draft the release notes', 32); await p.keyboard.press('Enter'); await d.sleep(300);
    await d.type('Check the accessibility audit', 32); await p.keyboard.press('Enter'); await d.sleep(450);
    await p.keyboard.press('Escape'); await d.sleep(150);
  });

  await beat('drag', 'Drag a card between lists', async () => {
    await d.drag(tile('Export boards to CSV'), p.locator(`#col-${S.lists['Done']} .boards-tile`).first());
    await d.sleep(350);
  });

  await beat('card', 'Card details: description, labels, tasks, due date, comments', async () => {
    await d.click(tile('Launch checklist')); await p.waitForSelector('#card-modal'); await d.sleep(1200);
    const modal = p.locator('#modal');
    await modal.evaluate(m => m.scrollTo({top: 300, behavior: 'smooth'})); await d.sleep(700);
    const box = modal.locator('.x-task:not(.done) input[type=checkbox]').first();
    if (await box.count()) { await d.click(box); await d.sleep(350); }
    await modal.evaluate(m => m.scrollTo({top: 900, behavior: 'smooth'})); await d.sleep(600);
    await d.click(p.getByPlaceholder(/Write a comment/)); await d.type('Merging the notes now.', 24); await d.sleep(200);
    await p.keyboard.press('Control+Enter'); await d.sleep(800);
  });
  await p.keyboard.press('Escape'); await d.sleep(350);

  await beat('column', 'Column settings: description, type and WIP limit', async () => {
    await d.click(`[data-list-sheet="${S.lists['In progress']}"]`); await p.waitForSelector('#list-sheet'); await d.sleep(800);
    await d.click('details[data-pop=wip] > summary'); await d.sleep(250);
    await p.locator('[data-ls-wip-input]').fill(''); await d.type('4', 80); await d.sleep(150);
    await d.click('form[data-ls-form=wip] button.primary'); await d.sleep(800);
  });
  await p.keyboard.press('Escape'); await d.sleep(350);

  await beat('filters', 'Filter by text, label, member or due date', async () => {
    await d.click('.boards-filter details.boards-dd:nth-of-type(1) > summary'); await d.sleep(250);
    await d.click(p.locator('.boards-dd-pop label', {hasText: 'Feature'}).first()); await d.sleep(900);
    await p.keyboard.press('Escape');
    await d.click('.boards-filter input[name=q]'); await d.type('search', 50); await d.sleep(700);
    await p.locator('.boards-filter input[name=q]').fill(''); await p.locator('.boards-filter details.boards-dd').first().evaluate(e => e.removeAttribute('open'));
  });

  await beat('views', 'Views: kanban, list, calendar and map', async () => {
    await p.goto(board); await d.sleep(200);
    await d.click('.view-switch a[data-view=list]'); await p.waitForSelector('.boards-body'); await d.sleep(900);
    await d.click('.view-switch a[data-view=calendar]'); await p.waitForLoadState('load'); await d.sleep(1100);
    await d.click('.view-switch a[data-view=map]'); await p.waitForLoadState('load'); await d.sleep(1100);
  });

  await beat('gallery', 'Start a board from the template gallery', async () => {
    await d.click('.crumb-proj a'); await p.waitForLoadState('load'); await d.sleep(400);
    await d.click(p.locator('.projects-board-new, .boards-new').first()); await p.waitForSelector('#tg .tg-tile'); await d.sleep(900);
    await d.click(p.locator('.tg-tile', {hasText: 'Product roadmap'}).first()); await p.waitForSelector('.tg-pv'); await d.sleep(900);
    await d.click('.tg-create input[name=name]'); await d.type('Apollo Roadmap', 35); await d.sleep(250);
    await d.click('.tg-go'); await p.waitForSelector('.boards-tile'); await d.sleep(800);
  });

  await beat('agent', 'Connect an agent: copy a ready-made prompt, never a key', async () => {
    await d.click('.boards-agent-btn'); await p.waitForSelector('#ap'); await d.sleep(500);
    await d.click(p.locator('.ap-tab', {hasText: 'Board'}).first()); await p.waitForSelector('#ap'); await d.sleep(350);
    await d.click('.ap-copy'); await d.sleep(850);
  });
  await p.keyboard.press('Escape'); await d.sleep(300);

  await beat('settings', 'Board settings: views, cards, sharing and more', async () => {
    await d.click('.boards-settings-btn:not(.boards-agent-btn)'); await p.waitForLoadState('load'); await d.sleep(400);
    await d.click(p.getByText('Views & cards', {exact: true}).first()); await d.sleep(850);
    await d.click(p.getByText('Sharing & public link', {exact: true}).first()); await d.sleep(850);
  });

  await beat('user', 'Your settings, and an admin console for the instance', async () => {
    await p.goto(`${BASE}/settings`); await p.waitForLoadState('load'); await d.sleep(1000);
    await p.goto(`${BASE}/admin`); await p.waitForLoadState('load'); await d.sleep(400);
    await d.click(p.getByText('Users', {exact: true}).first()); await d.sleep(1000);
  });

  await beat('theme', 'Light and dark themes', async () => {
    await p.goto(board); await p.waitForSelector('.boards-tile'); await d.sleep(200);
    await d.click('.theme-toggle [data-theme-set=dark]'); await d.sleep(1200);
  });
  META.mainDur = (Date.now() - T0) / 1000;
  await ctx.close();
  log('desktop done');
}

// ---------------------------------------------------------------- phone glimpse (dark, same account)
{
  const ctx = await newCtx(b, {width: 390, height: 720, theme: 'dark', record: RAW + '/phone', extra: {hasTouch: false}}); await overlay(ctx);
  const p = await ctx.newPage(); const tPage = Date.now(); const d = driver(p, TEXT);
  await p.goto(`${BASE}/boards/${S.board}`); await p.waitForSelector('.boards-tile'); await d.caption('Works on a phone too'); META.phoneOffset = (Date.now() - tPage) / 1000; await d.sleep(1000);
  await d.dump('phone');
  await p.evaluate(() => document.querySelector('.boards-lists, main')?.scrollBy({left: 200, behavior: 'smooth'})); await d.sleep(1300);
  await ctx.close();
}
await b.close();
fs.writeFileSync(RAW + '/meta.json', JSON.stringify(META));
console.log('raw videos in', RAW);
