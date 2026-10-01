'use strict';

const fs = require('node:fs');
const path = require('node:path');

const mapping = require('./mapping');

function findRow(artifact) {
  return mapping.find((row) => row.goos === artifact.goos && row.goarch === artifact.goarch);
}

function packageArtifacts({ artifacts, version, npmRoot, repoRoot }) {
  if (typeof version !== 'string' || !/^\d+\.\d+\.\d+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$/.test(version)) {
    throw new Error('packageArtifacts: version is required');
  }
  if (!Array.isArray(artifacts) || artifacts.length !== mapping.length * 2) {
    throw new Error(
      `packageArtifacts: expected exactly ${mapping.length * 2} artifacts, got ${Array.isArray(artifacts) ? artifacts.length : typeof artifacts}`
    );
  }

  const matched = artifacts.map((artifact) => {
    const row = findRow(artifact);
    if (!row) {
      throw new Error(
        `packageArtifacts: no mapping row matches artifact goos=${artifact.goos} goarch=${artifact.goarch}`
      );
    }
    return { artifact, row };
  });

  const found = matched.map(({ artifact }) => `${artifact.goos}/${artifact.goarch}/${path.basename(artifact.path)}`);
  const expected = mapping.flatMap((row) => ['pr-review-go', 'pr-review-server'].map((binary) => `${row.goos}/${row.goarch}/${binary}${row.platform === 'win32' ? '.exe' : ''}`));
  const missing = expected.filter((entry) => !found.includes(entry));
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
