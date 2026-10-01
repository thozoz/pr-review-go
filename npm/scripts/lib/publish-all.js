'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { execFileSync, execSync } = require('node:child_process');

const mapping = require('./mapping');

// On Windows the npm CLI is a .cmd shim. execFileSync cannot spawn a bare 'npm'
// there (ENOENT), and since the Node 20 fix for CVE-2024-27980 it refuses to spawn
// a .cmd at all without a shell (EINVAL). The local bootstrap script runs on the
// operator's machine, which may well be Windows, so go through cmd.exe there.
//
// Passing an args array alongside shell:true is deprecated (DEP0190) because the
// args are concatenated rather than escaped, so on Windows we build the single
// command string ourselves. Every argument is repo-controlled — fixed flags plus
// package names read from our own package.json files — and contains no spaces or
// shell metacharacters, so plain concatenation is sound here.
const IS_WINDOWS = process.platform === 'win32';

function runNpm(args, options) {
  if (IS_WINDOWS) {
    return execSync(['npm.cmd', ...args].join(' '), options);
  }
  return execFileSync('npm', args, options);
}

// An OTP can be supplied out-of-band (NPM_OTP=123456) for accounts whose 2FA level
// is "Authorization and writes". npm would normally prompt for it interactively, but
// the bootstrap script is often run from a non-TTY shell where no prompt is possible.
// Accounts set to "Authorization only" need nothing here.
function publishAll({ npmRoot, dryRun = false, log = console.log, otp = process.env.NPM_OTP }) {
  const dirs = [...mapping.map((row) => row.dir), 'pr-review-go'];

  for (const dir of dirs) {
    const pkgPath = path.join(npmRoot, dir, 'package.json');
    const { name, version } = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));

    if (dryRun) {
      log(`[dry-run] would publish ${name}@${version}`);
      continue;
    }

    let alreadyPublished = false;
    try {
      runNpm(['view', `${name}@${version}`, 'version'], { stdio: 'pipe' });
      alreadyPublished = true;
    } catch (err) {
      alreadyPublished = false;
    }

    if (alreadyPublished) {
      log(`${name}@${version} already published, skipping`);
      continue;
    }

    try {
      const publishArgs = ['publish', '--access', 'public', '--tag', version.includes('-') ? 'next' : 'latest'];
      if (otp) publishArgs.push('--otp', otp);

      runNpm(publishArgs, {
        cwd: path.join(npmRoot, dir),
        stdio: 'inherit',
      });
    } catch (err) {
      const output = `${err.message || ''} ${err.stdout || ''} ${err.stderr || ''}`;
      if (output.includes('EPUBLISHCONFLICT') || output.includes('cannot publish over')) {
        log(`${name}@${version} publish conflict (already published by a concurrent run), skipping`);
        continue;
      }
      throw err;
    }
  }
}

module.exports = publishAll;
