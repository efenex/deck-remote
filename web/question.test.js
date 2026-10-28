'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// Execute the actual renderer with a small DOM; no mirrored rendering logic.
const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
const renderer = source.slice(source.indexOf('  function renderQuestionPanel('), source.indexOf('  // A question menu the agent is parked on'));
function setup(questions) {
  class Element {
    constructor(tag) { this.tag = tag; this.children = []; this.dataset = {}; this.listeners = {}; this.value = ''; this.checked = false; }
    appendChild(child) { this.children.push(child); child.parentElement = this; return child; }
    addEventListener(event, fn) { (this.listeners[event] ||= []).push(fn); }
    fire(event) { for (const fn of this.listeners[event] || []) fn({ preventDefault() {} }); }
    setCustomValidity(message) { this.validationMessage = message; }
    reportValidity() { this.reported = true; }
    all() { return [this, ...this.children.flatMap(c => c.all())]; }
    querySelector(selector) {
      if (selector === '.question-freeform') return this.all().find(e => e.className === 'question-freeform');
      const name = selector.match(/name="([^"]+)"/)[1];
      return this.all().find(e => e.tag === 'input' && e.type === 'radio' && e.name === name && e.checked);
    }
  }
  const mount = new Element('div'), form = new Element('form'), sent = [];
  const info = { id: 'ask1', menuState: 'snapshot1', questions };
  const ctx = {
    document: { createElement: tag => new Element(tag) },
    el: (tag, cls, text) => Object.assign(new Element(tag), { className: cls, textContent: text }),
    $: selector => selector === '#approveMount' ? mount : form,
    esc: s => s, CSS: { escape: s => s }, syncComposerLock() {},
    respondAttention: (...args) => sent.push(JSON.parse(JSON.stringify(args))),
  };
  vm.runInNewContext(renderer + '\nthis.render = renderQuestionPanel;', ctx);
  ctx.render({ id: 's1', tool: 'claude' }, info);
  return { form, sent, card: mount.children[0], inputs: form.all().filter(e => e.tag === 'input'), submit: form.all().find(e => e.tag === 'button') };
}
const question = (multiSelect = true, id = 'q0') => ({ id, header: 'Multi', question: 'Pick several?', multiSelect, options: [{ label: 'One' }, { label: 'Two' }, { label: 'Three' }] });

test('multi-select renders checkboxes and sends several row indices on Submit', () => {
  const env = setup([question()]);
  assert.deepEqual(env.inputs.map(e => e.type), ['checkbox', 'checkbox', 'checkbox', 'checkbox', 'text']);
  assert.ok(env.inputs.slice(0, 4).every(e => !e.required));
  assert.equal(env.submit.textContent, 'Submit answers');
  env.inputs[0].checked = env.inputs[2].checked = true;
  env.form.fire('submit');
  assert.deepEqual(env.sent[0][3], [{ id: 'q0', options: [1, 3], answer: '' }]);
  assert.equal(env.sent[0][1].menuState, 'snapshot1');
  assert.equal(env.submit.disabled, true);
});

test('multi-select supports Other alongside options, and rejects empty selection or Other', () => {
  const env = setup([question()]);
  env.form.fire('submit');
  assert.equal(env.sent.length, 0);
  assert.match(env.inputs[0].validationMessage, /at least one/);
  env.inputs[1].checked = true;
  env.inputs[1].fire('input');
  assert.equal(env.inputs[0].validationMessage, '');
  const other = env.inputs[4];
  other.fire('focus');
  env.form.fire('submit');
  assert.equal(env.sent.length, 0);
  assert.match(other.validationMessage, /other answer/);
  other.value = 'Custom words';
  other.fire('input');
  env.form.fire('submit');
  assert.deepEqual(env.sent[0][3], [{ id: 'q0', options: [2], answer: 'Custom words' }]);
});

test('mixed questions retain single-select radio answers', () => {
  const env = setup([question(), question(false, 'q1')]);
  assert.equal(env.inputs[5].type, 'radio');
  assert.equal(env.inputs[5].required, true);
  env.inputs[0].checked = env.inputs[1].checked = env.inputs[6].checked = true;
  env.form.fire('submit');
  assert.deepEqual(env.sent[0][3], [{ id: 'q0', options: [1, 2], answer: '' }, { id: 'q1', answer: 'Two' }]);
});

test('response transport includes the menu snapshot for server-side stale refusal', () => {
  const transport = source.slice(source.indexOf('  async function respondAttention('), source.indexOf('  async function respondAttention(') + 1800);
  assert.match(transport, /menuState: info\.menuState/);
});

// F1: a refused send re-reads the kept card's menu snapshot, so the next Submit is not refused as
// stale forever (the docked form is kept for the same request id, a half-filled answer included).
function between(start, end) {
  const i = source.indexOf(start), j = source.indexOf(end, i);
  assert.ok(i >= 0 && j > i, start);
  return source.slice(i, j);
}
const attention = between('  async function checkAttention(', '  function approveNote(') +
  between('  const attnSend = {', '  function attnSendTick(') +
  between('  async function respondAttention(', '  // SSE streams');
function refreshEnv(serverState, postError) {
  const env = setup([question()]);
  const card = env.card;
  const foot = { textContent: '', style: {} }, posts = [];
  const mount = { innerHTML: '', querySelector: sel => (sel === '.approve-card' ? card : null), appendChild() {} };
  const ctx = {
    state: { openSheetId: 's1' }, CURRENT_PROFILE: 'default',
    $: sel => ({ '#approveMount': mount, '#approveMount .approve-card': card, '#approveFoot': foot })[sel] || null,
    api: async (method, url, body) => {
      if (method === 'POST') { posts.push(body); if (postError) throw postError; return { responded: true }; }
      return { pending: true, kind: 'question', id: 'ask1', menuState: serverState, questions: [question()] };
    },
    renderDeck() {}, syncComposerLock() {}, toast() {}, renderConvo() {}, ensureConvo: () => [],
    renderQuestionPanel() { throw new Error('kept card must not be re-rendered'); }, renderApprovePanel() {},
    attnLock() {}, attnSendTick() {}, setInterval: () => 1, clearInterval() {}, Date,
  };
  vm.runInNewContext(attention + '\nthis.check = checkAttention; this.respond = respondAttention;', ctx);
  return { ...env, foot, posts, ctx };
}

test('a refused send refreshes the kept card snapshot and the next Submit sends it', async () => {
  const env = refreshEnv('snapshot2', Object.assign(new Error('question menu changed or cleared: stale card'), { status: 409 }));
  assert.equal(env.card.attnInfo.menuState, 'snapshot1');
  await env.ctx.respond({ id: 's1' }, env.card.attnInfo, 'answer', [{ id: 'q0', options: [1], answer: '' }]);
  assert.equal(env.posts[0].menuState, 'snapshot1');
  assert.equal(env.card.attnInfo.menuState, 'snapshot2');
  assert.match(env.foot.textContent, /stale card.*re-read; check your choices and send again/);
  env.inputs[0].checked = true;
  env.form.fire('submit');
  assert.equal(env.sent[0][1].menuState, 'snapshot2'); // the form's own Submit uses the fresh snapshot
});

test('a passive re-check keeps the snapshot; the check button refreshes it', async () => {
  const env = refreshEnv('snapshot2');
  assert.equal(await env.ctx.check({ id: 's1' }, false), false);
  assert.equal(env.card.attnInfo.menuState, 'snapshot1');
  assert.equal(await env.ctx.check({ id: 's1' }, true), true);
  assert.equal(env.card.attnInfo.menuState, 'snapshot2');
});
