// Regenerates the README / docs screenshots from a freshly seeded throwaway instance (see seed.sh, lib.mjs for env).
//   node scripts/demo/shots.mjs docs/screenshots      (ONLY=regex limits it to matching names)
// Every screenshot is a real Chromium render of fictional data. Run seed.sh on a fresh data dir first (it creates the
// state this script expects), and do not run it right after record.mjs without re-seeding (the demo moves cards).
import fs from 'fs';
import zlib from 'zlib';
import {BASE, state as S, sessions, launch, newCtx} from './lib.mjs';

const OUT = process.argv[2] || 'docs/screenshots';
fs.mkdirSync(OUT, {recursive: true});
const b = await launch();
const wait = ms => new Promise(r => setTimeout(r, ms));

async function shoot(name, {w = 1280, h = 800, theme = 'light', url, user = 'avery', act, clip, anon}) {
  if (process.env.ONLY && !new RegExp(process.env.ONLY).test(name)) return;
  const ctx = await newCtx(b, {width: w, height: h, theme, user});
  if (anon) await ctx.clearCookies();
  const p = await ctx.newPage();
  p.on('pageerror', e => console.log('PAGEERROR', name, e.message));
  await p.goto(BASE + url); await p.waitForLoadState('load'); await wait(900);
  if (act) await act(p, ctx);
  await wait(500);
  await p.screenshot({path: `${OUT}/${name}.png`, ...(clip ? {clip} : {})});
  if (process.env.DEMO_DIR) { // visible text of every page, for the PII scan (grep for emails, paths, tokens)
    fs.mkdirSync(process.env.DEMO_DIR + '/text', {recursive: true});
    fs.writeFileSync(`${process.env.DEMO_DIR}/text/shot-${name}.txt`, await p.evaluate(() => document.body.innerText + '\n' + [...document.querySelectorAll('textarea,input')].map(e => e.value).join('\n') + '\n' + [...document.querySelectorAll('[title],[aria-label],[placeholder],img[alt]')].map(e => e.getAttribute('title') || e.getAttribute('aria-label') || e.getAttribute('placeholder') || e.getAttribute('alt')).join('\n')));
  }
  console.log('shot', name, w + 'x' + h);
  await ctx.close();
}
// API calls as the avery browser session (no API key exists on this instance)
async function api(method, path, body, multipart) {
  const ctx = await newCtx(b); const r = await ctx.request.fetch(BASE + '/api/v1' + path, {method, ...(multipart ? {multipart} : {data: body})});
  const t = await r.text(); await ctx.close();
  if (!r.ok()) throw new Error(`${method} ${path}: ${r.status()} ${t.slice(0, 200)}`);
  return JSON.parse(t).item;
}
const board = `/boards/${S.board}`;
const L = S.lists;

// ---- the main board pair
await shoot('board-light', {w: 1440, h: 900, url: board});
await shoot('board-dark', {w: 1440, h: 900, theme: 'dark', url: board});
await shoot('demo-poster', {w: 1280, h: 720, theme: 'dark', url: board});

// ---- card: full page and sheet
await shoot('card-light', {url: `/cards/${S.launch}`, theme: 'light', h: 800});
await shoot('card-dark', {url: `/cards/${S.launch}`, theme: 'dark', h: 800});
await shoot('card-sheet-light', {h: 1000, url: board, act: async p => { await p.locator('.boards-tile', {hasText: 'Launch checklist'}).first().click(); await p.waitForSelector('#card-modal'); await wait(1200); await p.evaluate(() => document.activeElement && document.activeElement.blur()); }});

// ---- board settings pages
const settings = (section) => async p => { await p.getByText(section, {exact: true}).first().click(); await wait(600); };
await shoot('board-settings-dark', {h: 720, theme: 'dark', url: `${board}/settings`});
await shoot('colors-settings-card-colouring', {h: 700, url: `${board}/settings`, act: settings('Views & cards')});

// ---- list menu (colour picker)
const menu = async p => { await p.locator(`#col-${L['In progress']} details.boards-menu > summary`).click(); await wait(700); };
await shoot('colors-list-menu-dark', {h: 720, theme: 'dark', url: board, act: menu});
await shoot('colors-list-menu-phone-dark', {w: 390, h: 800, theme: 'dark', url: board, act: menu});

