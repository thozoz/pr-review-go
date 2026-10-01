# Releases

`.github/workflows/release.yml` validates every PR and supports manual preview
runs. Preview runs build all twelve binaries, six archives, checksums, and seven
npm tarballs without publishing. They also run Go race tests, vet, npm packaging
tests, a Docker smoke test, and a local npm installation smoke test.

Push a semantic version tag to publish after the validation job succeeds:

```bash
git tag v1.0.0
git push origin v1.0.0
```

Use `v1.0.0-rc.1` for a pre-release. GoReleaser marks the GitHub release as a
pre-release; npm uses the `next` dist-tag for pre-releases and `latest` for stable
versions. Docker images are published to `ghcr.io/thozoz/pr-review-go` using the
explicit version without `v`, for linux/amd64 and linux/arm64. Stable releases
also update `latest`; pre-releases leave `latest` unchanged. Ensure the first
GHCR package is public if it must be pulled without login.

GitHub Release and GHCR publication use the repository's `GITHUB_TOKEN`.
No personal token is needed. Actions must be allowed to write repository contents
and packages. Each job declares only its own required permissions.

## npm: prepared, initially disabled

The npm packages are `@thozoz/pr-review-go` plus six platform packages with
suffixes `linux-x64`, `linux-arm64`, `darwin-x64`, `darwin-arm64`, `win32-x64`, and
`win32-arm64`. Platform packages contain both Go binaries. The wrapper exports
`pr-review-go` and `pr-review-server` commands. No install scripts download or
execute remote code.

No npm login or initial publication has been performed as part of this setup.
The repository has no declared license, so package manifests use `UNLICENSED`;
choose a project license separately if desired.

When ready, perform these one-time steps yourself:

1. Log in to your npm account with access to the `@thozoz` scope.
2. From a release tag checkout, publish the initial package versions:

   ```bash
   VERSION=1.0.0 node npm/scripts/bootstrap-publish.js
   ```

   This command builds all twelve binaries and performs real publication of all
   seven packages. `DRY_RUN=true` builds and prepares packages without publishing.
   If your account requires an OTP, supply it through `NPM_OTP`.
3. Configure a GitHub Actions Trusted Publisher for **each** of the seven npm
   packages: owner `thozoz`, repository `pr-review-go`, workflow `release.yml`.
4. Set the repository Actions variable `NPM_PUBLISH_ENABLED` to `true`.

Future tags then publish platform packages first and the wrapper last using
OIDC, without storing an npm token. Existing package versions are skipped on
reruns. npm requires Node >=22.14.0 and npm >=11.5.1 for trusted publishing;
the workflow uses Node 24 and verifies the npm minimum version.
See the [npm Trusted Publishing documentation](https://docs.npmjs.com/trusted-publishers/).

The release job uploads prepared npm packages as the `npm-release` artifact,
even while npm publication is disabled. It retains them for 14 days. A failed
npm publication does not undo GitHub Release or GHCR publication; inspect the
failed job and rerun it after fixing publisher configuration.

## Local preview

With GoReleaser v2.18.2 and Node available:

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=docker
node npm/scripts/lib/package-artifacts.test.js
node npm/scripts/lib/publish-all.test.js
VERSION=0.0.0-local node npm/scripts/prepare.js
DRY_RUN=true node npm/scripts/publish.js
mkdir -p dist/npm
for package in npm/pr-review-go*/; do
  npm pack "./${package}" --pack-destination dist/npm
done
node npm/scripts/smoke.js
```

Preparation stamps local manifests with the selected version; restore tracked
manifest changes afterward. Generated binaries, archives, and tarballs are ignored.
