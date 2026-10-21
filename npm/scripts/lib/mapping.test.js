'use strict';

const assert = require('node:assert/strict');

const mapping = require('./mapping');

function main() {
  const requiredFields = ['goos', 'goarch', 'platform', 'arch', 'dir', 'pkgName', 'binaries'];
  const expectedByGoos = {
    linux: ['pr-review-go', 'pr-review-server', 'pr-review-sandbox-helper'],
    darwin: ['pr-review-go', 'pr-review-server'],
    windows: ['pr-review-go', 'pr-review-server'],
  };

  assert.equal(mapping.length, 6, 'expected exactly 6 mapping rows');

  const seen = new Set();
  for (const row of mapping) {
    const key = `${row.goos}/${row.goarch}`;
    assert.ok(!seen.has(key), `duplicate mapping row ${key}`);
    seen.add(key);

    for (const field of requiredFields) {
      assert.ok(row[field] !== undefined, `${key}: missing ${field}`);
    }
    assert.ok(Array.isArray(row.binaries), `${key}: binaries must be an array`);
    assert.deepEqual(
      row.binaries,
      expectedByGoos[row.goos],
      `${key}: expected binaries ${JSON.stringify(expectedByGoos[row.goos])}, got ${JSON.stringify(row.binaries)}`
    );
  }

  // The helper is a Linux-only host artifact: it must appear on no other row.
  for (const row of mapping) {
    const hasHelper = row.binaries.includes('pr-review-sandbox-helper');
    if (row.goos === 'linux') {
      assert.ok(hasHelper, `${row.goos}/${row.goarch} must ship the helper`);
    } else {
      assert.ok(!hasHelper, `${row.goos}/${row.goarch} must not ship the helper`);
    }
  }

  assert.deepEqual(
    mapping.binariesFor('linux'),
    ['pr-review-go', 'pr-review-server', 'pr-review-sandbox-helper'],
    'binariesFor(linux) must be the three-name list'
  );
  assert.deepEqual(
    mapping.binariesFor('darwin'),
    ['pr-review-go', 'pr-review-server'],
    'binariesFor(darwin) must be the two-name list'
  );
  assert.deepEqual(
    mapping.binariesFor('windows'),
    ['pr-review-go', 'pr-review-server'],
    'binariesFor(windows) must be the two-name list'
  );
  assert.deepEqual(mapping.binariesFor('plan9'), [], 'an unknown GOOS has no binaries');

  console.log('mapping.test.js: PASS');
}

try {
  main();
} catch (err) {
  console.error(err);
  process.exit(1);
}
