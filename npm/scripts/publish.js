'use strict';

const path = require('node:path');

const publishAll = require('./lib/publish-all');

function main() {
  const repoRoot = path.resolve(__dirname, '..', '..');
  const npmRoot = process.env.NPM_ROOT || path.join(repoRoot, 'npm');
  const dryRun = process.env.DRY_RUN === 'true' || process.env.DRY_RUN === '1';

  // Authentication is npm Trusted Publishing (OIDC) — the npm CLI handles it
  // automatically when the workflow has `id-token: write` permission and the
  // trusted publisher is configured on npmjs.com. No token/secret handling here.
  publishAll({ npmRoot, dryRun });
}

try {
  main();
} catch (err) {
  console.error(err.message);
  process.exit(1);
}
