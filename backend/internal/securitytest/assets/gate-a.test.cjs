const { describe, it } = require('node:test');
const assert = require('node:assert/strict');
const { add } = require('./gate-impl.cjs');
describe('existing-a', () => {
  it('adds positive numbers', () => assert.equal(add(1, 2), 3));
});
