'use strict';

const fs = require('node:fs');
const path = require('node:path');

const mapping = require('./mapping');

// expectedArtifactNames is the single computed expectation for the full
// release artifact set: every mapping row crossed with that row's binary list.
// It replaces the old hard-coded mapping.length * 2 so the goreleaser artifact
// set and the npm packager can never drift (14 entries today: 12 + the two
// linux helpers).
function expectedArtifactNames() {
  return mapping.flatMap((row) =>
    row.binaries.map(
      (binary) => `${row.goos}/${row.goarch}/${binary}${row.platform === 'win32' ? '.exe' : ''}`
    )
  );
}

function findRow(artifact) {
  return mapping.find((row) => row.goos === artifact.goos && row.goarch === artifact.goarch);
}

function packageArtifacts({ artifacts, version, npmRoot, repoRoot }) {
  if (typeof version !== 'string' || !/^\d+\.\d+\.\d+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$/.test(version)) {
    throw new Error('packageArtifacts: version is required');
  }
  const expectedNames = expectedArtifactNames();
  if (!Array.isArray(artifacts) || artifacts.length !== expectedNames.length) {
    throw new Error(
      `packageArtifacts: expected exactly ${expectedNames.length} artifacts, got ${Array.isArray(artifacts) ? artifacts.length : typeof artifacts}`
    );
  }

  const matched = artifacts.map((artifact) => {
    const row = findRow(artifact);
    if (!row) {
      throw new Error(
        `packageArtifacts: no mapping row matches artifact goos=${artifact.goos} goarch=${artifact.goarch}`
      );
    }
    // A helper artifact for darwin/windows is a hard error, not a silent extra.
    const baseName = path.basename(artifact.path).replace(/\.exe$/i, '');
    if (!row.binaries.includes(baseName)) {
      throw new Error(
        `packageArtifacts: artifact ${baseName} is not a ${row.goos}/${row.goarch} binary (expected one of: ${row.binaries.join(', ')})`
      );
    }
    return { artifact, row };
  });

  const found = matched.map(({ artifact }) => `${artifact.goos}/${artifact.goarch}/${path.basename(artifact.path)}`);
  const missing = expectedNames.filter((entry) => !found.includes(entry));
  if (missing.length > 0) {
    throw new Error(
      `packageArtifacts: missing artifacts for mapping rows: ${missing.join(', ')} (found: ${found.join(', ')})`
    );
  }

  // Reject missing files or malformed manifests before changing any package.
  for (const { artifact, row } of matched) {
    const src = path.isAbsolute(artifact.path) ? artifact.path : path.resolve(repoRoot, artifact.path);
    if (!fs.statSync(src).isFile()) throw new Error('Artifact is not a file');
    JSON.parse(fs.readFileSync(path.join(npmRoot, row.dir, 'package.json'), 'utf8'));
  }
  JSON.parse(fs.readFileSync(path.join(npmRoot, 'pr-review-go', 'package.json'), 'utf8'));

  for (const { artifact, row } of matched) {
    const destDir = path.join(npmRoot, row.dir, 'bin');
    fs.mkdirSync(destDir, { recursive: true });

    const src = path.isAbsolute(artifact.path) ? artifact.path : path.resolve(repoRoot, artifact.path);
    const dest = path.join(destDir, path.basename(artifact.path));
    fs.copyFileSync(src, dest);
    if (row.platform !== 'win32') {
      fs.chmodSync(dest, 0o755);
    }

    const pkgPath = path.join(npmRoot, row.dir, 'package.json');
    const pkg = JSON.parse(fs.readFileSync(pkgPath, 'utf8'));
    pkg.version = version;
    fs.writeFileSync(pkgPath, JSON.stringify(pkg, null, 2) + '\n');
  }

  const mainPkgPath = path.join(npmRoot, 'pr-review-go', 'package.json');
  const mainPkg = JSON.parse(fs.readFileSync(mainPkgPath, 'utf8'));
  mainPkg.version = version;
  mainPkg.optionalDependencies = {};
  for (const row of mapping) {
    mainPkg.optionalDependencies[row.pkgName] = version;
  }
  fs.writeFileSync(mainPkgPath, JSON.stringify(mainPkg, null, 2) + '\n');

  return {
    version,
    updatedDirs: [...mapping.map((row) => row.dir), 'pr-review-go'],
  };
}

module.exports = packageArtifacts;
module.exports.expectedArtifactNames = expectedArtifactNames;
