// The thread must never pan sideways: wide content scrolls inside its own
// block, and the page / convo are locked to vertical scroll.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const css = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const rule = (sel) => {
  const i = css.indexOf(sel + '{');
  assert.ok(i >= 0, 'rule not found: ' + sel);
  return css.slice(i, css.indexOf('}', i));
};

test('html/body disable rubber-band overscroll', () => {
  assert.match(rule('html,body'), /overscroll-behavior:none/);
});

test('.convo and .screen clip horizontal overflow (overflow-y:auto alone makes x auto)', () => {
  for (const sel of ['.convo', '.screen']) {
    const r = rule(sel);
    assert.match(r, /overflow-y:auto/);
    assert.match(r, /overflow-x:hidden/);
  }
});

test('.convo is vertical-pan only and contains overscroll', () => {
  const r = rule('.convo');
  assert.match(r, /touch-action:pan-y/);
  assert.match(r, /overscroll-behavior:contain/);
});

test('.msg rows do not add width:100% (margin-left on .msg.reply made them 1px too wide)', () => {
  assert.doesNotMatch(rule('.msg'), /width:100%/);
});

test('wide bash output scrolls in its own pre', () => {
  const r = rule('.msg.bash pre');
  assert.match(r, /white-space:pre/);
  assert.match(r, /overflow-x:auto/);
});

test('composer sits on the home-indicator inset only (max, not inset + 8px)', () => {
  const r = rule('  .composer'); // indented: skip `html.kb .composer`
  assert.match(r, /padding-bottom:max\(var\(--safe-bottom\),8px\)/);
  assert.doesNotMatch(r, /calc\(8px \+ var\(--safe-bottom\)\)/);
});

test('hide-keyboard button exists only while the keyboard is up', () => {
  assert.match(rule('.kb-hide'), /display:none/);
  assert.match(rule('html.kb .kb-hide'), /display:grid/);
});

test('composer textarea is capped at ~4 lines', () => {
  assert.match(rule('.composer textarea'), /max-height:110px/);
});

test('.sheet is sized by inset:0, not a (possibly stale) 100dvh height', () => {
  const r = rule('  .sheet');
  assert.match(r, /inset:0/);
  assert.doesNotMatch(r, /height:/);
});

test('iOS standalone: root and full-screen fixed layers are pinned to 100lvh (web view layout viewport is short by the top inset)', () => {
  const i = css.indexOf('@supports (height:100lvh)');
  assert.ok(i >= 0, 'lvh block missing');
  const blk = css.slice(i, css.indexOf('\n  }\n', i) + 5);
  assert.match(blk, /@media \(display-mode:standalone\)/);
  assert.match(blk, /html,body\{height:100lvh\}/);
  assert.match(blk, /#app,\.scrim,\.sheet,\.docview\{top:0;bottom:auto;height:100lvh\}/);
  // after the base rules, so it wins on equal specificity
  assert.ok(i > css.indexOf('.docview{position:fixed'), 'lvh block must follow .docview');
  assert.ok(i > css.indexOf('#app{position:fixed'), 'lvh block must follow #app');
});

test('home-screen meta stays black-translucent + viewport-fit=cover (the web view is only full-height with both)', () => {
  assert.match(css, /<meta name="viewport"[^>]*viewport-fit=cover/);
  assert.match(css, /apple-mobile-web-app-status-bar-style" content="black-translucent"/);
  assert.match(css, /apple-mobile-web-app-capable" content="yes"/);
});
