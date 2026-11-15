# Releasing catflap

For maintainers. Users: to upgrade, see [Upgrading](using-catflap.md#upgrading); to check a
download, see [Verifying a download](security.md#verifying-a-download).

Releases are built by [goreleaser](https://goreleaser.com) in GitHub Actions
(`.github/workflows/release.yml`) when a `v*` tag is pushed. Nobody builds release
binaries on their own machine.

## What a release contains

| File | What |
|---|---|
| `catflap_linux_amd64`, `catflap_linux_arm64` | Linux binaries (static, `CGO_ENABLED=0`) |
| `catflap_linux_arm` | Linux 32-bit ARM, ARMv6 and later (Raspberry Pi with a 32-bit OS, incl. Pi 1 and Zero) |
| `catflap_darwin_arm64`, `catflap_darwin_amd64` | macOS binaries (Apple silicon, Intel) |
| `catflap_windows_amd64.exe`, `catflap_windows_arm64.exe` | Windows binaries |
| `install.sh`, `install.ps1` | install scripts |
| `checksums.txt` | SHA-256 of every file above |

The files are plain binaries, no archives. The checksum covers exactly the file that
gets installed, and the install scripts need no `tar` or `unzip`. File names carry no
version, so `releases/latest/download/<file>` always points at the newest release.

Builds are reproducible: `-trimpath`, file times from the commit, `CGO_ENABLED=0`. Every file
in `checksums.txt` also gets a signed **build provenance attestation** (Sigstore, keyless, via
`actions/attest-build-provenance`). It proves the file was built by this repository's release
workflow from a specific commit.

## Repository settings

The release process relies on these settings; check them on a fork:

- **Public repository.** GitHub build attestations need one (private repositories need
  GitHub Enterprise Cloud).
- **Rulesets:**
  - *release tags are permanent*: tags `v*` can't be moved or deleted, by anyone.
  - *main: no force push or deletion*: no bypass.
  - *main: CI must pass*: all eight `ci` jobs (unit and end-to-end tests, release dry
    runs, the systemd test and the homelab service tests on macOS and Windows) must be
    green before a pull request can be merged. Repository admins may push directly.
- **Immutable releases:** once published, a release's files can't be replaced or deleted.
  goreleaser creates the release as a draft, uploads everything, then publishes it.
- **Actions:** the default workflow token is read-only and can't approve pull requests.
  The workflows ask for what they need themselves (`release.yml`: `contents`, `id-token`,
  `attestations`). Actions are pinned to commit SHAs (version in the comment); bump them
  deliberately, e.g. with Dependabot, not by moving tags. The release build doesn't use the Go
  cache.
- **Security:** Dependabot alerts and security updates, secret scanning with push
  protection, private vulnerability reporting (see [SECURITY.md](../SECURITY.md)).
  `.github/dependabot.yml` proposes updates for the pinned Actions weekly.

## Dependencies

Go dependencies are bumped by hand. tailcat and `tailscale.com` must move together: tailcat
depends on `tailscale.com`, and catflap embeds both. catflap needs tailcat features newer than
v0.7.0, so `go.mod` pins a commit on tailcat's main branch; switch to the tag once tailcat v0.8
is released.

## Cut a release

1. `main` is green in CI (all jobs). Optionally run `tests/docker.sh` locally (see
   [Dry run locally](#dry-run-locally)).
2. Move the "Unreleased" entries in [CHANGELOG.md](../CHANGELOG.md) under the new version, commit
   and push.
3. Tag and push:

   ```sh
   git tag -a v1.0.0 -m "catflap v1.0.0"
   git push origin v1.0.0
   ```

4. The release workflow runs the unit tests, builds all seven targets, publishes the GitHub
   release with install instructions and the commit list, and attests every file. Check the
   release page and the workflow summary afterwards.

## Dry run locally

Needs a git checkout and goreleaser v2:

```sh
goreleaser release --snapshot --clean   # builds everything into dist/, publishes nothing
tests/install.sh dist                   # install.sh against those files, incl. tamper cases
tests/docker.sh dist                    # install.sh on several distros + e2e with the amd64, arm64 and 32-bit ARM binaries (Docker)
# Windows: powershell -ExecutionPolicy Bypass -File tests\install.ps1 dist
```
