// Shared helpers for record.mjs and shots.mjs. No machine-specific defaults: everything comes from the environment.
//   DEMO_DIR          work dir written by seed.sh (sessions.json, state.json)
//   WEAVE_BASE_URL    where the throwaway instance listens, e.g. http://localhost:18120
//   PW_CORE           path to playwright-core's index.mjs (default: the bare module name "playwright-core")
//   PW_CHROMIUM       path to a chromium / chrome-headless-shell executable (default: Playwright's own)
//   LD_LIBRARY_PATH / FONTCONFIG_FILE   passed through to the browser when set (real fonts matter for the look)
import fs from 'fs';

export const DEMO_DIR = process.env.DEMO_DIR;
export const BASE = process.env.WEAVE_BASE_URL || 'http://localhost:18120';
export const PUBLIC_URL = 'https://weave.example.com'; // what the agent prompts show instead of BASE
if (!DEMO_DIR) throw new Error('set DEMO_DIR (see seed.sh)');
export const state = JSON.parse(fs.readFileSync(DEMO_DIR + '/state.json', 'utf8'));
export const sessions = JSON.parse(fs.readFileSync(DEMO_DIR + '/sessions.json', 'utf8'));
const pw = await import(process.env.PW_CORE || 'playwright-core');
export const chromium = pw.chromium;

export function launch() {
  return chromium.launch({
    args: ['--disable-dev-shm-usage', '--disable-gpu'],
    executablePath: process.env.PW_CHROMIUM || undefined,
    env: process.env,
  });
}

export async function newCtx(b, {width = 1280, height = 720, theme = 'light', user = 'avery', record = null, scale = 1, extra = {}} = {}) {
  const ctx = await b.newContext({
    viewport: {width, height}, colorScheme: theme, deviceScaleFactor: scale, bypassCSP: true, locale: 'en-US', timezoneId: 'UTC',
    permissions: ['clipboard-read', 'clipboard-write'],
    ...(record ? {recordVideo: {dir: record, size: {width, height}}} : {}), ...extra,
  });
  // The agent prompt's install step embeds the project's public download URL (a real account name): always ask for the
  // variant without install steps, so no frame or screenshot ever shows it. The UI is unchanged (the toggle shows as checked).
  // The prompt also names the instance's own URL (http://localhost:PORT here): show a neutral weave.example.com instead.
  await ctx.route('**/ui/agent-prompt*', async r => {
    const u = new URL(r.request().url()); u.searchParams.set('noinstall', '1');
    const resp = await r.fetch({url: u.toString()});
    const host = new URL(BASE).host;
    const body = (await resp.text()).split(BASE).join(PUBLIC_URL).split(host).join(new URL(PUBLIC_URL).host);
    await r.fulfill({response: resp, body});
  });
  await ctx.addCookies([{name: 'weave_session', value: sessions[user].session, url: BASE}]);
  return ctx;
}

// Recording-only overlay: a cursor dot with a click ripple and a bottom caption bar. Pure CSS tokens (no gradients).
// Both live in the top layer (popover) so open dialogs never cover them. State survives full page loads via sessionStorage.
export const OVERLAY = () => {
  const css = `
  #demo-cur,#demo-cap{position:fixed;margin:0;padding:0;border:0;background:none;overflow:visible;pointer-events:none;inset:auto}
  #demo-cur{left:0;top:0;width:0;height:0}
  #demo-cur i{position:absolute;left:-9px;top:-9px;width:18px;height:18px;border-radius:50%;background:var(--accent);border:2px solid var(--surface);box-shadow:var(--shadow-2);transition:transform .08s var(--ease)}
  #demo-cur.dn i{transform:scale(.8)}
  #demo-cur b{position:absolute;left:-17px;top:-17px;width:34px;height:34px;border-radius:50%;border:2px solid var(--accent);opacity:0}
  #demo-cur.rip b{animation:demo-rip .5s var(--ease)}
  @keyframes demo-rip{from{opacity:.9;transform:scale(.4)}to{opacity:0;transform:scale(1.5)}}
  #demo-cap{left:50%;bottom:18px;transform:translateX(-50%);max-width:calc(100vw - 32px);width:max-content}
  #demo-cap div{font:600 15px/1.3 var(--font-sans);color:var(--fg);background:var(--surface);border:1px solid var(--border-strong);border-radius:var(--radius-pill);padding:9px 18px;box-shadow:var(--shadow-3);text-align:center;transition:opacity .2s var(--ease)}
  #demo-cap div:empty{opacity:0}`;
  const boot = () => {
    if (document.getElementById('demo-cur')) return;
    const st = document.createElement('style'); st.textContent = css; document.head.appendChild(st);
    const cur = document.createElement('div'); cur.id = 'demo-cur'; cur.setAttribute('popover', 'manual'); cur.innerHTML = '<b></b><i></i>';
    const cap = document.createElement('div'); cap.id = 'demo-cap'; cap.setAttribute('popover', 'manual'); cap.innerHTML = '<div></div>';
    document.body.append(cur, cap);
    const put = (x, y) => { cur.style.left = x + 'px'; cur.style.top = y + 'px'; try { sessionStorage.setItem('demo-xy', x + ',' + y); } catch (e) { /* */ } };
    let xy = [-40, -40]; try { xy = (sessionStorage.getItem('demo-xy') || '-40,-40').split(',').map(Number); } catch (e) { /* */ }
    put(xy[0], xy[1]);
    window.__cap = t => { try { sessionStorage.setItem('demo-cap', t || ''); } catch (e) { /* */ } cap.firstChild.textContent = t || ''; };
    try { window.__cap(sessionStorage.getItem('demo-cap') || ''); } catch (e) { /* */ }
    const top = () => { for (const el of [cap, cur]) { try { el.hidePopover(); el.showPopover(); } catch (e) { /* */ } } };
    top(); setInterval(() => { if (document.querySelector('dialog[open]')) top(); }, 200);
    addEventListener('pointermove', e => put(e.clientX, e.clientY), true);
    addEventListener('pointerdown', e => { put(e.clientX, e.clientY); cur.classList.add('dn'); cur.classList.remove('rip'); void cur.offsetWidth; cur.classList.add('rip'); }, true);
    addEventListener('pointerup', () => cur.classList.remove('dn'), true);
  };
  if (document.readyState === 'loading') addEventListener('DOMContentLoaded', boot); else boot();
};