// ---- card colouring: pill vs fill, with a board colour
await api('PATCH', `/boards/${S.board}`, {color: 'blue', cardColorMode: 'pill'});
await shoot('colors-board-pill-light', {h: 720, url: board});
await shoot('colors-board-pill-dark', {h: 640, theme: 'dark', url: board});
await api('PATCH', `/boards/${S.board}`, {cardColorMode: 'fill'});
await shoot('colors-board-fill-light', {h: 640, url: board});
await shoot('colors-board-fill-dark', {h: 720, theme: 'dark', url: board});
await api('PATCH', `/boards/${S.board}`, {color: '', cardColorMode: 'pill'});

// ---- project pages: plain banner, then an uploaded (flat, two-tone) background image
await shoot('project-dark', {theme: 'dark', url: `/projects/${S.project}`, h: 800});
await shoot('colors-project-banner-dark', {h: 360, theme: 'dark', url: `/projects/${S.project}`});
{ // flat two-tone PNG, built by hand (no gradients): sky band over a hill band
  const W = 1600, H = 400, raw = Buffer.alloc((W * 3 + 1) * H);
  for (let y = 0; y < H; y++) { raw[y * (W * 3 + 1)] = 0; for (let x = 0; x < W; x++) { const hill = y > 250 + 40 * Math.sin(x / 160); const c = hill ? [47, 110, 98] : [196, 221, 235]; raw.set(c, y * (W * 3 + 1) + 1 + x * 3); } }
  const crc = (() => { const t = []; for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; t[n] = c >>> 0; } return buf => { let c = 0xffffffff; for (const x of buf) c = t[(c ^ x) & 255] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; }; })();
  const chunk = (type, data) => { const l = Buffer.alloc(4); l.writeUInt32BE(data.length); const td = Buffer.concat([Buffer.from(type), data]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([l, td, c]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(W, 0); ihdr.writeUInt32BE(H, 4); ihdr[8] = 8; ihdr[9] = 2;
  const png = Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(raw)), chunk('IEND', Buffer.alloc(0))]);
  try {
    const img = await api('POST', `/projects/${S.project}/background-images`, null, {file: {name: 'hills.png', mimeType: 'image/png', buffer: png}});
    await api('PATCH', `/projects/${S.project}`, {backgroundType: 'image', backgroundImageId: img.id});
    await shoot('colors-project-banner-image', {h: 360, url: `/projects/${S.project}`});
  } catch (e) { console.log('SKIP colors-project-banner-image:', e.message); }
}

// ---- home, settings, login
await shoot('home-light', {url: '/'});
await shoot('home-dark', {theme: 'dark', url: '/'});
await shoot('login-light', {url: '/login?loggedout=1', user: 'avery', anon: true});
await shoot('login-dark-phone', {w: 390, h: 800, theme: 'dark', url: '/login?loggedout=1', anon: true});
await shoot('settings-light-phone', {w: 390, h: 800, url: '/settings'});

// ---- new in this set
await shoot('gallery-light', {url: `/projects/${S.project}`, act: async p => { await p.locator('.projects-board-new, .boards-new').first().click(); await p.waitForSelector('#tg .tg-tile'); await wait(800); }});
await shoot('gallery-preview-dark', {theme: 'dark', url: `/projects/${S.project}`, act: async p => { await p.locator('.projects-board-new, .boards-new').first().click(); await p.waitForSelector('#tg .tg-tile'); await wait(600); await p.locator('.tg-tile', {hasText: 'Product roadmap'}).first().click(); await p.waitForSelector('.tg-pv'); await wait(900); await p.locator('.tg-create input[name=name]').fill('Apollo Roadmap'); }});
await shoot('agent-dialog-dark', {theme: 'dark', url: board, act: async p => { await p.locator('.boards-agent-btn').click(); await p.waitForSelector('#ap'); await wait(500); await p.locator('.ap-tab', {hasText: 'Board'}).first().click(); await p.waitForSelector('#ap'); await wait(400); await wait(600); }});
await shoot('column-sheet-light', {url: board, act: async p => { await p.locator(`[data-list-sheet="${L['In progress']}"]`).click(); await p.waitForSelector('#list-sheet'); await wait(900); }});
await shoot('project-settings-light', {url: `/projects/${S.project}/settings`});
await shoot('admin-dark', {theme: 'dark', url: '/admin'});
await shoot('user-settings-light', {url: '/settings'});
await shoot('calendar-light', {url: `${board}/calendar`});

await b.close();
