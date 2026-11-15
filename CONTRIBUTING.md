# Contributing

Bug reports, fixes and ideas are welcome. For anything bigger than a small fix, open an
[issue](https://github.com/PaddeK/catflap/issues) first, so we can agree on the approach.
Security problems go through [private reporting](SECURITY.md), not public issues.

Read [the design](docs/design.md) first: it explains the roles, the security model and why
things are the way they are.

## Build

Needs Go 1.27 or later.

```sh
go build -o catflap ./cmd/catflap
go vet ./...
go test ./...
```

catflap builds with `CGO_ENABLED=0` for every target, so cross-compiling is just
`GOOS=windows GOARCH=amd64 go build ./cmd/catflap`. Release builds are made only by the release
workflow; see [releasing](docs/releasing.md).

## Tests

| Script | What it covers | Runs on |
|---|---|---|
| `go test ./...` | codes and crypto, state files, `authorized_keys` and ssh config editing, notifications, Windows admin-keys detection | any OS |
| `tests/e2e.sh` | relay, homelab and two clients on one machine, in temp directories: enrollment (including tampered, expired and reused invites), tunnels, forwards, removal, the client agent, moving a homelab, uninstall, enrollment requests, notifications and the Telegram bot against fakes | Linux; needs bash, python3, openssl, ssh-keygen |
| `tests/systemd.sh` | `setup`, `restart`, relay backup/restore, `homelab move` and `uninstall` with the real systemd services, in a privileged container | Linux with Docker |
| `tests/service.sh` | the homelab as the real LaunchAgent (macOS) or scheduled task (Windows): setup, a device through the tunnel, restart, move, uninstall | CI runners; it changes the machine (trusts a test certificate, installs the service) |
| `tests/install.sh`, `tests/install.ps1` | the install scripts against a local goreleaser snapshot: install, upgrade over a running catflap, tampered or unlisted downloads refused | Linux/macOS, Windows |
| `tests/docker.sh` | `install.sh` on several distros and CPU architectures, and the end-to-end test with the amd64, arm64 and 32-bit ARM release binaries | Docker with arm64 emulation |

Each script's header comment says how to run it and lists its options. `tests/e2e.sh -k` keeps
the temp directory and logs, and `CATFLAP_BIN=path` tests a given binary instead of building one.
The end-to-end test trusts its self-signed certificate through `SSL_CERT_FILE`; catflap has no
"insecure" switch, and shouldn't get one.

CI (`.github/workflows/ci.yml`) runs all of these except `tests/docker.sh` on every push and pull
request, plus shellcheck on the shell scripts. A pull request can only be merged when all CI jobs
are green.

## Repository layout

```
cmd/catflap/          the CLI (one binary for all roles)
internal/codes/       invite and join codes, key derivation, MAC, sealed replies
internal/api/         relay HTTP API types and client
internal/relay/       embedded DERP relay, STUN, TLS, admission, enrollment API
internal/homelab/     embedded tailcat server, relay connection, control socket, Telegram bot
internal/client/      enroll, ssh ProxyCommand, ssh config, client agent
internal/notify/      notification channels (ntfy, Pushover, Telegram, command)
internal/setup/       services (systemd, launchd, Task Scheduler), relay system user, firewall
internal/osutil/      Windows admin rights and file permissions
internal/fsutil/      atomic file writes, JSON helpers
tests/                the test scripts above, plus fakes for Telegram/ntfy/Pushover
install.sh            installer for Linux and macOS
install.ps1           installer for Windows
.goreleaser.yaml      release build
.github/workflows/    CI and release
docs/                 user guides, design, releasing
```

## Style

- Go: `gofmt`, `go vet`, no new dependencies without a good reason (tailcat and `tailscale.com`
  move together, see [releasing](docs/releasing.md#repository-settings)).
- Shell scripts: POSIX `sh` for `install.sh`, bash for tests; both pass shellcheck.
- User-facing text (CLI output and docs): short sentences, say what to do next. Never print
  secrets (tokens, invite or join codes) where they don't belong.
- Add a line to [CHANGELOG.md](CHANGELOG.md) under "Unreleased" for anything a user would notice.