export async function overlay(ctx) { await ctx.addInitScript(OVERLAY); }

// Never show even a key prefix (an agent account row reads "Key wk_1a2b..."): rewrite such text nodes as they appear.
export const MASK_KEYS = () => {
  const fix = root => {
    const w = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n; (n = w.nextNode());) if (/wk_[0-9A-Za-z]/.test(n.nodeValue)) n.nodeValue = n.nodeValue.replace(/wk_[0-9A-Za-z]+(\.\.\.|…)?/g, 'wk_••••');
  };
  const boot = () => { fix(document.body); new MutationObserver(ms => { for (const m of ms) for (const n of m.addedNodes) fix(n.nodeType === 1 ? n : n.parentNode || document.body); }).observe(document.body, {childList: true, subtree: true}); };
  if (document.readyState === 'loading') addEventListener('DOMContentLoaded', boot); else boot();
};
export async function maskKeys(ctx) { await ctx.addInitScript(MASK_KEYS); }

export function driver(p, textDir) {
  let mx = 640, my = 360;
  const sleep = ms => p.waitForTimeout(ms);
  const d = {
    sleep,
    async caption(t) { await p.evaluate(t => window.__cap && window.__cap(t), t); },
    // Smooth, eased pointer travel in real time (the recording only sees what is actually on screen).
    async move(x, y, ms) {
      const dist = Math.hypot(x - mx, y - my); const T = ms ?? Math.min(420, Math.max(160, dist / 2.4)); const n = Math.max(4, Math.round(T / 16));
      const x0 = mx, y0 = my;
      for (let i = 1; i <= n; i++) { const t = i / n, e = t < .5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2; await p.mouse.move(x0 + (x - x0) * e, y0 + (y - y0) * e); await sleep(T / n); }
      mx = x; my = y;
    },
    async hover(loc, {ms, dx = 0, dy = 0} = {}) {
      const l = typeof loc === 'string' ? p.locator(loc).first() : loc;
      await l.scrollIntoViewIfNeeded(); const r = await l.boundingBox();
      await d.move(r.x + r.width / 2 + dx, r.y + r.height / 2 + dy, ms);
    },
    async click(loc, opts = {}) { await d.hover(loc, opts); await sleep(opts.pause ?? 120); await p.mouse.down(); await sleep(60); await p.mouse.up(); },
    async type(text, delay = 55) { await p.keyboard.type(text, {delay}); },
    async dump(name) {
      if (!textDir) return; fs.mkdirSync(textDir, {recursive: true});
      fs.writeFileSync(`${textDir}/${name}.txt`, await p.evaluate(() => document.body.innerText + '\n' + [...document.querySelectorAll('textarea,input')].map(e => e.value).join('\n') + '\n' + document.title + '\n' + [...document.querySelectorAll('[title],[aria-label],[placeholder],img[alt]')].map(e => e.getAttribute('title') || e.getAttribute('aria-label') || e.getAttribute('placeholder') || e.getAttribute('alt')).join('\n')));
    },
    async drag(from, to) {
      await d.hover(from); await sleep(200); await p.mouse.down(); await sleep(120);
      const r = await to.boundingBox(); await d.move(r.x + r.width / 2, r.y + 10, 800); await sleep(350); await p.mouse.up();
    },
    pos: () => [mx, my],
  };
  return d;
}
