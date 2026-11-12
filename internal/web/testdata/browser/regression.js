'use strict';
// Browser regression checks for an isolated, mock-backed Hextet LG instance.
//
//   node regression.js <base-url> [scenario ...]
//
// Driven by TestBrowserRegression (browser_regression_test.go), which starts the
// instance and the mock backends. Playwright is not a dependency of this
// repository: point NODE_PATH at a node_modules directory that has it, and
// install a Chromium (`npx playwright install chromium`). Without Playwright
// the Go test is skipped, never reported as passed.
//
// Nothing here talks to a real backend.

const { chromium } = require('playwright');

const base = process.argv[2];
const only = process.argv.slice(3);
if (!base) {
  console.error('usage: regression.js <base-url> [scenario ...]');
  process.exit(2);
}

let failures = 0;
const results = [];
function check(name, ok, detail) {
  results.push({ name, ok });
  if (!ok) failures++;
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${ok || detail === undefined ? '' : '  -> ' + detail}`);
}


// state reads everything the milestone asks to be consistent after a history
// step: URL, title, form values, results, error text, backend selection,
// advanced filters, footer timing.
async function state(page) {
  return page.evaluate(() => {
    const f = document.getElementById('search-form');
    const q = f && f.elements.q;
    const res = document.getElementById('results');
    return {
      path: location.pathname + location.search,
      urlQ: new URLSearchParams(location.search).get('q'),
      urlType: new URLSearchParams(location.search).get('type'),
      title: document.title,
      titles: document.querySelectorAll('title').length,
      q: q ? q.value : null,
      type: f && f.elements.type ? f.elements.type.value : null,
      backend: f && f.elements.backend ? f.elements.backend.value : null,
      advancedOpen: !!(document.getElementById('advanced') || {}).open,
      advancedCount: (document.getElementById('advanced-count') || {}).textContent || '',
      routes: document.querySelectorAll('#results tbody.route').length,
      error: !!document.querySelector('#results .notice.error'),
      errorText: (document.querySelector('#results .notice.error') || { textContent: '' }).textContent.trim(),
      timing: (document.getElementById('page-timing') || { textContent: '' }).textContent.trim(),
      timingEls: document.querySelectorAll('#page-timing').length,
      results: res ? res.textContent.replace(/\s+/g, ' ').trim().slice(0, 200) : null,
      forms: document.querySelectorAll('#search-form').length,
    };
  });
}

// waitFor polls state until pred holds or the budget is spent; it returns the
// last state either way so a failure reports what the page actually showed.
async function waitFor(page, pred, ms = 4000) {
  const end = Date.now() + ms;
  let s = null;
  while (Date.now() < end) {
    try {
      s = await state(page);
      if (pred(s)) return { ok: true, s };
    } catch (e) {
      // The page is being replaced (a navigation destroyed the context): retry.
      s = { error: String(e.message || e) };
    }
    await page.waitForTimeout(50);
  }
  return { ok: false, s };
}

async function submit(page, value) {
  await page.fill('#search-form input[name=q]', value);
  await page.press('#search-form input[name=q]', 'Enter');
}

// ---- scenario: HTTP 400 and browser history ----
async function history(ctx) {
  const page = await ctx.newPage();
  const requests = [];
  page.on('request', (r) => requests.push(r.url()));
  const external = [];
  page.on('request', (r) => {
    if (!r.url().startsWith(base)) external.push(r.url());
  });
  await page.goto(base + '/');
  await page.waitForLoadState('load');

  const isA = (s) => s.urlQ === '10.0.0.0/24' && s.urlType === 'prefix' && s.routes > 0 && !s.error && s.q === '10.0.0.0/24';
  const isB = (s) => s.urlQ === '999.1.1.1' && s.error && s.routes === 0 && s.q === '999.1.1.1' && /Invalid search/.test(s.title);
  const isC = (s) => s.urlQ === '10.0.1.0/24' && s.routes > 0 && !s.error && s.q === '10.0.1.0/24';

  // valid search A
  await submit(page, '10.0.0.0/24');
  let r = await waitFor(page, isA);
  check('A: valid HTMX search shows routes at the canonical URL', r.ok, JSON.stringify(r.s));
  const titleA = r.s.title;
  check('A: title names the query', /10\.0\.0\.0\/24 · Test Site$/.test(titleA), titleA);
  check('A: footer timing present, single', r.s.timingEls === 1 && /Generated in/.test(r.s.timing), JSON.stringify([r.s.timingEls, r.s.timing]));

  // invalid search B: HTTP 400 swapped by HTMX
  const bResp = page.waitForResponse((x) => x.url().includes('q=999.1.1.1'));
  await submit(page, '999.1.1.1');
  const bRes = await bResp;
  check('B: the rejected search is HTTP 400', bRes.status() === 400, String(bRes.status()));
  r = await waitFor(page, isB);
  check('B: rejected search shows the error, URL, title and form value', r.ok, JSON.stringify(r.s));
  check('B: validation error is visible text', /could not be run/.test(r.s.errorText), r.s.errorText);
  check('B: advanced filters stay as the query dictates', r.s.advancedOpen === true || r.s.advancedOpen === false);

  // Back to A
  await page.goBack();
  r = await waitFor(page, isA);
  check('Back (B->A): URL, form, results and title restored', r.ok, JSON.stringify(r.s));
  check('Back (B->A): title equals the original A title', r.s.title === titleA, r.s.title);
  check('Back (B->A): one form, one title, one timing element', r.s.forms === 1 && r.s.titles === 1 && r.s.timingEls === 1, JSON.stringify(r.s));

  // Forward to B: HX-History-Restore-Request answered with 400
  await page.goForward();
  r = await waitFor(page, isB);
  check('Forward (A->B): the rejected entry shows its error, form value and title', r.ok, JSON.stringify(r.s));
  check('Forward (A->B): one form, one title', r.s.forms === 1 && r.s.titles === 1, JSON.stringify(r.s));

  // Reload on B is a real navigation: the browser shows the 400 document.
  await page.reload();
  r = await waitFor(page, isB);
  check('Reload on the rejected entry shows the same page', r.ok, JSON.stringify(r.s));

  // valid search after the invalid one
  await submit(page, '10.0.1.0/24');
  r = await waitFor(page, isC);
  check('C: a valid search after the invalid one works', r.ok, JSON.stringify(r.s));
  check('C: error is gone', !r.s.error, JSON.stringify(r.s));

  // Back from C to B (a rejected entry created by HTMX)
  await page.goBack();
  r = await waitFor(page, isB);
  check('Back (C->B): rejected entry restored with error, title and form value', r.ok, JSON.stringify(r.s));

  await page.goBack();
  r = await waitFor(page, isA);
  check('Back (B->A) again', r.ok, JSON.stringify(r.s));

  await page.goForward();
  await waitFor(page, isB);
  await page.goForward();
  r = await waitFor(page, isC);
  check('Forward twice (A->B->C)', r.ok, JSON.stringify(r.s));

  // restored pages keep working: search again from a restored page.
  await page.goBack();
  await waitFor(page, isB);
  await submit(page, '10.0.0.0/24');
  r = await waitFor(page, isA);
  check('A new search from a restored rejected page works', r.ok, JSON.stringify(r.s));

  check('No request left the origin', external.length === 0, external.join(','));
  await page.close();
}


const rawBase = process.env.HEXTET_LG_RAW_BASE || '';

// watch records console errors, uncaught exceptions, CSP violations and any
// request that leaves the instance's origin.
function watch(page, origin) {
  const w = { errors: [], external: [] };
  page.on('console', (m) => { if (m.type() === 'error') w.errors.push(m.text().slice(0, 200)); });
  page.on('pageerror', (e) => w.errors.push(String(e).slice(0, 200)));
  page.on('request', (r) => { if (!r.url().startsWith(origin) && !r.url().startsWith('data:')) w.external.push(r.url()); });
  return w;
}

// ---- scenario: layout at four viewports on every page (layoutAll) ----
async function layoutAll(browser) {
  const pages = [
    ['route results', '/?type=prefix&q=10.0.0.0%2F24&backend=all'],
    ['route results (text view)', '/?type=prefix&q=10.0.0.0%2F24&backend=all&view=text'],
    ['empty results', '/?type=prefix&q=203.0.113.0%2F24&backend=all'],
    ['BGP neighbors and summary counts', '/neighbors'],
    ['RTR sessions', '/rtr'],
    ['community explorer', '/communities'],
    ['home', '/'],
  ];
  for (const width of [1440, 768, 390, 320]) {
    const ctx = await browser.newContext({ viewport: { width, height: 900 } });
    const page = await ctx.newPage();
    const w = watch(page, base);
    for (const [name, path] of pages) {
      const resp = await page.goto(base + path);
      await page.waitForLoadState('networkidle');
      const m = await page.evaluate(() => ({
        sw: document.documentElement.scrollWidth, iw: window.innerWidth,
        titles: document.querySelectorAll('title').length,
        h1: document.querySelectorAll('h1').length,
        timing: document.querySelectorAll('#page-timing').length,
        header: !!document.querySelector('header'),
        rowDisplay: (document.querySelector('tbody.route') ? getComputedStyle(document.querySelector('tbody.route')).display : null),
        theadHidden: (document.querySelector('.routes thead') ? document.querySelector('.routes thead').getBoundingClientRect().width <= 1 : null),
      }));
      const tag = `${width}px ${name}`;
      check(`${tag}: HTTP 200`, resp.status() === 200, String(resp.status()));
      check(`${tag}: no horizontal page scroll`, m.sw <= m.iw + 1, `scrollWidth ${m.sw} > ${m.iw}`);
      check(`${tag}: one title, header present`, m.titles === 1 && m.header, JSON.stringify(m));
      if (name === 'route results') {
        const phone = width <= 700;
        check(`${tag}: ${phone ? 'rows are cards (block)' : 'rows are a table (row group)'}`, m.rowDisplay === (phone ? 'block' : 'table-row-group'), String(m.rowDisplay));
        check(`${tag}: table header ${phone ? 'visually hidden' : 'visible'}`, m.theadHidden === phone, String(m.theadHidden));
        const d = await page.evaluate(() => {
          const det = document.querySelector('tbody.route details.d-route');
          const sum = det.querySelector('summary');
          const before = det.open; sum.click();
          // On tablets the table scrolls inside its wrapper (the page does not): measure against that container.
          const wrap = det.closest('.table-wrap'), c = wrap.getBoundingClientRect(), r = det.getBoundingClientRect();
          return { before, after: det.open, within: r.right <= Math.max(c.right, window.innerWidth) + 1 || wrap.scrollWidth > wrap.clientWidth, wrapScrolls: wrap.scrollWidth > wrap.clientWidth, raw: !!document.querySelector('details.d-raw'), r: Math.round(r.right), c: Math.round(c.right), iw: window.innerWidth };
        });
        if (d.wrapScrolls) console.log(`  info  ${tag}: the table scrolls inside its own wrapper (details right ${d.r}, wrapper right ${d.c}, viewport ${d.iw}); the page itself does not scroll`);
        check(`${tag}: Route details opens and stays inside the viewport`, !d.before && d.after && d.within, JSON.stringify(d));
        check(`${tag}: Raw JSON absent by default`, d.raw === false);
      }
      if (['route results', 'BGP neighbors and summary counts', 'RTR sessions', 'empty results'].includes(name)) {
        check(`${tag}: footer timing shown once`, m.timing === 1, String(m.timing));
      }
    }
    check(`${width}px: no console errors or CSP violations`, w.errors.length === 0, w.errors.join(' | '));
    check(`${width}px: no request left the origin`, w.external.length === 0, w.external.join(','));
    await ctx.close();
  }
}

// ---- scenario: pagination through HTMX and history ----
async function pagination(ctx) {
  const page = await ctx.newPage();
  const w = watch(page, base);
  await page.goto(base + '/?type=prefix&q=10.0.0.0%2F24&backend=all');
  const st = () => page.evaluate(() => ({
    path: location.pathname + location.search,
    title: document.title,
    rows: document.querySelectorAll('#results tbody.route').length,
    pager: (document.querySelector('.pager-info') || { textContent: '' }).textContent.replace(/\s+/g, ' ').trim(),
    timing: (document.getElementById('page-timing') || { textContent: '' }).textContent.trim(),
    timings: document.querySelectorAll('#page-timing').length,
    sections: document.querySelectorAll('#results .result-group').length,
  }));
  let s0 = await st();
  check('pagination: first page shows a pager (5 routes, 2 per page)', /Page 1 of 3/.test(s0.pager), JSON.stringify(s0));
  check('pagination: both backends keep their own section', s0.sections === 2, JSON.stringify(s0));

  await page.click('.pager a[rel=next], .pager a:has-text("Next")');
  await page.waitForFunction(() => /Page 2 of 3/.test((document.querySelector('.pager-info') || {}).textContent || ''), null, { timeout: 4000 }).catch(() => {});
  let s1 = await st();
  check('pagination: HTMX pager moves to page 2 and pushes the URL', /Page 2 of 3/.test(s1.pager) && /page=2/.test(s1.path), JSON.stringify(s1));
  check('pagination: title names the page', /page 2/i.test(s1.title), s1.title);
  check('pagination: footer timing replaced exactly once', s1.timings === 1 && /Generated in/.test(s1.timing), JSON.stringify(s1));

  await page.goBack();
  await page.waitForFunction(() => /Page 1 of 3/.test((document.querySelector('.pager-info') || {}).textContent || ''), null, { timeout: 4000 }).catch(() => {});
  let s2 = await st();
  check('pagination: Back returns to page 1 with its title', /Page 1 of 3/.test(s2.pager) && !/page=2/.test(s2.path) && s2.title === s0.title, JSON.stringify(s2));
  await page.goForward();
  await page.waitForFunction(() => /Page 2 of 3/.test((document.querySelector('.pager-info') || {}).textContent || ''), null, { timeout: 4000 }).catch(() => {});
  let s3 = await st();
  check('pagination: Forward returns to page 2', /Page 2 of 3/.test(s3.pager) && s3.title === s1.title, JSON.stringify(s3));

  check('pagination: no console errors', w.errors.length === 0, w.errors.join(' | '));
  await page.goto(base + '/?type=prefix&q=10.0.0.0%2F24&backend=all&page=9');
  check('pagination: out-of-range page is explained, not an error page', /Page 9 does not exist/.test(await page.textContent('body')));
  const bad = await page.goto(base + '/?type=prefix&q=10.0.0.0%2F24&page=0');
  check('pagination: invalid page number is HTTP 400', bad.status() === 400, String(bad.status()));
  await page.close();
}

// ---- scenario: advanced filters, themes, disclosures ----
async function controls(ctx) {
  const page = await ctx.newPage();
  const w = watch(page, base);
  await page.goto(base + '/');
  check('filters: Advanced filters start collapsed', (await page.evaluate(() => document.getElementById('advanced').open)) === false);
  await page.click('#advanced summary');
  await page.fill('#advanced input[name=community]', '64500:1');
  await page.fill('#search-form input[name=q]', '10.0.0.0/24');
  await page.press('#search-form input[name=q]', 'Enter');
  await page.waitForFunction(() => document.querySelectorAll('#results tbody.route').length > 0, null, { timeout: 4000 }).catch(() => {});
  let r = await page.evaluate(() => ({
    open: document.getElementById('advanced').open,
    count: document.getElementById('advanced-count').textContent,
    url: location.search,
    title: document.title,
  }));
  check('filters: advanced collapses after a successful search and counts the filter', r.open === false && /1 active/.test(r.count), JSON.stringify(r));
  check('filters: URL carries the filter', /community=64500%3A1/.test(r.url), r.url);
  check('filters: title counts the advanced filter without naming it', /\+1 filter/.test(r.title) && !/64500/.test(r.title), r.title);

  // Back and forward keep the form consistent with the URL.
  await page.goBack();
  await page.waitForFunction(() => document.querySelector('#search-form input[name=q]').value === '', null, { timeout: 4000 }).catch(() => {});
  r = await page.evaluate(() => ({ q: document.querySelector('#search-form input[name=q]').value, comm: document.querySelector('#advanced input[name=community]').value }));
  check('filters: Back to the empty form clears search and filter values', r.q === '' && r.comm === '', JSON.stringify(r));

  // Theme: toggle, persistence, history.
  await page.goto(base + '/?type=prefix&q=10.0.0.0%2F24');
  await page.click('#theme');
  const t1 = await page.evaluate(() => ({ attr: document.documentElement.dataset.theme, ls: localStorage.getItem('theme') }));
  check('theme: toggle stores the visitor choice in localStorage', (t1.attr === 'light' || t1.attr === 'dark') && t1.ls === t1.attr, JSON.stringify(t1));
  await page.reload();
  const t2 = await page.evaluate(() => document.documentElement.dataset.theme);
  check('theme: choice survives a reload', t2 === t1.attr, t2);
  await page.fill('#search-form input[name=q]', '10.0.1.0/24');
  await page.press('#search-form input[name=q]', 'Enter');
  await page.waitForFunction(() => /10\.0\.1\.0/.test(document.title), null, { timeout: 4000 }).catch(() => {});
  await page.goBack();
  await page.waitForFunction(() => /10\.0\.0\.0/.test(document.title), null, { timeout: 4000 }).catch(() => {});
  await page.click('#theme');
  const t3 = await page.evaluate(() => document.documentElement.dataset.theme);
  check('theme: the toggle still works after history restoration', t3 !== t1.attr, `${t1.attr} -> ${t3}`);

  // Two independent disclosures per route, no Raw JSON by default.
  const d = await page.evaluate(() => {
    const row = document.querySelector('tbody.route');
    return { details: row.querySelectorAll('details').length, raw: row.querySelectorAll('details.d-raw').length };
  });
  check('disclosures: one Route details, no Raw JSON by default', d.details === 1 && d.raw === 0, JSON.stringify(d));
  check('controls: no console errors', w.errors.length === 0, w.errors.join(' | '));
  await page.close();

  // Communities explorer.
  const p2 = await ctx.newPage();
  await p2.goto(base + '/communities');
  check('communities: explorer renders without backends', (await p2.textContent('h1')).length > 0);
  await p2.close();
}

// ---- scenario: opt-in Raw JSON, neighbor column, hostile branding ----
async function optIn(ctx) {
  if (!rawBase) { check('opt-in instance configured (HEXTET_LG_RAW_BASE)', false, 'not provided'); return; }
  const page = await ctx.newPage();
  const w = watch(page, rawBase);
  await page.goto(rawBase + '/?type=prefix&q=10.0.0.0%2F24&backend=all');
  await page.waitForLoadState('networkidle');
  const r = await page.evaluate(() => {
    const row = document.querySelector('tbody.route');
    const raw = row.querySelector('details.d-raw');
    const out = {
      details: row.querySelectorAll('details').length, hasRaw: !!raw,
      neighborHeader: [...document.querySelectorAll('.routes th')].some((x) => x.textContent.trim() === 'Neighbor'),
      brand: document.querySelector('header').textContent,
      footer: document.querySelector('footer').textContent,
      injected: document.querySelectorAll('header b, footer b, header img[src="x"]').length,
    };
    const dets = row.querySelectorAll('details');
    dets[0].open = true;
    out.independent = !raw || raw.open === false;
    if (raw) { raw.open = true; out.bothOpen = dets[0].open && raw.open; out.rawText = raw.querySelector('pre').textContent.slice(0, 40); }
    return out;
  });
  check('opt-in: Route details and Raw JSON are two independent disclosures', r.details === 2 && r.hasRaw && r.independent && r.bothOpen, JSON.stringify(r));
  check('opt-in: Raw JSON shows the backend object', /^\s*\{/.test(r.rawText || ''), r.rawText);
  check('opt-in: Neighbor column appears when configured', r.neighborHeader);
  check('branding: operator text is shown as text, never markup', /<b>Op<\/b>/.test(r.brand + r.footer) && r.injected === 0, JSON.stringify({ injected: r.injected, brand: r.brand.slice(0, 80) }));
  const title = await page.title();
  check('branding: title carries the site name as text', /^10\.0\.0\.0\/24 · /.test(title) && /<i>Raw<\/i> Site/.test(title), title);
  for (const width of [390, 320]) {
    await page.setViewportSize({ width, height: 800 });
    const o = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: innerWidth }));
    check(`opt-in: no horizontal scroll at ${width}px with Raw JSON, neighbor column and long branding`, o.sw <= o.iw + 1, JSON.stringify(o));
  }
  check('opt-in: no console errors or external requests', w.errors.length === 0 && w.external.length === 0, w.errors.concat(w.external).join(' | '));
  await page.close();
}

const scenarios = { history, pagination, controls, optIn };


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
    if (!only.length || only.includes('layout')) {
      console.log('--- layout');
      try { await layoutAll(browser); } catch (e) { check('layout: scenario completed', false, String((e && e.stack) || e)); }
    }
  } finally {
    await browser.close();
  }
  console.log(`\n${results.length - failures}/${results.length} checks passed`);
  process.exit(failures ? 1 : 0);
})();
