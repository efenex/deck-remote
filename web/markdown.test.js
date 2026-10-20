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

test('tables: header, alignment, escaped cells, inline code; not inside fences', () => {
  const out = render('| Name | N |\n|:--|--:|\n| <b>x</b> | `a|b` |\n| y | 2 |\n\nafter');
  assert.ok(out.includes('<table><thead><tr><th style="text-align:left">Name</th><th style="text-align:right">N</th></tr></thead>'), out);
  assert.ok(out.includes('<td style="text-align:left">&lt;b&gt;x&lt;/b&gt;</td>'), out);
  assert.ok(out.includes('<tr><td style="text-align:left">y</td><td style="text-align:right">2</td></tr>'), out);
  assert.ok(out.endsWith('<p>after</p>'), out);
  assert.ok(!render('```\n| a | b |\n|---|---|\n```').includes('<table'));
  assert.ok(!render('a | b\nnot a table').includes('<table'));
});

test('nested lists follow indentation, mixed ul/ol, flat lists unchanged', () => {
  const out = render('- one\n  - one.a\n  - one.b\n    1. deep\n- two\n\n1. x\n2. y');
  assert.strictEqual(out,
    '<ul><li>one<ul><li>one.a</li><li>one.b<ol><li>deep</li></ol></li></ul></li><li>two</li></ul><ol><li>x</li><li>y</li></ol>');
  assert.strictEqual(render('    - indented first\n- back'), '<ul><li>indented first</li><li>back</li></ul>');
});

test('heading levels only with opts.levels; mermaid stays an escaped code block', () => {
  assert.ok(render('### T', { levels: true }).includes('class="md-h md-h3"'));
  assert.ok(render('### T').includes('class="md-h">'));
  const m = render('```mermaid\ngraph TD; A-->B\n```');
  assert.ok(m.includes('<span class="code-lang">mermaid</span>') && m.includes('A--&gt;B'), m);
});

test('document paths become links: absolute, ~/, relative, bare .md, :line, backticked', () => {
  const link = (p, l) => '<a class="doclink" href="#" data-path="' + p + '"' + (l ? ' data-line="' + l + '"' : '') + '>';
  let out = render('see /Users/r/repo/docs/a.md:42, ~/n/b.txt and docs/c.markdown. Also README.md');
  assert.ok(out.includes(link('/Users/r/repo/docs/a.md', '42') + '/Users/r/repo/docs/a.md:42</a>,'), out);
  assert.ok(out.includes(link('~/n/b.txt') + '~/n/b.txt</a>'), out);
  assert.ok(out.includes(link('docs/c.markdown') + 'docs/c.markdown</a>.'), out);
  assert.ok(out.includes(link('README.md') + 'README.md</a>'), out);
  out = render('open `docs/x.md:7` now');
  assert.ok(out.includes(link('docs/x.md', '7') + '<code>docs/x.md:7</code></a>'), out);
});

test('things that are not document references stay plain text', () => {
  for (const t of ['https://example.com/a.md', 'notes.txt', 'a.mdx', 'x/y.md5', 'foo.md.exe', 'user@host.md', 'v1.2.md-ish', '/var/log/deploy.log']) {
    assert.ok(!render(t).includes('doclink'), t + ' -> ' + render(t));
  }
  assert.ok(!render('```\n/a/b.md\n```').includes('doclink'));
});

test('doc links cannot inject attributes or markup', () => {
  const out = render('/a/"onmouseover="x.md and `/a/<img>.md` and /a/b&c.md');
  assert.ok(!/onmouseover=|<img/.test(out.replace(/&quot;/g, '')) || !out.includes('<img'), out);
  assert.ok(!out.includes('<img'), out);
  assert.ok(!/data-path="[^"]*"[^>]*onmouseover/.test(out), out);
});
