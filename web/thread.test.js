// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { mergeFreshHistory, trimOverlap } = require('./thread.js');

const h = (role, content, ts) => ({ role, content, ts, hist: true });
const live = (role, content, ts) => ({ role, content, ts });

test('reopening a thread does not duplicate history', () => {
  let conv = [];
  const page = [h('me', 'a'), h('reply', 'b'), h('me', 'c')];
  for (let i = 0; i < 3; i++) conv = mergeFreshHistory(page.map((t) => ({ ...t })), conv);
  assert.deepStrictEqual(conv.map((t) => t.content), ['a', 'b', 'c']);
});

test('live rows survive a reload; ones now in the transcript are not doubled', () => {
  const conv = [h('me', 'a'), live('me', 'sent'), live('reply', 'answer'), live('me', 'pending')];
  const fresh = [h('me', 'a'), h('me', 'sent'), h('reply', 'answer')];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.content), ['a', 'sent', 'answer', 'pending']);
});

test('notes and errors are kept', () => {
  const conv = [live('note', 'x'), live('error', 'boom')];
  assert.strictEqual(mergeFreshHistory([h('me', 'a')], conv).length, 3);
});

test('a genuine repeat is kept', () => {
  const conv = [h('me', 'yes', 1000), live('me', 'yes', 5000.5)];
  const fresh = [h('me', 'yes', 1000)];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.content), ['yes', 'yes']);
});

test('older page overlapping the head is trimmed at the boundary only', () => {
  const head = [h('me', 'c'), h('reply', 'd'), h('me', 'e')];
  const older = [h('me', 'ok'), h('reply', 'b'), h('me', 'c'), h('reply', 'd')];
  assert.deepStrictEqual(trimOverlap(older, head).map((t) => t.content), ['ok', 'b']);
  const repeat = [h('me', 'c'), h('reply', 'x')];
  assert.strictEqual(trimOverlap(repeat, head).length, 2);
});

test('a live row is matched to its transcript copy by time', () => {
  const conv = [h('me', 'yes', 1000), live('me', 'yes', 5000.5)];
  const fresh = [h('me', 'yes', 1000), h('reply', 'ok', 4990), h('me', 'yes', 5001)];
  assert.deepStrictEqual(mergeFreshHistory(fresh, conv).map((t) => t.ts), [1000, 4990, 5001]);
});

const { bashBlockHTML, turnKey } = require('./thread.js');

test('bashBlockHTML: command and stdout, escaped', () => {
  const html = bashBlockHTML({ role: 'bash', content: "echo '<b>'", stdout: '<script>alert(1)</script> & "x"', stderr: '' });
  assert.match(html, /<div class="bash-cmd"><span class="bash-prompt">\$ <\/span>echo &#39;&lt;b&gt;&#39;<\/div>/);
  assert.match(html, /<pre class="bash-out">&lt;script&gt;alert\(1\)&lt;\/script&gt; &amp; &quot;x&quot;<\/pre>/);
  assert.ok(!html.includes('<script>') && !html.includes('<b>'));
  assert.ok(!html.includes('bash-err'));
});

test('bashBlockHTML: empty stdout and stderr shows just the command', () => {
  const html = bashBlockHTML({ role: 'bash', content: 'true', stdout: '', stderr: '' });
  assert.match(html, /bash-cmd/);
  assert.ok(!html.includes('<pre'));
});

test('bashBlockHTML: stderr only renders in the error element', () => {
  const html = bashBlockHTML({ role: 'bash', content: 'ls /nope', stdout: '', stderr: 'ls: /nope: No such file' });
  assert.ok(!html.includes('bash-out'));
  assert.match(html, /<pre class="bash-err">ls: \/nope: No such file<\/pre>/);
});

test('bashBlockHTML: multi-line output keeps its lines', () => {
  const html = bashBlockHTML({ role: 'bash', content: 'x', stdout: 'l1\nl2\n\nl4', stderr: 'e1\ne2' });
  assert.ok(html.includes('l1\nl2\n\nl4'));
  assert.ok(html.includes('e1\ne2'));
});

test('bashBlockHTML: nothing at all renders nothing; output-only is kept', () => {
  assert.strictEqual(bashBlockHTML({ role: 'bash', content: '', stdout: '', stderr: ' \n' }), '');
  assert.match(bashBlockHTML({ role: 'bash', content: '', stdout: 'late', stderr: '' }), /bash-out/);
});

test('turnKey: bash rows with the same command but different output differ', () => {
  const a = { role: 'bash', content: 'date', stdout: '1' };
  const b = { role: 'bash', content: 'date', stdout: '2' };
  assert.notStrictEqual(turnKey(a), turnKey(b));
});
