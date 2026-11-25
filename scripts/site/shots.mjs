// Screenshots for the project website (site/img), from an instance seeded by scripts/site/seed.sh.
//   node scripts/site/shots.mjs OUT_DIR          (ONLY=regex limits it to matching names)
// Env as in scripts/demo/lib.mjs (DEMO_DIR, WEAVE_BASE_URL, PW_CORE, PW_CHROMIUM, LD_LIBRARY_PATH, FONTCONFIG_FILE).
// WEAVE_CLI=/path/to/weave also starts `weave listen` as the seeded agent while shooting, so the board shows a listener.
// Writes PNGs plus the visible text of every page (OUT_DIR/text/*.txt) for the PII scan; convert with scripts/site/webp.py.
// Images: week, platform, calendar, timeline-list, pulse, gallery, claim (card sheet with an agent's claim), multiboard
// (a card on two boards, with the Add to board picker), rules (a column sheet: auto-close and keep the newest N), phone.
// Agent key prefixes are masked before each screenshot (MASK_KEYS in scripts/demo/lib.mjs).
import fs from 'fs';
import {spawn} from 'child_process';
import {BASE, state as S, launch, newCtx, MASK_KEYS} from '../demo/lib.mjs';

const OUT = process.argv[2];
if (!OUT) throw new Error('usage: node scripts/site/shots.mjs OUT_DIR');
fs.mkdirSync(OUT + '/text', {recursive: true});
const wait = ms => new Promise(r => setTimeout(r, ms));

let listener = null;
if (process.env.WEAVE_CLI) {
  const key = fs.readFileSync(process.env.DEMO_DIR + '/agent.key', 'utf8').trim();
  listener = spawn(process.env.WEAVE_CLI, ['listen', '--board', 'Apollo Platform', '--list', 'Sprint'], {
    env: {...process.env, WEAVE_URL: BASE, WEAVE_TOKEN: key, HOME: process.env.DEMO_DIR}, stdio: 'ignore'});
  await wait(2500);
}

const b = await launch();
async function shoot(name, {w = 1280, h = 800, theme = 'light', url, act, full = false}) {
  if (process.env.ONLY && !new RegExp(process.env.ONLY).test(name)) return;
  const ctx = await newCtx(b, {width: w, height: h, theme});
  const p = await ctx.newPage();
  p.on('pageerror', e => console.log('PAGEERROR', name, e.message));
  await p.goto(BASE + url); await p.waitForLoadState('load'); await wait(1000);
  if (act) await act(p);
  await wait(600);
  await p.evaluate(MASK_KEYS);
  await p.screenshot({path: `${OUT}/${name}.png`, fullPage: full});
  fs.writeFileSync(`${OUT}/text/${name}.txt`, await p.evaluate(() => document.body.innerText + '\n' +
    [...document.querySelectorAll('textarea,input')].map(e => e.value).join('\n') + '\n' +
    [...document.querySelectorAll('[title],[aria-label],[placeholder],img[alt]')].map(e => e.getAttribute('title') || e.getAttribute('aria-label') || e.getAttribute('placeholder') || e.getAttribute('alt')).join('\n')));
  console.log('shot', name, w + 'x' + h);
  await ctx.close();
}
const gallery = async p => { await p.locator('.projects-board-new, .boards-new').first().click(); await p.waitForSelector('#tg .tg-tile'); await wait(800); };
const inProgress = async p => { await p.evaluate(id => document.getElementById('col-' + id)?.scrollIntoView({inline: 'start', block: 'nearest'}), S.platformLists['In progress']); await wait(400); };
const blur = p => p.evaluate(() => document.activeElement && document.activeElement.blur());
const multi = async p => { await p.locator('.boards-tile', {hasText: 'Apollo 2.0 launch checklist'}).first().click(); await p.waitForSelector('#card-modal'); await wait(1200); await blur(p); await p.locator('.cm-board-add').first().click(); await p.waitForSelector('.cm-place'); await wait(800); };
const rules = async p => { const id = S.platformLists['Done']; await p.locator(`#col-${id} details.boards-menu > summary`).click(); await p.locator(`.boards-menu-pop[data-list="${id}"] [data-list-sheet]`).click(); await p.waitForSelector('#list-sheet'); await wait(1000); await blur(p); };
const sheet = async p => { await p.locator('.boards-tile', {hasText: 'Triage new crash reports'}).first().click(); await p.waitForSelector('#card-modal'); await wait(1200); await p.evaluate(() => document.activeElement && document.activeElement.blur()); };

for (const theme of ['light', 'dark']) {
  await shoot(`week-${theme}`, {theme, w: 1480, h: 440, url: `/boards/${S.week}`});
  await shoot(`platform-${theme}`, {theme, w: 1480, h: 680, url: `/boards/${S.platform}`});
  await shoot(`calendar-${theme}`, {theme, h: 720, url: `/boards/${S.timeline}/calendar?month=${S.timelineMonth}`});
  await shoot(`timeline-list-${theme}`, {theme, h: 720, url: `/boards/${S.timeline}?view=list`});
  await shoot(`pulse-${theme}`, {theme, h: 660, url: `/projects/${S.apollo}`});
  await shoot(`gallery-${theme}`, {theme, url: `/projects/${S.apollo}`, act: gallery});
  await shoot(`claim-${theme}`, {theme, url: `/boards/${S.platform}`, act: sheet});
  await shoot(`multiboard-${theme}`, {theme, url: `/boards/${S.platform}`, act: multi});
  await shoot(`rules-${theme}`, {theme, url: `/boards/${S.platform}`, act: rules});
  await shoot(`phone-${theme}`, {theme, w: 390, h: 800, url: `/boards/${S.platform}`, act: inProgress});
}

await b.close();
if (listener) listener.kill('SIGTERM');
