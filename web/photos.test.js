'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const P = require('./photos.js');
test('phone photo validation bounds source files and rejects non-images', () => {
  assert.throws(() => P.validate({ size: 0, type: 'image/jpeg' }), /empty/);
  assert.throws(() => P.validate({ size: 17 * 1024 * 1024, type: 'image/jpeg' }), /16 MiB/);
  assert.throws(() => P.validate({ size: 10, type: 'text/plain' }), /Choose an image/);
  assert.doesNotThrow(() => P.validate({ size: 123, type: 'image/heic' }));
});
test('normalization keeps aspect ratio and never upscales a small photo', () => {
  assert.deepEqual(P.size(4000, 3000), [2560, 1920]);
  assert.deepEqual(P.size(3000, 4000), [1920, 2560]);
  assert.deepEqual(P.size(100, 75), [100, 75]);
});
