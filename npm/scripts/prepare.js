'use strict';

const fs = require('node:fs');
const path = require('node:path');

const packageArtifacts = require('./lib/package-artifacts');

const EXCLUDED_TYPES = new Set(['Archive', 'Checksum', 'UploadableFile']);

function main() {
  const repoRoot = path.resolve(__dirname, '..', '..');

  const rawVersion = process.env.VERSION || '';
  const version = rawVersion.replace(/^v/, '');
  if (!version) {
    throw new Error('VERSION env var is required (e.g. VERSION=1.0.3 or VERSION=v1.0.3)');
  }

  const artifactsJsonPath = process.env.ARTIFACTS_JSON || path.join(repoRoot, 'dist', 'artifacts.json');
  const npmRoot = process.env.NPM_ROOT || path.join(repoRoot, 'npm');

  const allArtifacts = JSON.parse(fs.readFileSync(artifactsJsonPath, 'utf8'));

  const filtered = allArtifacts.filter((a) => {
    if (!a.goos || !a.goarch) return false;
    if (EXCLUDED_TYPES.has(a.type)) return false;
    if (a.extra && a.extra.ID !== undefined) {
      if (!['pr-review-go', 'pr-review-server', 'pr-review-sandbox-helper'].includes(a.extra.ID)) return false;
      // The helper is a Linux-only host artifact. Publishing it for another
      // GOOS would ship a binary that can never enforce anything.
      if (a.extra.ID === 'pr-review-sandbox-helper' && a.goos !== 'linux') {
        throw new Error(
          `prepare.js: pr-review-sandbox-helper artifact must be linux-only, got goos=${a.goos} goarch=${a.goarch}`
        );
      }
    }
    return true;
  });

  const expectedCount = packageArtifacts.expectedArtifactNames().length;
  if (filtered.length !== expectedCount) {
    throw new Error(
      `prepare.js: expected exactly ${expectedCount} matching artifacts, found ${filtered.length}: ${JSON.stringify(
        filtered.map((a) => ({ goos: a.goos, goarch: a.goarch, type: a.type, id: a.extra && a.extra.ID }))
      )}`
    );
  }

  const result = packageArtifacts({
    artifacts: filtered.map((a) => ({ goos: a.goos, goarch: a.goarch, path: a.path })),
    version,
    npmRoot,
    repoRoot,
  });

  console.log(`prepare.js: stamped version ${result.version} into: ${result.updatedDirs.join(', ')}`);
}

try {
  main();
} catch (err) {
  console.error(err.message);
  process.exit(1);
}
