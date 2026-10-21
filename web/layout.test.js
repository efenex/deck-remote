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
  assert.match(blk, /html:not\(\.kb\),html:not\(\.kb\) body\{height:100lvh\}/);
  for (const sel of ['#app', '.scrim', '.sheet', '.docview']) {
    assert.ok(blk.includes('html:not(.kb) ' + sel), sel + ' must be pinned only while html.kb is off');
  }
  assert.match(blk, /top:0;bottom:auto;height:100lvh\}/);
  // after the base rules, so it wins on equal specificity
  assert.ok(i > css.indexOf('.docview{position:fixed'), 'lvh block must follow .docview');
  assert.ok(i > css.indexOf('#app{position:fixed'), 'lvh block must follow #app');
});

test('home-screen meta stays black-translucent + viewport-fit=cover (the web view is only full-height with both)', () => {
  assert.match(css, /<meta name="viewport"[^>]*viewport-fit=cover/);
  assert.match(css, /apple-mobile-web-app-status-bar-style" content="black-translucent"/);
  assert.match(css, /apple-mobile-web-app-capable" content="yes"/);
});

test('keyboard up: no 100lvh rule can apply (it does not shrink with the keyboard), so the sheet rides on visualViewport', () => {
  // 1. every selector in the lvh block is scoped to html:not(.kb)
  const i = css.indexOf('@supports (height:100lvh)');
  const blk = css.slice(i, css.indexOf('\n  }\n', i) + 5);
  const sels = blk.match(/[^{}\n]+(?=\{[^{}]*100lvh)/g) || [];
  assert.ok(sels.length >= 2);
  for (const group of sels) for (const sel of group.split(',')) {
    assert.match(sel.trim(), /^(?:@media[^{]*\{\s*)?html:not\(\.kb\)/, 'unscoped: ' + sel);
  }
  // 2. applyViewport still toggles html.kb and writes the visual viewport into the sheet
  const js = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const f = js.slice(js.indexOf('function applyViewport()'), js.indexOf('window.visualViewport.addEventListener'));
  assert.match(f, /classList\.toggle\('kb', kb\)/);
  assert.match(f, /sheet\.style\.top = kb \? vv\.offsetTop/);
  assert.match(f, /sheet\.style\.height = kb \? Math\.max\(0, vv\.height - pad\)/);
  assert.match(f, /sheet\.style\.bottom = kb \? 'auto' : ''/);
  // 3. with the keyboard down the inline overrides are cleared, so 100lvh governs again
  assert.match(f, /kb \? [^:]+: ''/);
});

test('layout debug overlay: a chip that never intercepts taps outside itself, with its own close', () => {
  const r = rule('  .dbg-layout');
  assert.match(r, /pointer-events:none/);
  assert.match(rule('  .dbg-bar'), /pointer-events:auto/);
  assert.match(rule('  .dbg-text'), /display:none/);
  assert.match(rule('  .dbg-text'), /pointer-events:none/);
  assert.match(css, /\.dbg-layout\.open \.dbg-text\{display:block\}/);
  // clear of the top bar (safe-top) and the composer/tab bar (bottom): right edge, mid-screen
  assert.doesNotMatch(r, /top:calc\(var\(--safe-top\)/);
  assert.doesNotMatch(r, /bottom:/);
  const js = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const f = js.slice(js.indexOf('function layoutDebug(on)'), js.indexOf("get('debug') === 'layout'"));
  assert.match(f, /el\('button', 'dbg-x'/);
  assert.match(f, /prefs\.debugLayout = false;\s*savePrefs\(prefs\)/); // the x turns the setting off
});

test('inbox cards carry their session id so doc links resolve against that session', () => {
  const js = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  assert.match(js, /card\.dataset\.session = n\.sessionId/);
  assert.match(js, /a\.closest\('\.notif'\)\) session = a\.closest\('\.notif'\)\.dataset\.session/);
});

test('keyboard up in the home-screen app: sheet is sized from visualViewport minus the accessory-bar allowance', () => {
  const js = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const f = js.slice(js.indexOf('function applyViewport()'), js.indexOf('window.visualViewport.addEventListener'));
  assert.match(f, /const pad = kb && isStandalone\(\) \? kbPad\(\) : 0/);
  assert.match(f, /sheet\.style\.height = kb \? Math\.max\(0, vv\.height - pad\) \+ 'px'/);
  assert.match(f, /sheet\.style\.top = kb \? vv\.offsetTop/);
  // allowance: default 0 (vv already excludes the accessory bar), clamped 0..120, persisted
  const k = js.slice(js.indexOf('function kbPad()'), js.indexOf('function applyViewport()'));
  assert.match(k, /: 0;/);
  assert.match(k, /n >= 0 && n <= 120/);
  // the overlay shows kb state and offers -/+
  assert.match(js, /'kb ' \+ document\.documentElement\.classList\.contains\('kb'\)/);
  assert.match(js, /minus\.addEventListener\('click', \(\) => nudge\(-4\)\)/);
});

test('applyViewport detects the keyboard via DeckViewport.keyboardUp, not vv < innerHeight alone', () => {
  const js = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const f = js.slice(js.indexOf('function applyViewport()'), js.indexOf('window.visualViewport.addEventListener'));
  assert.match(f, /DeckViewport\.keyboardUp\(vv, window\.innerHeight, document\.documentElement\.clientHeight\)/);
  assert.doesNotMatch(f, /vv\.height < window\.innerHeight - 120/);
  assert.match(css, /<script src="\/viewport\.js\?v=\d+"><\/script>/);
  assert.match(fs.readFileSync(path.join(__dirname, 'sw.js'), 'utf8'), /'\/viewport\.js\?v=\d+'/);
});

test('composer placeholder is just "Message…" (the / button sits beside it; the long form wrapped in the 1-line input)', () => {
  const js = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  assert.match(js, /ta\.placeholder = 'Message…';/);
  assert.doesNotMatch(js, /for commands/);
});

test('doc viewer tables (real design-all report): wrapper scrolls, table is content-wide, cells wrap by word', () => {
  const md = fs.readFileSync(path.join(__dirname, '..', 'testdata', 'design-all-report-table.md'), 'utf8');
  const html = require('./markdown.js').render(md, { levels: true });
  assert.match(html, /<div class="md-table"><table><thead>/);
  const rows = html.match(/<tr>/g).length;
  assert.ok(rows >= 5, 'rows: ' + rows);
  assert.equal((html.match(/<th[ >]/g) || []).length, 5); // doc | S0 verdict | bucket before → after | forks parked | spec
  assert.match(html, /distributed-production-publisher/);
  // wrapper: scrolls horizontally on touch
  const wrap = rule('  .md-table');
  assert.match(wrap, /overflow-x:auto/);
  assert.match(wrap, /-webkit-overflow-scrolling:touch/);
  // table as wide as its content, never squeezed below the box
  const tbl = rule('  .md-table table');
  assert.match(tbl, /width:max-content/);
  assert.match(tbl, /min-width:100%/);
  // cells: sane widths, wrap by word; NOT overflow-wrap:anywhere (the inherited .docbody value is what
  // squeezed the first column to 3 characters) and no nowrap header that forces a cut-off column
  const cell = rule('  .md-table th,.md-table td');
  assert.match(cell, /min-width:8ch/);
  assert.match(cell, /max-width:28ch/);
  assert.match(cell, /overflow-wrap:break-word/);
  assert.match(cell, /word-break:normal/);
  assert.doesNotMatch(cell, /anywhere|break-all/);
  assert.doesNotMatch(rule('  .md-table th'), /nowrap/);
  assert.match(css, /@media \(max-width:480px\)\{\.md-table th,\.md-table td\{padding:4px 6px\}\}/);
});
