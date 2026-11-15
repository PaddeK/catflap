# catflap

Reach your homelab from anywhere through **your own relay**, with one binary.

catflap builds on [tailcat](https://github.com/tailscale/tailcat), Tailscale's control-plane-free
WireGuard tunnel. It embeds tailcat and Tailscale's DERP relay server, adds an allowlist at every
hop, and replaces "copy keys around by hand" with one-time invite codes.

> **Status: early releases (v0.x).** It works and is in daily use. Every change runs the end-to-end
> tests on Linux (amd64 and arm64), the homelab service on Linux, macOS and Windows, and the install
> tests on all three. Expect rough edges.
> Not affiliated with Tailscale.

## Documentation

- **[Getting started](docs/getting-started.md)**: relay, homelab and first device, step by step
- **[Using catflap](docs/using-catflap.md)**: devices, more ports, phone alerts and the Telegram bot, upgrades, backups, moving the relay
- **[Troubleshooting](docs/troubleshooting.md)**: logs and common problems
- **[Security](docs/security.md)**: what protects your homelab, and what to do if something is lost
- [Design](docs/design.md) and [releasing](docs/releasing.md), for the technical details

## Install

Linux and macOS (installs to `~/.local/bin`, or `/usr/local/bin` as root):

```sh
curl -fsSL https://github.com/PaddeK/catflap/releases/latest/download/install.sh | sh
```

Windows (PowerShell; installs to `%LOCALAPPDATA%\Programs\catflap` and adds it to your PATH):

```powershell
irm https://github.com/PaddeK/catflap/releases/latest/download/install.ps1 | iex
```

Both scripts verify the download against the release's `checksums.txt` and install
nothing on a mismatch. Run them again to upgrade, then `catflap restart` on a homelab or relay. You can also download a binary from the
[releases page](https://github.com/PaddeK/catflap/releases) and check its signed build
attestation: `gh attestation verify catflap_linux_amd64 --repo PaddeK/catflap`.
See [docs/releasing.md](docs/releasing.md) for details.

## How it works (short)

The full walkthrough is in [Getting started](docs/getting-started.md).

```
catflap setup relay --hostname derp.example.com    # on a small VPS (as root)
  -> prints: catflap setup homelab --join cfj1-…

catflap setup homelab --join cfj1-…                # on the homelab (normal user)
catflap invite laptop                               # on the homelab
  -> prints: catflap enroll cfi1-…

catflap enroll cfi1-…                               # on the laptop
ssh homelab                                         # done
```

More ports, e.g. Node-RED:

```
catflap forward add 1880                            # on the homelab (live, no restart)
catflap forward homelab 1880                        # on the laptop -> http://localhost:1880
```

Alerts on your phone, and a Telegram bot for new devices when you're away:

```
catflap notify add telegram                         # on the homelab: link your chat
  -> in Telegram: /invite laptop                    # one-time invite, valid 15 minutes
catflap notify add ntfy                             # optional second channel for alerts
```

- **One port.** DERP, STUN and the enrollment API run in one process. Clients only need TCP 443
  (plus UDP for direct paths when available).
- **Invites, not side channels.** The invite secret never reaches the relay. The homelab checks a
  MAC over the device's keys and answers with an encrypted reply. A compromised relay can't add
  devices or read your homelab's address. No invite at hand? A new device can ask, and you
  approve it with a tap in Telegram after comparing a 6-digit code.
- **Only what you forward.** Port 22 by default; `catflap forward add` opens more, live. Every
  other port is refused.
- **Know what happens.** Every enrollment, removal and invite, and every failed enrollment,
  shows up on your phone (ntfy, Pushover, Telegram or a command of your choice). The Telegram bot
  answers `/invite`, `/list` and `/remove` from your chat only; nothing is opened on the homelab.
- **Three gates.** Relay admission, the homelab's live tailcat allowlist, and SSH keys. `catflap
  remove` closes all three immediately; no restarts.
- **Platforms.** Relay: Linux. Homelab and clients: Linux amd64/arm64/32-bit ARM (homelab with systemd),
  macOS arm64/amd64, Windows amd64/arm64. Raspberry Pi works as homelab or client (64-bit OS: arm64;
  32-bit OS: ARMv6+ build).

See [Security](docs/security.md) for what this means in practice, and [the design](docs/design.md) for the details.

## Build and test

Requires Go 1.27+.

```sh
go build -o catflap ./cmd/catflap
go test ./...
tests/e2e.sh          # relay + homelab + clients on this machine, incl. attack cases
```

Release builds: see [docs/releasing.md](docs/releasing.md).

## Layout

```
cmd/catflap/          CLI
internal/codes/       invite/join codes, HKDF, MAC, sealed replies
internal/api/         relay HTTP API types + client
internal/relay/       embedded DERP + STUN + TLS + admission + enrollment API
internal/homelab/     embedded tailcat server, relay agent, control socket, Telegram bot
internal/client/      enroll, ssh ProxyCommand, ssh config, client agent
internal/notify/      notification channels (ntfy, Pushover, Telegram, command)
internal/setup/       services (systemd, launchd, Windows Task Scheduler), service user, firewall
internal/osutil/      Windows admin rights and ACLs
tests/e2e.sh          end-to-end test
tests/systemd.sh      setup, restart, move, uninstall under real systemd (container)
tests/service.sh      the homelab service on macOS and Windows (CI runners)
tests/install.sh      install.sh against a goreleaser snapshot (install.ps1: Windows)
tests/docker.sh       install.sh on several distros + e2e on amd64, arm64, ARMv7, ARMv6, in Docker
tests/fakeservices.py fake Telegram/ntfy/Pushover for the e2e test
install.sh            installer for Linux/macOS
install.ps1           installer for Windows
.goreleaser.yaml      release build
.github/workflows/    CI and release (with build attestations)
docs/getting-started.md, using-catflap.md, troubleshooting.md, security.md   user guides
docs/design.md        design and security model
docs/releasing.md     releases, verification, upgrades
```

## License

BSD 3-Clause, see [LICENSE](LICENSE). catflap embeds [tailcat](https://github.com/tailscale/tailcat)
and [Tailscale](https://github.com/tailscale/tailscale) code, which are BSD-3-Clause as well.
