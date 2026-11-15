# Using catflap

Day-to-day tasks after the [setup](getting-started.md). Commands marked *homelab* run on the
homelab as your normal user; *client* commands run on the device you connect from.

- [Devices](#devices)
- [More ports (forwards)](#more-ports-forwards)
- [Alerts on your phone and the Telegram bot](#alerts-on-your-phone-and-the-telegram-bot)
- [Several homelabs](#several-homelabs)
- [Upgrading](#upgrading)
- [What to back up](#what-to-back-up)
- [Moving the relay to another server](#moving-the-relay-to-another-server)
- [Uninstalling](#uninstalling)

## Devices

| Task | Command |
|---|---|
| invite a device (*homelab*) | `catflap invite NAME` (valid 15 min, one use; `--ttl 2h` for longer) |
| show open invites (*homelab*) | `catflap invites` |
| revoke an open invite (*homelab*) | `catflap invites --revoke NAME` |
| enroll (*client*) | `catflap enroll cfi1-…` |
| list devices (*homelab*) | `catflap list` |
| remove a device (*homelab*) | `catflap remove NAME` |
| forget a homelab (*client*) | `catflap forget homelab` |
| what's set up here (*any*) | `catflap status` |

**Removing a device takes effect immediately:** it's disconnected, its SSH key is removed from
`authorized_keys`, and the relay stops admitting it. No restarts. `catflap forget` on a client
only removes the local entry; the homelab still lists the device until you remove it there.

**Lost or stolen device:** remove it right away, from the homelab or from your phone (`/remove`,
see [the Telegram bot](#the-telegram-invite-bot)). See also [Security](security.md#if-something-is-lost).

**Device names** are lowercase letters, digits, `.`, `-` and `_`. To enroll a device again
(e.g. after reinstalling it), remove the old entry first, then invite it again.

**Enroll options** (*client*):
- `--alias NAME`: use another SSH alias than the homelab's name (e.g. for two homelabs).
- `--ssh-key FILE.pub`: authorize a specific SSH key.
- `--no-ssh-key`: don't send an SSH key (you manage `authorized_keys` yourself).
- `--no-ssh-config`: don't touch `~/.ssh/config`; use
  `ssh -o ProxyCommand='catflap proxy homelab' user@homelab` instead.

## More ports (forwards)

Only port 22 is reachable by default. To reach another service on the homelab, e.g. Node-RED on
port 1880:

```sh
catflap forward add 1880                 # homelab: live, no restart needed
catflap forward homelab 1880             # client: then open http://localhost:1880
```

The client command keeps running until you press Ctrl-C. It listens on `localhost` only, never
on your network, because anyone reaching it would use your device's access.

| Task | Command |
|---|---|
| expose a port (*homelab*) | `catflap forward add PORT` |
| expose another machine in the homelab's LAN (*homelab*) | `catflap forward add 8080=192.168.1.50:80` |
| list exposed ports (*homelab*) | `catflap forward list` |
| close a port (*homelab*) | `catflap forward remove PORT` (also cuts open connections) |
| use another local port (*client*) | `catflap forward homelab 8000:1880` → `http://localhost:8000` |

Every enrolled device can reach every forwarded port. Connections to other ports are refused.
For a one-off you can also use SSH: `ssh -L 1880:localhost:1880 homelab`.

## Alerts on your phone and the Telegram bot

The homelab can tell you whenever something security-relevant happens:

- a device enrolled (with the command to remove it, in case it wasn't you);
- a device was removed, an invite was created;
- **an enrollment failed** (a tampered, expired or reused invite: someone trying something);
- the relay has been unreachable for 10 minutes, and when it's back;
- someone else wrote to your Telegram bot.

Add one or more channels on the homelab. catflap asks for tokens in a hidden prompt (never pass
them on the command line) and only adds a channel after its test message got through.

| Channel | Command | You need |
|---|---|---|
| Telegram (alerts + bot) | `catflap notify add telegram` | a bot from [@BotFather](https://t.me/BotFather) (`/newbot`) |
| ntfy | `catflap notify add ntfy` | the [ntfy](https://ntfy.sh) app; ntfy.sh or your own server |
| Pushover | `catflap notify add pushover` | a [Pushover](https://pushover.net) user key and app token |
| anything else | `catflap notify add command -- CMD ARGS…` | a program that sends the message |

`catflap notify list` shows the channels, `catflap notify test` sends a test message to each,
`catflap notify remove NAME` removes one.

**Tips:**
- Use two channels, e.g. Telegram and ntfy. An action you take in Telegram is answered there and
  reported on the *other* channels, so you'd notice if someone else took over your Telegram.
- **ntfy.sh:** anyone who knows a topic name can read it there, so keep the random name catflap
  suggests. **Your own ntfy server:** give catflap its own user with write access to one topic
  only, and use the server's local address if it runs on the homelab (e.g.
  `http://127.0.0.1:8085`). On iPhones, subscribe with exactly the server's `base-url`, or push
  won't work.
- **Command channel:** the message comes on stdin, details in `CATFLAP_EVENT`, `CATFLAP_TITLE`,
  `CATFLAP_MESSAGE` and `CATFLAP_PRIORITY`. Example for mail:
  `catflap notify add command --name mail -- sh -c 'mail -s "$CATFLAP_TITLE" you@example.com'`

### The Telegram invite bot

When you add Telegram, catflap shows a link. Open it on your phone and tap **Start**: this links
your chat (and only yours) to the homelab. Answer "yes" to run the bot. Then, in that chat:

| Command | Does |
|---|---|
| `/invite NAME` | one-time invite, valid 15 minutes; tap the code to copy it |
| `/invites` | open invites |
| `/revoke NAME` | revoke an open invite |
| `/list` | enrolled devices |
| `/remove NAME` | remove a device (asks you to confirm) |
| `/status` | homelab status |

So with only your phone at hand you can let a new laptop in, or lock out a stolen one.

- The bot answers **your chat only**. Messages from anyone else are ignored and trigger an alert.
- Once an invite is used, its message changes to "used"; when it expires, the message disappears.
- Commands sent while the homelab was offline aren't executed later; the bot tells you.
- Telegram can read bot chats (they aren't end-to-end encrypted). That's why bot invites are
  short-lived and deleted after use. See [Security](security.md#who-can-see-or-do-what).

## Several homelabs

One client can reach several homelabs; each enrollment adds an entry named after the homelab
(`--alias` to choose another). Several homelabs can also share one relay: create a join code
for each with `sudo catflap relay join-code` on the relay.

Each enrollment has its own device key, so sessions to several homelabs on the same relay work
at the same time. (Enrollments made before v0.3 share one key. If you use two homelabs on the
same relay at the same time and their sessions disturb each other, enroll one of them again.)

## Upgrading

Run the install command again on each machine. It replaces catflap in place, after verifying the
download:

```sh
curl -fsSL https://github.com/PaddeK/catflap/releases/latest/download/install.sh | sh   # Linux, macOS
irm https://github.com/PaddeK/catflap/releases/latest/download/install.ps1 | iex       # Windows
```

Then restart the service on the homelab and the relay:

```sh
catflap restart          # homelab (as your user)
sudo catflap restart     # relay
```

- **Homelab:** open sessions through catflap drop and reconnect in a few seconds. If you run
  `catflap restart` through catflap itself, it warns you that your session drops.
- **Relay:** connected devices reconnect on their own.
- **Clients:** nothing to do; the background agent switches to the new version by itself.
- `catflap status` shows the version the homelab is running; `catflap version` the installed one.

Releases: <https://github.com/PaddeK/catflap/releases>. Settings, devices and notification
channels are kept across upgrades.

## What to back up

| Machine | Files | If lost |
|---|---|---|
| homelab | `~/.config/catflap/homelab.json`, `~/.local/state/catflap/` (state and notification channels; mode 600, contains keys and tokens) | set up the homelab again with a new join code and re-enroll every device |
| relay | one file: `sudo catflap relay backup FILE` | without a backup: set up the relay again, then the homelab (`--force`), then re-enroll every device. With one: [restore it](#moving-the-relay-to-another-server) |
| client | nothing important | enroll the device again |

Keep these backups as private as the machines themselves: they contain keys. Make a new relay
backup after you set up another homelab on it.

## Moving the relay to another server

For a new VPS or provider, with the **same hostname**. Homelabs and devices know the relay by its
name, so nothing changes on them:

1. On the old server: `sudo catflap relay backup relay-backup.json`, and copy the file to the new
   server (e.g. with `scp`). It contains the relay's private key: keep it private, delete it
   afterwards.
2. On the new server: install catflap (see [Getting started](getting-started.md#2-set-up-the-relay)),
   open the same ports, then
   `sudo catflap setup relay --restore relay-backup.json`.
3. Point the DNS records of the relay's name to the new server.
4. Homelabs and devices reconnect on their own once DNS has updated; Let's Encrypt issues a new
   certificate for the new server. Check with `catflap status` on the homelab.
5. On the old server: `sudo catflap uninstall`.

Devices you enroll between the backup and the move aren't in the backup; the homelab sends them
to the new relay by itself when it reconnects. A homelab set up after the backup isn't in it:
make the backup after setting up homelabs.

A **new hostname or port** isn't supported yet: set up the new relay, then each homelab with
`catflap setup homelab --join … --force`, then enroll the devices again.

## Uninstalling

```sh
catflap uninstall          # on a client or the homelab, as your user
sudo catflap uninstall     # on the relay
```

It works out what's set up on the machine, shows what it will remove, and asks before doing
anything (`--yes` skips the question, `--keep-binary` keeps the catflap program itself):

- **Client:** forgets its homelabs (and their `~/.ssh/config` blocks), stops the background
  agent, deletes this device's keys and settings. Then remove the device on the homelab:
  `catflap remove NAME`. An SSH key catflap created for you (`~/.ssh/catflap_ed25519`) is kept.
- **Homelab:** stops and removes the service, removes the homelab from the relay together with
  its devices and open invites, removes the SSH keys catflap added to `authorized_keys`, and
  deletes its config, state and notification channels. Its devices can't connect anymore.
- **Relay:** stops and removes the service and the `catflap` system user, and deletes its
  config, keys and certificates. Every homelab and device using it stops working. Ports it opened
  in the firewall are listed so you can close them; catflap doesn't close them itself, since
  other services may need 80 or 443.
- **The catflap program** goes too, unless something else on the machine still uses it. On
  Windows its folder is also removed from your PATH.

To remove only one role on a machine that has several: `catflap uninstall client|homelab|relay`.

**A homelab that's gone without uninstall** (a dead disk, a reinstalled machine) stays listed on
the relay. Remove it there: `sudo catflap relay status` shows the homelabs,
`sudo catflap relay remove NAME` removes one. (`catflap setup homelab --force` replaces its old
relay record by itself.)
