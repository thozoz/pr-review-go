'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');

const repoRoot = path.resolve(__dirname, '..', '..', '..');
const mapping = require('./mapping');
const packageArtifacts = require('./package-artifacts');

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
      fs.copyFileSync(
        path.join(realNpmRoot, dir, 'package.json'),
        path.join(npmRoot, dir, 'package.json')
      );
    }

    const binSrcRoot = path.join(tmp, 'bin-src');
    const artifacts = mapping.flatMap((row) => ['pr-review-go', 'pr-review-server'].map((binary) => {
      const binName = binary + (row.platform === 'win32' ? '.exe' : '');
      const dir = path.join(binSrcRoot, `${row.goos}_${row.goarch}`);
      fs.mkdirSync(dir, { recursive: true });
      const binPath = path.join(dir, binName);
      fs.writeFileSync(binPath, `fake binary for ${row.goos}/${row.goarch}\n`);
      return { goos: row.goos, goarch: row.goarch, path: binPath };
    }));

    const result = packageArtifacts({
      artifacts,
      version: '9.9.9',
      npmRoot,
      repoRoot,
    });

    assert(result.version === '9.9.9', 'expected returned version to be 9.9.9');
    assert(
      Array.isArray(result.updatedDirs) && result.updatedDirs.length === 7,
      'expected updatedDirs to contain 7 entries'
    );

    const mainPkg = JSON.parse(
      fs.readFileSync(path.join(npmRoot, 'pr-review-go', 'package.json'), 'utf8')
    );
    assert(mainPkg.version === '9.9.9', 'expected main package version to be stamped to 9.9.9');

    const expectedOptionalDeps = mapping.map((row) => row.pkgName).sort();
    const actualOptionalDeps = Object.keys(mainPkg.optionalDependencies || {}).sort();
    assert(
      JSON.stringify(expectedOptionalDeps) === JSON.stringify(actualOptionalDeps),
      `expected optionalDependencies to be exactly ${JSON.stringify(expectedOptionalDeps)}, got ${JSON.stringify(actualOptionalDeps)}`
    );
    for (const [name, version] of Object.entries(mainPkg.optionalDependencies)) {
      assert(version === '9.9.9', `expected optionalDependencies[${name}] === 9.9.9, got ${version}`);
    }

    for (const row of mapping) {
      const pkgPath = path.join(npmRoot, row.dir, 'package.json');
      const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));
      assert(pkg.version === '9.9.9', `expected ${row.dir} package.json version === 9.9.9, got ${pkg.version}`);

      const binName = row.platform === 'win32' ? 'pr-review-go.exe' : 'pr-review-go';
      const binPath = path.join(npmRoot, row.dir, 'bin', binName);
      assert(fs.existsSync(binPath), `expected ${binPath} to exist`);
      const stat = fs.statSync(binPath);
      assert(stat.size > 0, `expected ${binPath} to be non-empty`);
    }

    // Validation: wrong artifact count throws.
    let threw = false;
    try {
      packageArtifacts({ artifacts: artifacts.slice(0, 5), version: '1.0.0', npmRoot, repoRoot });
    } catch (err) {
      threw = true;
    }
    assert(threw, 'expected packageArtifacts to throw when artifacts.length !== 12');

    // Validation: unmatched goos/goarch throws.
    threw = false;
    try {
      const badArtifacts = artifacts.slice(0, 11).concat([{ goos: 'plan9', goarch: 'amd64', path: artifacts[0].path }]);
      packageArtifacts({ artifacts: badArtifacts, version: '1.0.0', npmRoot, repoRoot });
    } catch (err) {
      threw = true;
    }
    assert(threw, 'expected packageArtifacts to throw when an artifact has no matching mapping row');

    for (const row of mapping) {
 assert(fs.existsSync(path.join(npmRoot, row.dir, 'bin', 'pr-review-server' + (row.platform === 'win32' ? '.exe' : ''))), 'server binary missing');
 }
 for (const bad of [artifacts.slice(0, 11).concat(artifacts[0])]) {
 require('node:assert/strict').throws(() => packageArtifacts({ artifacts: bad, version: '1.0.0', npmRoot, repoRoot }));
 }
 require('node:assert/strict').throws(() => packageArtifacts({ artifacts, version: 'invalid', npmRoot, repoRoot }));
 console.log('package-artifacts.test.js: PASS');
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
