'use strict';

// Install only local tarballs: no npm account or registry publication is needed.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync, spawnSync } = require('node:child_process');
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

  // The helper is a Linux-only co-located artifact: on Linux the platform
  // tarball must place it next to the server binary and the helper must be
  // executable and reachable, even without a Landlock-capable kernel (the
  // usage contract returns 75 before any kernel call).
  if (process.platform === 'linux') {
    const helper = path.join(
      tmp, 'node_modules', `@thozoz/pr-review-go-${process.platform}-${process.arch}`, 'bin', 'pr-review-sandbox-helper'
    );
    assert.ok(fs.existsSync(helper), `expected co-located helper at ${helper}`);
    fs.accessSync(helper, fs.constants.X_OK);
    const result = spawnSync(helper, ['landlock-run'], { encoding: 'utf8' });
    assert.equal(result.status, 75, `expected helper landlock-run usage exit 75, got ${result.status} (stderr: ${result.stderr})`);
    console.log(`smoke.js: co-located ${path.basename(helper)} present and reports usage exit 75`);
  } else {
    console.log(`smoke.js: skipping helper assertion on ${process.platform} (the helper is Linux-only)`);
  }
} finally {
  fs.rmSync(tmp, { recursive: true, force: true });
}
