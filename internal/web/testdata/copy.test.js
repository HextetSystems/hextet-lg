const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

// The copy-button handler sits between the theme toggle and the search-form script.
const app = fs.readFileSync('internal/web/static/app.js', 'utf8');
const source = app.split('// Copy buttons confirm')[1].split('// Search form behavior is delegated')[0];

function harness({clipboard, status = {textContent: ''}, linkHref = 'https://lg.example/?type=prefix&q=192.0.2.1'} = {}) {
  let click, timers = [];
  const els = {'route-text': {textContent: 'route output'}, 'copy-status': status};
  const document = {addEventListener: (n, f) => { if (n === 'click') click = f; }, getElementById: id => els[id]};
  const ctx = {document, navigator: clipboard === undefined ? {} : {clipboard}, location: {href: linkHref},
    setTimeout: f => { timers.push(f); return timers.length; }, clearTimeout: () => {}};
  vm.runInNewContext('(function(){' + '// Copy buttons confirm' + source + '})()', ctx);
  const button = (attrs, text) => ({dataset: {...attrs}, textContent: text, matches: s => s.split(',').some(x => {const m = x.trim().match(/^\[(data-[a-z-]+)\]$/); return m && Object.keys(attrs).some(k => 'data-' + k.replace(/[A-Z]/g, c => '-' + c.toLowerCase()) === m[1]);})});
  const press = b => click({target: {closest: () => b}});
  return {press, button, timers, status};
}
const settle = async () => { for (let i = 0; i < 6; i++) await Promise.resolve(); };

test('Copy link copies the current location and confirms in label and live region', async () => {
  const written = [];
  const h = harness({clipboard: {writeText: async t => { written.push(t); }}});
  const b = h.button({copyLink: ''}, 'Copy link');
  h.press(b); await settle();
  assert.deepEqual(written, ['https://lg.example/?type=prefix&q=192.0.2.1']);
  assert.equal(b.textContent, 'Copied');
  assert.equal(h.status.textContent, 'Link copied');
  h.timers.shift()();
  assert.equal(b.textContent, 'Copy link');
  assert.equal(h.status.textContent, '');
});

test('Copy text copies the referenced element', async () => {
  const written = [];
  const h = harness({clipboard: {writeText: async t => { written.push(t); }}});
  const b = h.button({copy: 'route-text'}, 'Copy text');
  h.press(b); await settle();
  assert.deepEqual(written, ['route output']);
  assert.equal(h.status.textContent, 'Text copied');
});

test('a rejected clipboard write is reported, not silently swallowed', async () => {
  const h = harness({clipboard: {writeText: async () => { throw new Error('denied'); }}});
  const b = h.button({copyLink: ''}, 'Copy link');
  h.press(b); await settle();
  assert.equal(b.textContent, 'Copy failed');
  assert.equal(h.status.textContent, 'Copy failed');
  h.timers.shift()();
  assert.equal(b.textContent, 'Copy link');
});

test('without a clipboard API the button says so and keeps its label afterwards', async () => {
  const h = harness();
  const b = h.button({copyLink: ''}, 'Copy link');
  h.press(b); await settle();
  assert.equal(b.textContent, 'Copy unavailable');
  assert.match(h.status.textContent, /not available/);
  h.timers.shift()();
  assert.equal(b.textContent, 'Copy link');
});

test('repeated presses keep the original label', async () => {
  const h = harness({clipboard: {writeText: async () => {}}});
  const b = h.button({copyLink: ''}, 'Copy link');
  h.press(b); await settle(); h.press(b); await settle();
  h.timers.splice(0).forEach(f => f());
  assert.equal(b.textContent, 'Copy link');
});

test('clicks elsewhere are ignored; a missing live region is tolerated', async () => {
  const h = harness({clipboard: {writeText: async () => { throw Error('should not be called'); }}, status: undefined});
  const click = h.press;
  assert.doesNotThrow(() => click(null));
  const b = h.button({copyLink: ''}, 'Copy link');
  assert.doesNotThrow(() => h.press(b));
  await settle();
});
