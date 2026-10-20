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
