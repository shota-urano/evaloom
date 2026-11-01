const { describe, it } = require('node:test');
const assert = require('node:assert/strict');
const { add } = require('./gate-impl.cjs');
describe('existing-b', () => {
  it('adds negative numbers', () => assert.equal(add(-1, -2), -3));
});
