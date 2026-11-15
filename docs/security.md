# Security

What protects your homelab, who can see what, and what to do when something is lost. The
technical details are in the [design document](design.md#security-model).

## What protects your homelab

A device needs to pass three independent checks ("gates") to log in:

| Gate | Where | Checks |
|---|---|---|
| 1 | relay | Is this device allowed to use the relay at all? |
| 2 | homelab | Is this device enrolled? (otherwise the homelab doesn't even answer) |
| 3 | homelab's SSH server | Does this person have an authorized SSH key? |

**The homelab decides, not the relay.** The relay only admits devices the homelab tells it
about. Traffic is end-to-end encrypted (WireGuard) between the device and the homelab; the relay
passes it on without being able to read it.

**Enrollment uses one-time invites.** The invite's secret never reaches the relay. The homelab
checks a cryptographic signature over the new device's keys, so nobody in between can swap in
their own keys. An invite works once, for 15 minutes by default, for one device name.

**Only forwarded ports are reachable,** port 22 by default. The SSH key catflap adds only works
through catflap (`from="127.0.0.1,::1"`), not over your LAN.

**`catflap remove` closes all three gates at once,** without restarts.

## Who can see or do what

| Who | Can | Can't |
|---|---|---|
| **The relay** (or someone who took it over) | see which devices connect when, from which IP; block the service; let unknown devices talk *to the relay* | read your traffic, read invites, learn your homelab's address, add a device to your homelab |
| **Someone with an unused invite** | enroll one device under that name, until the invite expires or is used. A re-enroll invite (`--replace`, `homelab move`) replaces that device, its SSH key included | anything after that; you see the device in `catflap list` (and get an alert, if set up) |
| **A stolen client device** | connect like you did, until you remove it (and log in only if its SSH key isn't protected by a passphrase or agent) | anything once removed |
| **Telegram** (if you use the bot) | read the bot chat: invites, device names, status | use an invite after it was used or expired |
| **Someone in your Telegram account** | create invites and remove devices through the bot | stay unnoticed if you have a second alert channel |
| **Your ntfy/Pushover service** | read alert texts (device names, IP addresses, request codes) | change anything, approve a request |
| **Anyone who knows `homelab`** | ask to join while your request window is open (a few times per hour) | get in without your approval in Telegram or on the homelab |
| **Someone on your network or the internet** | see encrypted traffic to the relay | read it, or reach the homelab |

## Good habits

- **Look at the alerts.** "new device" means someone enrolled. If that wasn't you, remove the
  device. A "failed enrollment" alert means someone tried a wrong or used invite.
- **Use two alert channels** if you use the Telegram bot, e.g. Telegram and ntfy. Actions taken in
  Telegram are reported on the other channel, so you'd notice someone else in your account.
- **Protect your Telegram account** with Two-Step Verification (Settings → Privacy and
  Security). Whoever controls it controls the bot.
- **Compare the code before you approve a request**, and open the request window only when you
  need it (it closes by itself after an hour).
- **Keep invites short-lived.** Use long ones (`--ttl 30d`, "break-glass") only stored in a
  password manager.
- **Protect your SSH key with a passphrase** (and use an SSH agent so you don't type it all the
  time). A stolen laptop then has the tunnel but no login.
- **Review `catflap list`** now and then and remove devices you no longer use.
- **Keep catflap up to date** (see [Upgrading](using-catflap.md#upgrading)). Releases are signed
  with build attestations; you can check them with `gh attestation verify FILE --repo PaddeK/catflap`.

## If something is lost

| What | Do this |
|---|---|
| **A laptop or other client** | `catflap remove NAME` on the homelab, or `/remove NAME` in the Telegram bot. It's cut off immediately. |
| **Your phone** (with Telegram) | From another device: Telegram → Settings → Devices → end the phone's session. If in doubt, `catflap notify remove telegram` on the homelab and revoke the bot token with @BotFather (`/mybots` → API Token → Revoke). Then set it up again. |
| **An invite you shared by mistake** | `catflap invites --revoke NAME`, or `/revoke NAME` in the bot. If it was used already, remove the device. |
| **A notification token** (bot token, ntfy or Pushover token) | Revoke it at the service, `catflap notify remove NAME`, add the channel again with a new token. |
| **The homelab's state files** (backup leaked, disk stolen) | They contain the homelab's keys. Set the homelab up again with a new join code (`sudo catflap relay join-code` on the relay, then `catflap setup homelab --join … --force`) and re-enroll your devices. |
| **The relay** (VPS compromised) | It can't let anyone into your homelab, but set it up fresh on a clean server anyway, then the homelab (`--force`), then re-enroll your devices. Don't restore a backup made on the compromised server. |
| **A relay backup file** | It contains the relay's private key and the registrations (secrets only as hashes). That's not enough to get into your homelab, or to pose as your relay (that would also take its DNS name and a certificate). Still, delete copies you don't need; if in doubt, set the relay up fresh. |

## Reporting a vulnerability

Please report security problems privately, not in a public issue:
[Security → Report a vulnerability](https://github.com/PaddeK/catflap/security/advisories/new).
See [SECURITY.md](../SECURITY.md).
