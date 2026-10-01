'use strict';

// One-time, human-run entry point. This script is written and syntax-checked
// as part of packaging automation, but it is NEVER invoked automatically —
// it performs a real, human-authenticated `npm publish`. See README/SUMMARY
// for the manual steps required to run it.

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');

const mapping = require('./lib/mapping');
const packageArtifacts = require('./lib/package-artifacts');
const publishAll = require('./lib/publish-all');

function main() {
  const repoRoot = path.resolve(__dirname, '..', '..');

  const rawVersion = process.env.VERSION || '';
  const version = rawVersion.replace(/^v/, '');
  if (!version) {
    throw new Error('VERSION env var is required (e.g. VERSION=1.0.3 or VERSION=v1.0.3)');
  }
  const dryRun = process.env.DRY_RUN === 'true' || process.env.DRY_RUN === '1';

  let commit = 'none';
  try {
    commit = execFileSync('git', ['rev-parse', '--short', 'HEAD'], { cwd: repoRoot }).toString().trim();
  } catch (err) {
    commit = 'none';
  }
  const date = new Date().toISOString();

  const ldflags =
    `-s -w -X github.com/thozoz/pr-review-go/pkg/version.Version=${version} ` +
    `-X github.com/thozoz/pr-review-go/pkg/version.Commit=${commit} ` +
    `-X github.com/thozoz/pr-review-go/pkg/version.Date=${date}`;

  const stagingDir = fs.mkdtempSync(path.join(os.tmpdir(), 'pr-review-go-bootstrap-'));

  const artifacts = [];
  for (const row of mapping) {
   for (const binary of ['pr-review-go', 'pr-review-server']) {
    const binName = binary + (row.platform === 'win32' ? '.exe' : '');
    const outPath = path.join(stagingDir, `${row.goos}_${row.goarch}`, binName);
    fs.mkdirSync(path.dirname(outPath), { recursive: true });

    console.log(`bootstrap-publish.js: building ${row.goos}/${row.goarch} -> ${outPath}`);
    execFileSync('go', ['build', '-ldflags', ldflags, '-o', outPath, `./cmd/${binary}`], {
      cwd: repoRoot,
      env: { ...process.env, CGO_ENABLED: '0', GOOS: row.goos, GOARCH: row.goarch },
      stdio: 'inherit',
    });

    artifacts.push({ goos: row.goos, goarch: row.goarch, path: outPath });
   }
  }

  const npmRoot = path.join(repoRoot, 'npm');
  const result = packageArtifacts({ artifacts, version, npmRoot, repoRoot });
  console.log(`bootstrap-publish.js: stamped version ${result.version} into: ${result.updatedDirs.join(', ')}`);

  publishAll({ npmRoot, dryRun });

  const allPkgNames = ['@thozoz/pr-review-go', ...mapping.map((row) => row.pkgName)];
  console.log('');
  console.log('bootstrap-publish.js: done. For EACH of the following packages, configure Trusted');
  console.log('Publishing on npmjs.com -> Settings -> Trusted Publisher -> GitHub Actions:');
  console.log('  org/repo: thozoz/pr-review-go');
  console.log('  workflow filename: release.yml');
  for (const name of allPkgNames) {
    console.log(`  - ${name}`);
  }
}

try {
  main();
} catch (err) {
  console.error(err.message);
  process.exit(1);
}
