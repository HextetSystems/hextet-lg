'use strict';
// M5.2 browser security checks for isolated, mock-backed Hextet LG instances.
//
//   HEXTET_LG_SEC_BASE=<default instance> HEXTET_LG_SEC_OPTIN=<opt-in instance>
//   HEXTET_LG_SEC_PROXY=<default instance behind a reverse proxy>
//   node security.js [scenario ...]
//
// Driven by TestBrowserSecurity (m52_browser_test.go), which starts the
// instances, a hostile mock backend and the proxy. Playwright is resolved
// through NODE_PATH exactly as for regression.js; it is not a dependency of
// this repository. Nothing here talks to a real backend.
//
// The mock backend puts markup into every text field, a "RAWONLY" marker into
// raw backend objects only, and RTR validator addresses that occur nowhere
// else. A JavaScript dialog (alert) means injected markup executed.

const { chromium } = require('playwright');

const base = process.env.HEXTET_LG_SEC_BASE;
const optin = process.env.HEXTET_LG_SEC_OPTIN;
const proxy = process.env.HEXTET_LG_SEC_PROXY;
const only = process.argv.slice(2);
if (!base || !optin || !proxy) {
  console.error('HEXTET_LG_SEC_BASE, HEXTET_LG_SEC_OPTIN and HEXTET_LG_SEC_PROXY are required');
  process.exit(2);
}

