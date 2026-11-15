# Releasing catflap

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

Every file in `checksums.txt` also gets a signed **build provenance attestation**
(Sigstore, keyless, via `actions/attest-build-provenance`). It proves the file was built
by this repository's release workflow from a specific commit.

## Repository settings

The release process relies on these settings (set since 2026-11-15; check them on a fork):

- **Public repository.** GitHub build attestations need one (private repositories need
  GitHub Enterprise Cloud).
- **Rulesets:**
  - *release tags are permanent*: tags `v*` can't be moved or deleted, by anyone.
  - *main: no force push or deletion*: no bypass.
  - *main: CI must pass*: all five `ci` jobs must be green before a pull request can be
    merged. Repository admins may push directly.
- **Immutable releases:** once published, a release's files can't be replaced or deleted.
  goreleaser creates the release as a draft, uploads everything, then publishes it.
- **Actions:** the default workflow token is read-only and can't approve pull requests.
  The workflows ask for what they need themselves (`release.yml`: `contents`, `id-token`,
  `attestations`).
- **Security:** Dependabot alerts and security updates, secret scanning with push
  protection, private vulnerability reporting (see [SECURITY.md](../SECURITY.md)).
  `.github/dependabot.yml` proposes updates for the pinned Actions weekly. Go
  dependencies are bumped by hand: tailcat and `tailscale.com` must move together.

Once tailcat v0.8 is out, switch `go.mod` from the pinned main commit to the tag.

## Cut a release

1. `main` is green in CI (unit tests and `tests/e2e.sh` on amd64 + arm64, release dry run with the install tests on Linux, macOS and Windows). Optionally run `tests/docker.sh` locally.
2. Tag and push:

   ```sh
   git tag -a v1.0.0 -m "catflap v1.0.0"
   git push origin v1.0.0
   ```

3. The release workflow runs the tests, builds all seven targets, publishes the GitHub
   release with the changelog and attests every file. Check the release page and the
   workflow summary afterwards.

The Actions in the workflows are pinned to commit SHAs (version in the comment). Bump
them deliberately, e.g. with Dependabot, not by moving tags.

## Dry run locally

Needs a git checkout and goreleaser v2:

```sh
goreleaser release --snapshot --clean   # builds everything into dist/, publishes nothing
tests/install.sh dist                   # install.sh against those files, incl. tamper cases
tests/docker.sh dist                    # install.sh on several distros + e2e with the amd64, arm64 and 32-bit ARM binaries (Docker)
# Windows: powershell -ExecutionPolicy Bypass -File tests\install.ps1 dist
```

## Verify a download by hand

```sh
sha256sum --check --ignore-missing checksums.txt
gh attestation verify catflap_linux_amd64 --repo PaddeK/catflap
```

The install scripts check the SHA-256 against `checksums.txt` from the same release.
That catches broken and swapped downloads in transit. It can't catch a swapped release,
because `checksums.txt` would be swapped with it. Immutable releases and the attestation
cover that case.

## Upgrade a running setup

Run the install script again. It replaces the binary in place, and running processes
keep the old file until they restart:

- **Clients:** nothing to do. The client agent notices the new build on the next
  connection, lets open sessions finish and hands over to a fresh agent.
- **Homelab:** `catflap restart`. This cuts open sessions through catflap, including the one
  you may be typing in (catflap warns you then); reconnect after a few seconds.
  `catflap status` shows the running version.
- **Relay:** `curl -fsSL …/install.sh | sudo sh`, then `sudo catflap restart`.
  Connected devices reconnect on their own.

`catflap version` shows the version, the commit and the embedded tailcat and Tailscale versions.
