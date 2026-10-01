'use strict';

// Install only local tarballs: no npm account or registry publication is needed.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const assert = require('node:assert/strict');

const repoRoot = path.resolve(__dirname, '../..');
const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'pr-review-npm-smoke-'));
try {
  const version = JSON.parse(fs.readFileSync(path.join(repoRoot, 'npm/pr-review-go/package.json'))).version;
  const archives = ['pr-review-go', `pr-review-go-${process.platform}-${process.arch}`]
    .map((name) => path.join(repoRoot, 'dist/npm', `thozoz-${name}-${version}.tgz`));
  fs.writeFileSync(path.join(tmp, 'package.json'), '{"private":true}');
  execFileSync('npm', ['install', '--omit=optional', '--ignore-scripts', '--no-audit', '--no-fund', ...archives], {
    cwd: tmp, stdio: 'inherit',
  });
  for (const command of ['pr-review-go', 'pr-review-server']) {
    const output = execFileSync(path.join(tmp, 'node_modules/.bin', command), ['-version'], { encoding: 'utf8' });
    assert.ok(output.startsWith(`${command} `), output);
    console.log(output.trim());
  }
} finally {
  fs.rmSync(tmp, { recursive: true, force: true });
}
