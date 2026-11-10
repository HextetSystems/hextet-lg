const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const init = fs.readFileSync('internal/web/static/theme-init.js', 'utf8');
// The first lines of app.js (up to the next top-level statement) hold the theme toggle.
const appHead = fs.readFileSync('internal/web/static/app.js', 'utf8').split('\ndocument.addEventListener("click"')[0];

// A minimal browser: <html> with a dataset, a button that records onclick,
// localStorage (optionally broken) and matchMedia.
function browser({stored, def = 'auto', systemDark = false, brokenStorage = false} = {}) {
  const store = new Map(stored === undefined ? [] : [['theme', stored]]);
  const root = {dataset: {}};
  if (def !== undefined) root.dataset.themeDefault = def;
  const button = {onclick: null};
  const localStorage = {
    getItem: k => { if (brokenStorage) throw new Error('blocked'); return store.has(k) ? store.get(k) : null; },
    setItem: (k, v) => { if (brokenStorage) throw new Error('blocked'); store.set(k, String(v)); },
  };
  const document = {documentElement: root, getElementById: id => id === 'theme' ? button : {}};
  const ctx = {document, localStorage, matchMedia: q => ({matches: q.includes('dark') && systemDark}), navigator: {}, location: {href: 'x'}};
  vm.createContext(ctx);
  return {ctx, root, button, store,
    runInit: () => vm.runInContext(init, ctx),
    runApp: () => vm.runInContext(appHead, ctx),
    click: () => button.onclick()};
}

test('stored choice is applied before paint; invalid values are ignored', () => {
  for (const [stored, want] of [['dark', 'dark'], ['light', 'light'], ['sepia', undefined], ['', undefined], [undefined, undefined]]) {
    const b = browser({stored}); b.runInit();
    assert.equal(b.root.dataset.theme, want, `stored=${stored}`);
  }
});

test('no stored choice leaves the configured default to the stylesheet', () => {
  for (const def of ['auto', 'light', 'dark']) {
    const b = browser({def}); b.runInit();
    assert.equal(b.root.dataset.theme, undefined);
    assert.equal(b.root.dataset.themeDefault, def);
  }
});

test('blocked storage never breaks page load or the toggle', () => {
  const b = browser({brokenStorage: true}); b.runInit(); b.runApp();
  assert.equal(b.root.dataset.theme, undefined);
  b.click();
  assert.equal(b.root.dataset.theme, 'dark', 'toggle still works for this page view');
});

test('auto follows the system preference, then the toggle overrides and persists it', () => {
  const dark = browser({def: 'auto', systemDark: true}); dark.runInit(); dark.runApp();
  dark.click();
  assert.equal(dark.root.dataset.theme, 'light');
  assert.equal(dark.store.get('theme'), 'light');
  const light = browser({def: 'auto', systemDark: false}); light.runInit(); light.runApp();
  light.click();
  assert.equal(light.root.dataset.theme, 'dark');
  assert.equal(light.store.get('theme'), 'dark');
});

test('explicit defaults ignore the system preference until the visitor chooses', () => {
  const d = browser({def: 'dark', systemDark: false}); d.runInit(); d.runApp();
  d.click();
  assert.equal(d.root.dataset.theme, 'light', 'first click leaves the configured dark default');
  const l = browser({def: 'light', systemDark: true}); l.runInit(); l.runApp();
  l.click();
  assert.equal(l.root.dataset.theme, 'dark', 'first click leaves the configured light default');
});

test('a stored choice overrides the configured default and survives reloads', () => {
  const first = browser({def: 'dark'}); first.runInit(); first.runApp();
  first.click();                                  // dark -> light, stored
  const reload = browser({def: 'dark', stored: first.store.get('theme')}); reload.runInit(); reload.runApp();
  assert.equal(reload.root.dataset.theme, 'light');
  reload.click();
  assert.equal(reload.root.dataset.theme, 'dark');
  assert.equal(reload.store.get('theme'), 'dark');
});

test('removing the stored key restores the configured default', () => {
  const b = browser({def: 'light', stored: 'dark'}); b.store.delete('theme'); b.runInit();
  assert.equal(b.root.dataset.theme, undefined);
});

test('a missing toggle or copy button does not throw', () => {
  const b = browser(); b.ctx.document.getElementById = () => null;
  assert.doesNotThrow(() => { b.runInit(); b.runApp(); });
});
