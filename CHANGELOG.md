# Changelog

All notable changes, newest first. Downloads and the full commit list for each version are on
the [releases page](https://github.com/PaddeK/catflap/releases). To upgrade, see
[Upgrading](docs/using-catflap.md#upgrading).

## Unreleased

- Temporary devices: `catflap invite NAME --expires 7d` removes the device automatically 7 days
  after it enrolls, with an alert. `catflap expire NAME 3d|never` changes that for any device;
  `catflap approve CODE --expires 1d` and `/invite NAME 7d` in the Telegram bot work too.
  `catflap list` shows when temporary devices expire.
- Ports for some devices only: `catflap forward add 1880 --only laptop,phone`. Other devices'
  connections to that port are closed right away; `catflap forward list` shows who may use each
  port (`any` for ports open to every device, so `any` can't be a device name anymore). The
  setting is stored in the homelab settings file as `forward_only`; an older catflap version
  ignores it, so after a downgrade those ports are open to every device again.
- `catflap status` shows each role as a short block with a table of the forwarded ports
  (homelab) and of the homelabs (client), instead of one long line.
- Fixed: `catflap ping -h` showed the general help instead of the ping options.

## v0.6.0 (2026-11-21)

- `catflap list` and `/list` in the Telegram bot show when each device was last seen, and for
  connected devices whether the connection is direct or goes through the relay. `catflap status`
  and `/status` count the devices connected right now.
- `catflap ping ALIAS` on a client pings the homelab through the tunnel: latency, and whether the
  path is direct or through the relay.
- Update alerts: once a day the homelab asks GitHub for the latest release and sends an alert
  when there's a newer one (once per version); `catflap status` and `/status` show it too. Turn
  it off with `"check_updates": false` in the homelab settings file.
- `catflap version` checks GitHub for a newer release, on any machine (`CATFLAP_NO_UPDATE_CHECK=1`
  skips it).
- Tab completion for bash, zsh, fish and PowerShell (`catflap completion SHELL`), including
  device names, aliases, channels and ports.

## v0.5.1 (2026-11-16)

- `catflap <command> -h` shows the command's options (or the overview, for commands without
  options) and exits cleanly; `catflap help` says so.
- Durations take days: `catflap invite NAME --ttl 7d`, `homelab move --ttl 3d`,
  `requests on --for 1d`, `/requests on 1d`.
- The install scripts tell you to restart a macOS or Windows homelab after an upgrade (before,
  only Linux got the hint), and warn when the relay would be upgraded without root.
- Documentation rewritten for people new to catflap; new CONTRIBUTING.md and this changelog.

## v0.5.0 (2026-11-15)

- **The homelab runs on macOS and Windows**, not only Linux:
  - macOS: a LaunchAgent (`catflap.homelab`); it runs while you're logged in.
  - Windows: a scheduled task (`catflap-homelab`) that starts at boot, without storing your
    password. Setting it up asks once for admin rights. On administrator accounts it runs
    elevated, so it can manage the SSH keys Windows' SSH server reads for administrators; see
    [Security](docs/security.md#windows-homelab).
  - `setup homelab`, `restart`, `homelab move` and `uninstall` work the same on all three.
- `setup homelab` warns if nothing answers on a forwarded port (e.g. no SSH server yet).
- New build for Intel Macs (`catflap_darwin_amd64`).

## v0.4.0 (2026-11-15)

- `catflap homelab move --join CODE`: move a homelab to a relay with a new hostname or port. It
  keeps its keys and devices and prints a re-enroll invite for each device.
- `catflap invite NAME --replace`: enroll an existing device again (e.g. after reinstalling it);
  its old entry and SSH key are replaced.
- **Tap to approve:** a new device can ask to join without an invite
  (`catflap enroll --request HOMELAB@RELAY`). You compare a 6-digit code and approve in the
  Telegram bot or with `catflap approve`, while a request window you opened is open.

## v0.3.0 (2026-11-15)

- `catflap uninstall [relay|homelab|client]` removes catflap from a machine; it shows a plan and
  asks first. A homelab removes itself from the relay.
- `sudo catflap relay remove NAME` forgets a homelab that's gone without uninstall.
- `sudo catflap relay backup FILE` and `setup relay --restore FILE`: move the relay to a new
  server under the same hostname.
- After a relay outage, the homelab sends its devices and open invites again, so a relay
  restored from an older backup catches up by itself.
- **One device key per enrollment.** Before, a client used one key for every homelab, so two
  homelabs on the same relay, used at the same time, disconnected each other. Enrollments made
  before v0.3 keep working with the old key; enroll again if you run into this.
- User guides: getting started, using catflap, troubleshooting, security.

## v0.2.0 (2026-11-15)

- **Alerts** to ntfy, Pushover, Telegram or a program of your choice: device enrolled or
  removed, invite created, failed enrollment, relay unreachable.
- **Telegram invite bot:** `/invite`, `/invites`, `/revoke`, `/list`, `/remove`, `/status`, from
  your own chat only.
- `catflap restart` restarts the relay or homelab service, e.g. after an upgrade.

## v0.1.0 (2026-11-15)

First release: `catflap setup relay`, `setup homelab`, `invite` and `enroll`, live port forwards
(`catflap forward`), the shared client tunnel, install scripts that verify checksums, and signed
build attestations for every release file.
