'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const repoRoot = path.resolve(__dirname, '..', '..', '..');
const mapping = require('./mapping');
const publishAll = require('./publish-all');

function assert(cond, message) {
  if (!cond) {
    throw new Error(message);
  }
}

function main() {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'pr-review-go-npm-test-'));
  try {
    const npmRoot = path.join(tmp, 'npm');
    const realNpmRoot = path.join(repoRoot, 'npm');
    const dirs = ['pr-review-go', ...mapping.map((row) => row.dir)];

    for (const dir of dirs) {
      fs.mkdirSync(path.join(npmRoot, dir), { recursive: true });
      const pkgPath = path.join(npmRoot, dir, 'package.json');
      fs.copyFileSync(path.join(realNpmRoot, dir, 'package.json'), pkgPath);
      const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));
      pkg.version = '9.9.9';
      fs.writeFileSync(pkgPath, JSON.stringify(pkg, null, 2) + '\n');
    }

    const logs = [];
    publishAll({ npmRoot, dryRun: true, log: (msg) => logs.push(msg) });

    assert(logs.length === 7, `expected 7 log lines, got ${logs.length}`);
    for (const line of logs) {
      assert(
        /^\[dry-run\] would publish .+@9\.9\.9$/.test(line),
        `expected log line to match dry-run pattern, got: ${line}`
      );
    }

    const platformIndexes = [];
    let mainIndex = -1;
    logs.forEach((line, idx) => {
      if (line.includes('@thozoz/pr-review-go-')) {
        platformIndexes.push(idx);
      } else if (line.includes('@thozoz/pr-review-go@9.9.9')) {
        mainIndex = idx;
      }
    });

    assert(platformIndexes.length === 6, `expected 6 platform log lines, got ${platformIndexes.length}`);
    assert(mainIndex !== -1, 'expected exactly one main package log line');
    assert(
      platformIndexes.every((idx) => idx < mainIndex),
      'expected all platform package publish lines to precede the main package publish line'
    );

    require('node:assert/strict').throws(() => publishAll({ npmRoot, dryRun: true, otp: '123456 & echo injected' }), /NPM_OTP/);
    const firstPath = path.join(npmRoot, mapping[0].dir, 'package.json');
    const first = JSON.parse(fs.readFileSync(firstPath));
    first.version = '8.8.8';
    fs.writeFileSync(firstPath, JSON.stringify(first));
    require('node:assert/strict').throws(() => publishAll({ npmRoot, dryRun: true }), /versions must match/);
    console.log('publish-all.test.js: PASS');
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

try {
  main();
} catch (err) {
  console.error(err);
  process.exit(1);
}