let failures = 0;
const results = [];
function check(name, ok, detail) {
  results.push({ name, ok });
  if (!ok) failures++;
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${ok || detail === undefined ? '' : '  -> ' + String(detail).slice(0, 600)}`);
}

const RTR_PRIVATE = ['10.77.77.77', '2001:db8:7777::7', '8282', '3323', '10.88.88.88'];
const SEARCH = '/?type=prefix&q=10.0.0.0%2F24&backend=a';
const PAGES = ['/', SEARCH, SEARCH + '&view=text', SEARCH + '&view=detail', '/neighbors', '/summary', '/rtr', '/communities',
  '/?type=prefix&q=%22%3E%3Cscript%3Ealert(9)%3C%2Fscript%3E'];

// watch records everything that would betray an injection or a policy gap:
// dialogs, console errors, uncaught exceptions, CSP violation reports, requests
// leaving the origin, and the body of every same-origin text response.
async function watch(page, origin) {
  const w = { dialogs: [], errors: [], csp: [], external: [], bodies: [], failed: [], urls: [] };
  page.on('dialog', (d) => { w.dialogs.push(d.message()); d.dismiss().catch(() => {}); });
  page.on('console', (m) => { if (m.type() === 'error') w.errors.push(m.text().slice(0, 300)); });
  page.on('pageerror', (e) => w.errors.push('pageerror: ' + String(e).slice(0, 300)));
  page.on('request', (r) => {
    const u = r.url();
    w.urls.push(u);
    if (!u.startsWith(origin) && !u.startsWith('data:') && !u.startsWith('about:')) w.external.push(u);
  });
  page.on('response', async (r) => {
    if (r.status() >= 400 && !/[?&]q=(%22|999)/.test(r.url())) w.failed.push(r.status() + ' ' + r.url());
    const ct = r.headers()['content-type'] || '';
    if (/text\/|json|svg/.test(ct)) {
      try { w.bodies.push({ url: r.url(), text: await r.text(), headers: r.headers() }); } catch (e) { /* redirected or aborted */ }
    }
  });
  await page.exposeFunction('__cspReport', (v) => w.csp.push(v));
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', (e) => window.__cspReport(`${e.violatedDirective} ${e.blockedURI}`));
  });
  return w;
}

// A rejected search is deliberately HTTP 400, which Chromium logs as a failed
// resource, and HTMX logs the history restore of one before app.js reloads
// (README, "Rejected searches in history"). Those two messages are expected
// in the scenarios that submit an invalid search; nothing else is.
const unexpected = (w) => w.errors.filter((e) => !/status of 400 \(Bad Request\)|^htmx:historyCacheMissLoadError$/.test(e));
const clean = (w) => w.dialogs.length === 0 && unexpected(w).length === 0 && w.csp.length === 0 && w.external.length === 0;
const report = (w) => JSON.stringify({ dialogs: w.dialogs, errors: unexpected(w), csp: w.csp, external: w.external });

// dom inspects the live document for anything executable that is not one of
// the application's own external scripts.
function dom(page) {
  return page.evaluate(() => {
    const all = [...document.querySelectorAll('*')];
    return {
      html: document.documentElement.outerHTML,
      text: document.body.innerText,
      title: document.title,
      inlineScripts: [...document.scripts].filter((s) => !s.src).length,
      foreignScripts: [...document.scripts].filter((s) => s.src && new URL(s.src).origin !== location.origin).length,
      handlers: all.filter((el) => [...el.attributes].some((a) => /^on/i.test(a.name))).length,
      inlineStyles: all.filter((el) => el.hasAttribute('style')).length,
      styleElements: document.querySelectorAll('style').length,
      jsLinks: all.filter((el) => /^\s*(javascript|data|vbscript):/i.test(el.getAttribute('href') || el.getAttribute('src') || '')).length,
      injected: document.querySelectorAll('img[src="x"], main script, header script, footer script, header b, header i, footer b').length,
      frames: document.querySelectorAll('iframe, object, embed').length,
      scroll: [document.documentElement.scrollWidth, innerWidth],
    };
  });
}

async function openAll(page) {
  await page.evaluate(() => document.querySelectorAll('details').forEach((d) => { d.open = true; }));
}

// ---- hostile backend text, CSP and privacy defaults ----
async function defaults(ctx) {
  const page = await ctx.newPage();
  const w = await watch(page, base);
  for (const p of PAGES) {
    await page.goto(base + p);
    await page.waitForLoadState('networkidle');
    await openAll(page);
    const d = await dom(page);
    const tag = `default ${p.slice(0, 48)}`;
    check(`${tag}: nothing executable was injected`, d.inlineScripts === 0 && d.foreignScripts === 0 && d.handlers === 0 && d.jsLinks === 0 && d.injected === 0 && d.frames === 0,
      JSON.stringify({ ...d, html: undefined, text: undefined }));
    check(`${tag}: no inline styles (style-src 'self')`, d.inlineStyles === 0 && d.styleElements === 0, `${d.inlineStyles} attributes, ${d.styleElements} elements`);
    check(`${tag}: no Raw JSON control or payload`, !/RAWONLY|m52_raw|Raw JSON|d-raw/.test(d.html), (d.html.match(/.{40}(RAWONLY|m52_raw|Raw JSON|d-raw).{40}/) || [''])[0]);
    check(`${tag}: no RTR validator address, port or local address`, !RTR_PRIVATE.some((s) => d.html.includes(s)), RTR_PRIVATE.filter((s) => d.html.includes(s)).join(','));
    check(`${tag}: exactly one title element`, (await page.locator('title').count()) === 1 && d.title.length > 0, d.title);
  }
  // The hostile text is shown, as text.
  await page.goto(base + SEARCH);
  await page.waitForLoadState('networkidle');
  await openAll(page);
  let d = await dom(page);
  check('default: backend markup is visible as literal text in the route table', d.text.includes('<script>alert(1)</script>') && d.text.includes('<img src=x onerror=alert(2)>'), d.text.slice(0, 300));
  await page.goto(base + '/neighbors');
  await page.waitForLoadState('networkidle');
  await openAll(page);
  d = await dom(page);
  check('default: backend markup is visible as literal text on the neighbor page', d.text.includes('<script>alert(1)</script>'), d.text.slice(0, 300));
  await page.goto(base + '/rtr');
  await page.waitForLoadState('networkidle');
  d = await dom(page);
  check('default: the RTR page shows the sessions without addresses', /established/i.test(d.text) && /idle/i.test(d.text) && d.text.includes('<script>alert(1)</script>'), d.text.slice(0, 300));
  const hidden = await page.evaluate((priv) => {
    const out = [];
    document.querySelectorAll('*').forEach((el) => {
      for (const a of el.attributes) if (priv.some((s) => a.value.includes(s))) out.push(`${el.tagName}[${a.name}]`);
    });
    return out;
  }, RTR_PRIVATE);
  check('default: no RTR address in any attribute, tooltip or hidden element', hidden.length === 0, hidden.join(','));

  // Every response body the browser received, not only the final DOM.
  const leakRaw = w.bodies.filter((b) => /RAWONLY|m52_raw/.test(b.text)).map((b) => b.url);
  const leakRTR = w.bodies.filter((b) => RTR_PRIVATE.slice(0, 2).concat('10.88.88.88').some((s) => b.text.includes(s))).map((b) => b.url);
  check('default: no response body carried raw backend JSON', leakRaw.length === 0 && w.bodies.length > 20, `${leakRaw.join(',')} (${w.bodies.length} bodies)`);
  check('default: no response body carried an RTR validator address', leakRTR.length === 0, leakRTR.join(','));
  const ptrAsked = w.urls.filter((u) => u.includes('/ptr-status')).map((u) => decodeURIComponent(u));
  check('default: enrichment ran, and never asked about an RTR validator', ptrAsked.length > 0 && !ptrAsked.some((u) => /10\.77\.77\.77|2001:db8:7777|10\.88\.88\.88/.test(u)), ptrAsked.slice(0, 3).join(' '));
  check('default: no dialog, console error, CSP violation or off-origin request on any page', clean(w), report(w));
  check('default: no failed subresource', w.failed.length === 0, w.failed.join(' | '));
  await page.close();
}

// ---- the policy really is enforced (so "no violations" above means something) ----
async function csp(ctx) {
  const page = await ctx.newPage();
  const w = await watch(page, base);
  await page.goto(base + SEARCH);
  await page.waitForLoadState('networkidle');
  const hdr = (w.bodies.find((b) => b.url === base + SEARCH) || { headers: {} }).headers;
  const policy = hdr['content-security-policy'] || '';
  check('csp: header present on the page, without unsafe-inline, unsafe-eval or wildcards', /script-src 'self'/.test(policy) && /style-src 'self'/.test(policy) && /frame-ancestors 'none'/.test(policy) && /base-uri 'none'/.test(policy) && /object-src 'none'/.test(policy) && !/unsafe-|\*|https?:/.test(policy), policy);
  check('csp: nosniff, no-referrer and frame denial accompany it', hdr['x-content-type-options'] === 'nosniff' && hdr['referrer-policy'] === 'no-referrer' && hdr['x-frame-options'] === 'DENY', JSON.stringify(hdr));
  check('csp: the application itself causes no violation', w.csp.length === 0 && unexpected(w).length === 0, report(w));
  const probe = await page.evaluate(async () => {
    const out = { inlineRan: false, evalRan: false, handlerRan: false, styleApplied: false };
    window.__ran = () => { out.inlineRan = true; };
    const s = document.createElement('script');
    s.textContent = 'window.__ran()';
    document.body.appendChild(s);
    // A string timer is compiled like eval, but outside this (debugger
    // initiated, therefore exempt) evaluation.
    try { setTimeout('window.__evalRan = true', 0); } catch (e) { /* refused synchronously */ }
    await new Promise((res) => setTimeout(res, 100));
    out.evalRan = window.__evalRan === true;
    const b = document.createElement('button');
    b.setAttribute('onclick', 'window.__handler = true');
    document.body.appendChild(b);
    b.click();
    out.handlerRan = window.__handler === true;
    const st = document.createElement('style');
    st.textContent = 'body{outline:7px solid red}';
    document.head.appendChild(st);
    out.styleApplied = getComputedStyle(document.body).outlineWidth === '7px';
    const img = new Image();
    out.offOriginImage = await new Promise((res) => { img.onload = () => res(true); img.onerror = () => res(false); img.src = 'http://127.0.0.1:9/x.png'; });
    return out;
  });
  await page.waitForTimeout(200);
  check('csp: an injected inline script does not run', probe.inlineRan === false, JSON.stringify(probe));
  check('csp: string evaluation (eval-class) is refused', probe.evalRan === false, JSON.stringify(probe));
  check('csp: an injected inline event handler does not run', probe.handlerRan === false, JSON.stringify(probe));
  check('csp: an injected <style> is not applied', probe.styleApplied === false, JSON.stringify(probe));
  check('csp: the browser reported those attempts as violations', w.csp.some((v) => /script-src/.test(v)) && w.csp.some((v) => /style-src/.test(v)) && w.csp.some((v) => /img-src/.test(v)), w.csp.join(' | '));
  // Framing is refused: the page cannot be embedded, even by itself.
  await page.goto(base + '/communities');
  const framed = await page.evaluate(async (src) => {
    const f = document.createElement('iframe');
    document.body.appendChild(f);
    return new Promise((res) => {
      f.onload = () => { try { res(f.contentDocument && f.contentDocument.body ? f.contentDocument.body.innerText.length : -1); } catch (e) { res(-2); } };
      setTimeout(() => res(-3), 1500);
      f.src = src;
    });
  }, base + '/');
  check('csp: the application refuses to be framed', framed <= 0, String(framed));
  await page.close();
}

// ---- opt-in disclosures and hostile operator text ----
async function optIn(ctx) {
  const page = await ctx.newPage();
  const w = await watch(page, optin);
  await page.goto(optin + SEARCH);
  await page.waitForLoadState('networkidle');
  await openAll(page);
  let d = await dom(page);
  const raw = await page.evaluate(() => {
    const pres = [...document.querySelectorAll('details.d-raw pre')];
    return { n: pres.length, text: pres.map((p) => p.textContent).join('\n'), children: pres.reduce((n, p) => n + p.children.length, 0), routes: document.querySelectorAll('tbody.route').length };
  });
  check('opt-in: Raw JSON is shown per route, as text only', raw.n === raw.routes && raw.n > 0 && /RAWONLY-0/.test(raw.text) && raw.text.includes('<script>alert(1)</script>') && raw.children === 0, JSON.stringify({ n: raw.n, routes: raw.routes, children: raw.children }));
  check('opt-in: nothing executable was injected with Raw JSON on', d.inlineScripts === 0 && d.handlers === 0 && d.jsLinks === 0 && d.injected === 0, JSON.stringify({ ...d, html: undefined, text: undefined }));
  const brand = await page.evaluate(() => ({ header: document.querySelector('header').innerText, footer: document.querySelector('footer').innerText, title: document.title, logo: (document.querySelector('img.logo') || {}).naturalWidth || 0 }));
  check('opt-in: hostile site name, subtitle and operator are literal text', brand.header.includes('<img src=x onerror=alert(3)>') && brand.header.includes('<b onmouseover=alert(5)>sub</b>') && brand.footer.includes('<script>alert(4)</script>') && d.injected === 0, JSON.stringify(brand));
  check('opt-in: the title carries the hostile site name as text', brand.title.includes('<img src=x onerror=alert(3)> Site'), brand.title);
  for (const p of ['/neighbors', '/summary']) {
    await page.goto(optin + p);
    await page.waitForLoadState('networkidle');
    await openAll(page);
    d = await dom(page);
    check(`opt-in ${p}: raw backend object shown as text, nothing injected`, /RAWONLY-N/.test(d.text) && d.injected === 0 && d.inlineScripts === 0 && d.handlers === 0, d.text.slice(0, 200));
  }
  await page.goto(optin + '/rtr');
  await page.waitForLoadState('networkidle');
  d = await dom(page);
  check('opt-in: RTR validator addresses are shown when enabled', d.text.includes('10.77.77.77:8282') && d.text.includes('[2001:db8:7777::7]:3323'), d.text.slice(0, 300));
  check('opt-in: the RTR page still has no Raw JSON and no enrichment target', !/RAWONLY|d-raw|data-ptr/.test(d.html));
  const ptrAsked = w.urls.filter((u) => u.includes('/ptr-status')).map((u) => decodeURIComponent(u));
  check('opt-in: shown RTR addresses are still never sent to enrichment', !ptrAsked.some((u) => /10\.77\.77\.77|2001:db8:7777|10\.88\.88\.88/.test(u)), ptrAsked.join(' '));
  for (const width of [390, 320]) {
    await page.setViewportSize({ width, height: 800 });
    for (const p of [SEARCH, '/rtr', '/neighbors']) {
      await page.goto(optin + p);
      await page.waitForLoadState('networkidle');
      await openAll(page);
      const o = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      check(`opt-in ${width}px ${p.slice(0, 20)}: no horizontal page scroll with hostile, long content and open disclosures`, o[0] <= o[1] + 1, JSON.stringify(o));
    }
  }
  check('opt-in: no dialog, console error, CSP violation or off-origin request', clean(w), report(w));
  await page.close();
}

// ---- assets: logo, favicon, stylesheets, scripts, caching ----
async function assets(ctx) {
  const page = await ctx.newPage();
  const w = await watch(page, base);
  const responses = [];
  page.on('response', (r) => responses.push({ url: r.url(), status: r.status(), h: r.headers() }));
  await page.goto(base + '/');
  await page.waitForLoadState('networkidle');
  const a = await page.evaluate(() => ({
    logo: (document.querySelector('img.logo') || {}).naturalWidth || 0,
    logoSrc: (document.querySelector('img.logo') || {}).getAttribute ? document.querySelector('img.logo').getAttribute('src') : '',
    icon: (document.querySelector('link[rel=icon]') || { getAttribute: () => '' }).getAttribute('href'),
    htmx: typeof window.htmx === 'object' && typeof window.htmx.ajax === 'function',
    sheets: [...document.styleSheets].map((s) => { try { return s.cssRules.length; } catch (e) { return -1; } }),
    themed: getComputedStyle(document.documentElement).getPropertyValue('--color-bg').trim() !== '' || getComputedStyle(document.body).backgroundColor !== 'rgba(0, 0, 0, 0)',
    scripts: [...document.scripts].map((s) => s.getAttribute('src')),
  }));
  check('assets: the operator logo rendered', a.logo > 0 && /^\/branding\/logo\?v=[0-9a-f]+$/.test(a.logoSrc), JSON.stringify(a));
  check('assets: HTMX and both stylesheets loaded under the page CSP', a.htmx && a.sheets.length === 2 && a.sheets.every((n) => n > 0) && a.themed, JSON.stringify(a));
  check('assets: every script and the favicon use a fingerprinted same-origin URL', a.scripts.every((s) => /^\/static\/[a-z.-]+\?v=[0-9a-f]+$/.test(s)) && /^\/branding\/favicon\?v=[0-9a-f]+$/.test(a.icon), JSON.stringify([a.scripts, a.icon]));
  const sub = responses.filter((r) => /\/static\/|\/branding\//.test(r.url));
  check('assets: all subresources answered 200', sub.length >= 5 && sub.every((r) => r.status === 200), sub.map((r) => r.status + ' ' + r.url).join(' '));
  check('assets: fingerprinted URLs are immutable, with a validator and nosniff', sub.filter((r) => /[?&]v=/.test(r.url) && !/theme\.css/.test(r.url)).every((r) => /immutable/.test(r.h['cache-control'] || '') && r.h.etag && r.h['x-content-type-options'] === 'nosniff'), sub.map((r) => `${r.url.replace(base, '')} [${r.h['cache-control']}]`).join(' '));
  // The SVG logo opened as a document is inert and sandboxed.
  const svg = await page.goto(base + a.logoSrc);
  check('assets: the SVG logo is served with the sandboxing policy', /default-src 'none'/.test(svg.headers()['content-security-policy'] || '') && /sandbox/.test(svg.headers()['content-security-policy'] || '') && svg.headers()['content-type'] === 'image/svg+xml', JSON.stringify(svg.headers()));
  for (const p of ['/static/', '/branding/', '/branding/logo.svg', '/static/../templates/index.html']) {
    const r = await page.goto(base + p);
    const t = await r.text();
    check(`assets: ${p} serves no listing or file`, r.status() === 404 && !/app\.css|<svg/.test(t), `${r.status()} ${t.slice(0, 60)}`);
  }
  check('assets: no dialog, console error or off-origin request', w.dialogs.length === 0 && w.external.length === 0, report(w));
  await page.close();
}

// ---- behind a reverse proxy: navigation, Copy link, HTMX history ----
async function reverseProxy(ctx) {
  await ctx.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: proxy });
  const page = await ctx.newPage();
  const w = await watch(page, proxy);
  await page.goto(proxy + '/');
  await page.waitForLoadState('networkidle');
  const url = () => page.evaluate(() => location.href);
  const rows = () => page.locator('#results tbody.route').count();
  await page.fill('#search-form input[name=q]', '10.0.0.0/24');
  await page.press('#search-form input[name=q]', 'Enter');
  await page.waitForFunction(() => document.querySelectorAll('#results tbody.route').length > 0);
  const a = await url();
  check('proxy: an HTMX search stays on the proxy origin with the canonical URL', a.startsWith(proxy + '/?type=prefix&q=10.0.0.0%2F24') && (await rows()) > 0, a);
  await page.click('[data-copy-link]');
  await page.waitForTimeout(150);
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  check('proxy: Copy link copies the public (proxy) URL of the current search', copied === a, copied);
  await page.fill('#search-form input[name=q]', '10.0.1.0/24');
  await page.press('#search-form input[name=q]', 'Enter');
  await page.waitForFunction(() => new URLSearchParams(location.search).get('q') === '10.0.1.0/24');
  await page.fill('#search-form input[name=q]', '999.1.1.1');
  await page.press('#search-form input[name=q]', 'Enter');
  await page.waitForFunction(() => new URLSearchParams(location.search).get('q') === '999.1.1.1' && !!document.querySelector('#results .notice.error'));
  await page.goBack();
  await page.waitForFunction(() => new URLSearchParams(location.search).get('q') === '10.0.1.0/24' && document.getElementById('search-form').elements.q.value === '10.0.1.0/24' && document.querySelectorAll('#results tbody.route').length > 0);
  await page.goBack();
  await page.waitForFunction(() => new URLSearchParams(location.search).get('q') === '10.0.0.0/24' && document.getElementById('search-form').elements.q.value === '10.0.0.0/24');
  check('proxy: Back restores earlier searches from the server (form, URL, results)', (await url()) === a && (await rows()) > 0, await url());
  await page.goForward();
  await page.goForward();
  await page.waitForFunction(() => new URLSearchParams(location.search).get('q') === '999.1.1.1' && !!document.querySelector('#results .notice.error') && document.getElementById('search-form').elements.q.value === '999.1.1.1');
  check('proxy: Forward to a rejected search (HTTP 400) shows it under the proxy origin', (await url()).startsWith(proxy + '/?'), await url());
  // Ordinary navigation and pagination links are relative, so they stay put.
  await page.goto(a);
  await page.waitForLoadState('networkidle');
  const links = await page.evaluate(() => [...document.querySelectorAll('a[href], form[action], link[href], script[src], img[src]')].map((el) => el.getAttribute('href') || el.getAttribute('action') || el.getAttribute('src')));
  check('proxy: every link, form and asset reference is path-only', links.length > 10 && links.every((h) => h.startsWith('/') && !h.startsWith('//')), links.filter((h) => !h.startsWith('/') || h.startsWith('//')).join(' '));
  for (const nav of ['Neighbors', 'Summary', 'RTR sessions', 'Communities', 'Routes']) {
    await page.click(`nav >> text="${nav}"`);
    await page.waitForLoadState('networkidle');
    const u = await url();
    check(`proxy: navigation to ${nav} stays on the proxy origin`, u.startsWith(proxy + '/') && (await page.locator('title').count()) === 1, u);
  }
  const html = await page.content();
  check('proxy: the page names neither the upstream address nor a forwarded host', !html.includes(base.replace('http://', '')) && !/X-Forwarded/i.test(html), base);
  check('proxy: no request went anywhere but the proxy, no console error or CSP violation', clean(w), report(w));
  await page.close();
}

// ---- light and dark themes ----
async function themes(ctx) {
  const page = await ctx.newPage();
  const w = await watch(page, base);
  await page.goto(base + SEARCH);
  await page.waitForLoadState('networkidle');
  const look = () => page.evaluate(() => ({ theme: document.documentElement.dataset.theme || '', stored: localStorage.getItem('theme'), bg: getComputedStyle(document.body).backgroundColor, fg: getComputedStyle(document.body).color, cookie: document.cookie }));
  const seen = {};
  for (let i = 0; i < 2; i++) {
    await page.click('#theme');
    const s = await look();
    seen[s.theme] = s;
    check(`themes: toggle ${i + 1} sets and stores "${s.theme}" without a cookie`, (s.theme === 'light' || s.theme === 'dark') && s.stored === s.theme && s.cookie === '', JSON.stringify(s));
    await page.reload();
    await page.waitForLoadState('networkidle');
    const again = await look();
    check(`themes: "${s.theme}" survives a reload and paints the same colors`, again.theme === s.theme && again.bg === s.bg, JSON.stringify(again));
    for (const width of [1440, 390, 320]) {
      await page.setViewportSize({ width, height: 850 });
      await openAll(page);
      const o = await page.evaluate(() => [document.documentElement.scrollWidth, innerWidth]);
      check(`themes: ${s.theme} at ${width}px has no horizontal page scroll with hostile backend text`, o[0] <= o[1] + 1, JSON.stringify(o));
    }
    await page.setViewportSize({ width: 1440, height: 900 });
  }
  check('themes: light and dark really differ', seen.light && seen.dark && seen.light.bg !== seen.dark.bg && seen.light.fg !== seen.dark.fg, JSON.stringify(seen));
  check('themes: no dialog, console error, CSP violation or off-origin request', clean(w), report(w));
  await page.close();
}

const scenarios = { defaults, csp, optIn, assets, reverseProxy, themes };

(async () => {
  const browser = await chromium.launch();
  try {
    for (const [name, fn] of Object.entries(scenarios)) {
      if (only.length && !only.includes(name)) continue;
      console.log(`--- ${name}`);
      const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
      try {
        await fn(ctx);
      } catch (e) {
        check(`${name}: scenario completed`, false, String((e && e.stack) || e));
      }
      await ctx.close();
    }
  } finally {
    await browser.close();
  }
  console.log(`\n${results.length - failures}/${results.length} checks passed`);
  process.exit(failures ? 1 : 0);
})();
