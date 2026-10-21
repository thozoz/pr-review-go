'use strict';

// Single source of truth for the 6 supported GOOS/GOARCH -> npm platform
// package mapping. Every build-time script imports this table rather than
// re-deriving it. (The runtime shim in npm/pr-review-go/bin/pr-review-go.js keeps its
// own small inline copy since it ships standalone inside the published
// package and cannot require a repo-internal path.)
//
// `binaries` is the per-row artifact list. The helper is a Linux-only host
// artifact (the landlock rank exists only on Linux), so it is intentionally
// absent from the darwin/windows rows; consumers must derive their expected
// artifact set from `binariesFor()` rather than a hard-coded count.
const mapping = [
  {
    goos: 'linux',
    goarch: 'amd64',
    platform: 'linux',
    arch: 'x64',
    dir: 'pr-review-go-linux-x64',
    pkgName: '@thozoz/pr-review-go-linux-x64',
    binaries: ['pr-review-go', 'pr-review-server', 'pr-review-sandbox-helper'],
  },
  {
    goos: 'linux',
    goarch: 'arm64',
    platform: 'linux',
    arch: 'arm64',
    dir: 'pr-review-go-linux-arm64',
    pkgName: '@thozoz/pr-review-go-linux-arm64',
    binaries: ['pr-review-go', 'pr-review-server', 'pr-review-sandbox-helper'],
  },
  {
    goos: 'darwin',
    goarch: 'amd64',
    platform: 'darwin',
    arch: 'x64',
    dir: 'pr-review-go-darwin-x64',
    pkgName: '@thozoz/pr-review-go-darwin-x64',
    binaries: ['pr-review-go', 'pr-review-server'],
  },
  {
    goos: 'darwin',
    goarch: 'arm64',
    platform: 'darwin',
    arch: 'arm64',
    dir: 'pr-review-go-darwin-arm64',
    pkgName: '@thozoz/pr-review-go-darwin-arm64',
    binaries: ['pr-review-go', 'pr-review-server'],
  },
  {
    goos: 'windows',
    goarch: 'amd64',
    platform: 'win32',
    arch: 'x64',
    dir: 'pr-review-go-win32-x64',
    pkgName: '@thozoz/pr-review-go-win32-x64',
    binaries: ['pr-review-go', 'pr-review-server'],
  },
  {
    goos: 'windows',
    goarch: 'arm64',
    platform: 'win32',
    arch: 'arm64',
    dir: 'pr-review-go-win32-arm64',
    pkgName: '@thozoz/pr-review-go-win32-arm64',
    binaries: ['pr-review-go', 'pr-review-server'],
  },
];

// binariesFor returns the distinct binary names expected for a GOOS across
// every row that targets it. This is the function every consumer uses so the
// table stays the single source of truth.
function binariesFor(goos) {
  const names = [];
  for (const row of mapping) {
    if (row.goos !== goos) continue;
    for (const binary of row.binaries) {
      if (!names.includes(binary)) names.push(binary);
    }
  }
  return names;
}

module.exports = mapping;
module.exports.binariesFor = binariesFor;
