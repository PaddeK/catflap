# catflap

Reach your homelab from anywhere through **your own relay**, with one binary.

You run a small relay on a cheap VPS. Your homelab connects out to it, so it needs no open
ports and no router changes. Your laptop and other devices connect through the relay and reach
the homelab over an end-to-end encrypted WireGuard tunnel, directly when the network allows.
After setup, `ssh homelab` works from any network.

- **No account anywhere.** No coordination service, no third party: you run the relay yourself.
- **Adding a device takes one invite code;** removing it takes effect immediately.
- **Only what you choose is reachable:** SSH by default, more ports when you add them.
- **You hear about everything:** new devices, failed attempts and more, on your phone.

catflap builds on [tailcat](https://github.com/tailscale/tailcat), Tailscale's library for a
WireGuard tunnel between two machines without a Tailscale account or coordination server. It
bundles tailcat with Tailscale's relay server (DERP) and adds invites, alerts and an allowlist on
top. Not affiliated with Tailscale.

> **Status: early releases (v0.x).** In daily use and tested in CI on Linux, macOS and Windows.
> Expect rough edges; see the [changelog](CHANGELOG.md).

## What you need

- A small **Linux VPS** with systemd, root access, a public IPv4 address, a free port 443 and a
  **DNS name** pointing to it, for the relay. 1 vCPU and 512 MB RAM are plenty.
- The **machine at home** you want to reach (the "homelab"): Linux with systemd, macOS or
  Windows, with an SSH server or another service you want to reach. A Raspberry Pi works.
- **Devices** to connect from: Linux, macOS or Windows, with the OpenSSH client (built in on all
  three).

## Quick look

```
catflap setup relay --hostname relay.example.com   # on the VPS (as root)
  -> prints: catflap setup homelab --join cfj1-…@relay.example.com

catflap setup homelab --join cfj1-…@relay.example.com   # on the homelab (your normal user)
catflap invite laptop                              # on the homelab
  -> prints: catflap enroll cfi1-…@relay.example.com

catflap enroll cfi1-…@relay.example.com            # on the laptop
ssh homelab                                        # done
```

More ports, e.g. a web UI on port 1880:

```
catflap forward add 1880                           # on the homelab (live, no restart)
catflap forward homelab 1880                       # on the laptop -> http://localhost:1880
```

Alerts on your phone, and a Telegram bot to invite or remove devices when you're away:

```
catflap notify add telegram                        # on the homelab: link your chat
  -> in Telegram: /invite laptop                   # one-time invite, valid 15 minutes
```

The full walkthrough is in [Getting started](docs/getting-started.md).

## Install

Linux and macOS (installs to `~/.local/bin`, or `/usr/local/bin` as root):

```sh
curl -fsSL https://github.com/PaddeK/catflap/releases/latest/download/install.sh | sh
```

Windows (PowerShell; installs to `%LOCALAPPDATA%\Programs\catflap` and adds it to your PATH):

```powershell
irm https://github.com/PaddeK/catflap/releases/latest/download/install.ps1 | iex
```

Both scripts verify the download against the release's checksums and install nothing on a
mismatch. Every release file also has a signed build attestation; see
[Verifying a download](docs/security.md#verifying-a-download). To upgrade, run the script again:
see [Upgrading](docs/using-catflap.md#upgrading).

| Role | Platforms |
|---|---|
| relay | Linux (amd64, arm64, 32-bit ARM) with systemd |
| homelab | Linux with systemd, macOS, Windows |
| client | Linux, macOS, Windows |

Binaries: Linux amd64, arm64 and 32-bit ARM (ARMv6 and later, so every Raspberry Pi), macOS
Apple silicon and Intel, Windows amd64 and arm64.

## How it keeps your homelab safe

- **Three checks for every connection:** the relay only admits devices your homelab told it
  about, the homelab only answers enrolled devices, and the SSH server still wants your key.
- **The homelab decides, not the relay.** The relay passes encrypted traffic it can't read. Even
  someone who took over the relay can't add a device to your homelab.
- **Invites can't be hijacked in transit.** The invite's secret never reaches the relay, so
  nobody in between can swap in their own device.
- **Nothing is opened on the homelab.** It only makes outgoing connections, also for alerts and
  the Telegram bot.

Details: [Security](docs/security.md) for users, [the design](docs/design.md) for the technical
model.

## Documentation

- **[Getting started](docs/getting-started.md)**: relay, homelab and first device, step by step
- **[Using catflap](docs/using-catflap.md)**: devices, more ports, alerts and the Telegram bot,
  upgrades, backups, moving the relay
- **[Troubleshooting](docs/troubleshooting.md)**: logs and common problems
- **[Security](docs/security.md)**: what protects you, and what to do if something is lost
- [Design](docs/design.md), [releasing](docs/releasing.md) and [contributing](CONTRIBUTING.md),
  for developers

## License

BSD 3-Clause, see [LICENSE](LICENSE). catflap embeds [tailcat](https://github.com/tailscale/tailcat)
and [Tailscale](https://github.com/tailscale/tailscale) code, which are BSD-3-Clause as well.
