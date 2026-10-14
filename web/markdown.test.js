// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { render } = require('./markdown.js');

test('escapes everything, including inside code and bold', () => {
  const out = render('<img src=x onerror=alert(1)> **<b>** `<script>`\n```html\n<script>alert(1)</script>\n```');
  assert.ok(!/<img|<script|<b><b>/.test(out), out);
  assert.ok(out.includes('&lt;script&gt;alert(1)&lt;/script&gt;'));
});

test('paragraphs, lists and headings are blocks, without stray <br>', () => {
  const out = render('Intro line\nsecond line\n\n## Next steps\n- one\n- two `x`\n\n1. first\n2. second\n\nDone.');
  assert.strictEqual(out,
    '<p>Intro line<br>second line</p><div class="md-h">Next steps</div>' +
    '<ul><li>one</li><li>two <code>x</code></li></ul><ol><li>first</li><li>second</li></ol><p>Done.</p>');
});

test('code block has a language bar, Wrap/Copy, and collapses when long', () => {
  const short = render('```go\nfunc f() {}\n```');
  assert.ok(short.includes('<span class="code-lang">go</span>'));
  assert.ok(short.includes('<pre><code>func f() {}</code></pre>'));
  assert.ok(!short.includes('collapsed'));
  const long = render('```\n' + Array.from({ length: 20 }, (_, i) => 'l' + i).join('\n') + '\n```');
  assert.ok(long.includes('code wrap collapsed') && long.includes('Show all 20 lines'));
});

test('an unclosed fence still renders as code', () => {
  assert.ok(render('look:\n```sh\nmake').includes('<pre><code>make</code></pre>'));
});

test('a lone backtick stays literal', () => {
  assert.strictEqual(render('a ` b'), '<p>a ` b</p>');
});
