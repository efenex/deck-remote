// Run: node --test web/
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const P = require('./pair.js');

const O = 'https://deck.example.ts.net';
const CODE = 'Ab_-'.repeat(10) + 'xyz'; // 43 chars, as genSecret makes

test('a pairing URL for this origin yields its code', () => {
  assert.deepStrictEqual(P.parse(O + '/#pair=' + CODE, O), { code: CODE });
  assert.deepStrictEqual(P.parse('  ' + CODE + '\n', O), { code: CODE });
});

test('another origin, malformed codes and junk are refused with a reason', () => {
  assert.match(P.parse('https://evil.example/#pair=' + CODE, O).error, /for https:\/\/evil\.example/);
  assert.match(P.parse(O + '/#pair=short', O).error, /malformed/);
  assert.match(P.parse('hello', O).error, /not a deck-remote/);
  assert.match(P.parse(O + '/', O).error, /not a deck-remote/);
  assert.match(P.parse('', O).error, /empty/);
});

test('an old ?token= enrolment URL still works', () => {
  assert.deepStrictEqual(P.parse(O + '/?token=raw123', O), { token: 'raw123' });
});

test('fromHash reads only a well-formed #pair=', () => {
  assert.strictEqual(P.fromHash('#pair=' + CODE), CODE);
  assert.strictEqual(P.fromHash('#pair=nope'), '');
  assert.strictEqual(P.fromHash(''), '');
});
