# Using catflap

Day-to-day tasks after the [setup](getting-started.md). Commands marked *homelab* run on the
homelab as your normal user; *client* commands run on the device you connect from. Unfamiliar
words are explained [at the end of Getting started](getting-started.md#words-used-in-these-guides).

- [Devices](#devices)
- [More ports (forwards)](#more-ports-forwards)
- [Alerts on your phone and the Telegram bot](#alerts-on-your-phone-and-the-telegram-bot)
- [Approve a new device from your phone (no invite)](#approve-a-new-device-from-your-phone-no-invite)
- [Several homelabs](#several-homelabs)
- [Tab completion](#tab-completion)
- [Homelab settings file](#homelab-settings-file)
- [Upgrading](#upgrading)
- [What to back up](#what-to-back-up)
- [Moving the relay to another server](#moving-the-relay-to-another-server)
- [Uninstalling](#uninstalling)

## Devices

| Task | Command |
|---|---|
| invite a device (*homelab*) | `catflap invite NAME` (valid 15 min, one use; `--ttl 2h` for longer, up to 90 days) |
| enroll an existing device again, e.g. after reinstalling it (*homelab*) | `catflap invite NAME --replace`: when the device enrolls, its old entry and SSH key are replaced; name and ssh alias stay |
| show open invites (*homelab*) | `catflap invites` |
| revoke an open invite (*homelab*) | `catflap invites --revoke NAME` |
| invite a temporary device, e.g. a friend's laptop for a week (*homelab*) | `catflap invite NAME --expires 7d`: removed automatically 7 days after it enrolls |
| make a device temporary, give it more time, or make it permanent again (*homelab*) | `catflap expire NAME 3d` (from now), `catflap expire NAME never` |
| enroll (*client*) | `catflap enroll cfi1-…` |
| list devices, with when each was last seen (*homelab*) | `catflap list` |
| remove a device (*homelab*) | `catflap remove NAME` |
| check the connection to the homelab (*client*) | `catflap ping ALIAS` |
| forget a homelab (*client*) | `catflap forget ALIAS` (the alias you `ssh` to, by default the homelab's name) |
| what's set up here (*any*) | `catflap status` |

**Is a device in use?** `catflap list` shows when each device was last seen, and for a device
connected right now whether it connects directly or through the relay (`/list` in the Telegram
bot shows the same). A device you haven't seen in months is a good candidate for `catflap remove`.

**How good is the connection?** `catflap ping ALIAS` on a client pings the homelab through the
tunnel and shows the latency and whether the path is direct. It often starts through the relay
and turns direct after a few seconds; if it never does, see
[Troubleshooting](troubleshooting.md#everything-goes-through-the-relay-never-direct).

**Removing a device takes effect immediately:** it's disconnected, the SSH key catflap added for
it is removed from `authorized_keys`, and the relay stops admitting it. No restarts. If that key
was in `authorized_keys` before (you added it yourself), it stays, and `catflap remove` says so.
`catflap forget` on a client only removes the local entry; the homelab still lists the device
until you remove it there.

**Temporary devices** are removed by the homelab when their time is up, exactly as with
`catflap remove`, and you get an alert. `catflap list` shows when (column EXPIRES). Use them for
guests and borrowed machines, so access doesn't stay around because nobody thought of removing
it. `--expires` also works with `catflap approve` and in the bot (`/invite NAME 7d`). A device
re-enrolled with `--replace` keeps its expiry time unless the invite sets a new one.

**Lost or stolen device:** remove it right away, from the homelab or from your phone (`/remove`,
see [the Telegram bot](#the-telegram-invite-bot)). See also [Security](security.md#if-something-is-lost).

**Device names** are up to 63 lowercase letters, digits, `.`, `-` and `_`, starting with a
letter or digit.

**Long-lived invites** (e.g. `--ttl 30d`, to enroll a device in an emergency) belong in a
password manager: whoever has one can enroll a device until it's used.

### Enroll options

On the client, `catflap enroll` takes:

- `--alias NAME`: the `ssh` alias for this homelab (default: the homelab's name). Useful if the
  name is taken in your `~/.ssh/config`, or for a second homelab. `add`, `remove`, `rm`, `list`
  and `ls` can't be aliases (they're `catflap forward` commands).
- `--ssh-key FILE.pub`: the SSH public key to authorize on the homelab, instead of
  `~/.ssh/id_ed25519.pub`.
- `--no-ssh-key`: don't send an SSH key; you manage `authorized_keys` on the homelab yourself.
- `--no-ssh-config`: don't touch `~/.ssh/config`. Then connect with
  `ssh -o ProxyCommand='catflap proxy homelab' USER@homelab`.

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
| open a port to some devices only (*homelab*) | `catflap forward add 1880 --only laptop,phone` |
| open it to every device again (*homelab*) | `catflap forward add 1880` (without `--only`) |
| use another local port (*client*) | `catflap forward homelab 8000:1880` → `http://localhost:8000` |

By default every enrolled device can reach every forwarded port; connections to other ports are
refused. With `--only`, the port is open to the devices you list and no other: their
connections are closed right away, and the homelab logs them. `catflap forward list` shows who
may use each port. `catflap forward add` always sets the whole forward, so run it again with the
new list to change it; a device that loses access is disconnected from that port at once.

`--only` goes by device name. You can name a device that only has an open invite so far, and a
device re-enrolled with `--replace` keeps its access. If you remove a device and later enroll
another one under the same name, the new one gets the same ports.

For a one-off you can also use SSH: `ssh -L 1880:localhost:1880 homelab`.

## Alerts on your phone and the Telegram bot

The homelab can tell you whenever something security-relevant happens:

- a device enrolled (with the command to remove it, in case it wasn't you);
- a device was removed, an invite was created, a temporary device's time ran out;
- **an enrollment failed** (a tampered, expired or reused invite: someone trying something);
- the relay has been unreachable for 10 minutes, and when it's back;
- someone else wrote to your Telegram bot;
- a new catflap version is out (see [Upgrading](#upgrading)).

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
- Use two channels, e.g. Telegram and ntfy; [Security](security.md#good-habits) explains why.
- **ntfy.sh:** anyone who knows a topic name can read it there, so keep the random name catflap
  suggests. **Your own ntfy server:** give catflap its own user with write access to one topic
  only. iPhone push needs extra server settings, see
  [Troubleshooting](troubleshooting.md#notifications).
- **Command channel:** the message comes on stdin, details in `CATFLAP_EVENT`, `CATFLAP_TITLE`,
  `CATFLAP_MESSAGE` and `CATFLAP_PRIORITY`. Example for mail:
  `catflap notify add command --name mail -- sh -c 'mail -s "$CATFLAP_TITLE" you@example.com'`

### The Telegram invite bot

When you add Telegram, catflap shows a link. Open it on your phone and tap **Start**: this links
your chat (and only yours) to the homelab. Answer "yes" to run the bot. Then, in that chat:

| Command | Does |
|---|---|
| `/invite NAME` | one-time invite, valid 15 minutes; tap the code to copy it. `/invite NAME 7d`: a temporary device, removed 7 days after it enrolls |
| `/invites` | open invites |
| `/revoke NAME` | revoke an open invite |
| `/list` | enrolled devices |
| `/remove NAME` | remove a device (asks you to confirm) |
| `/requests on [30m]` / `off` | let new devices ask to join, see [below](#approve-a-new-device-from-your-phone-no-invite) |
| `/status` | homelab status: devices connected now, update available |

So with only your phone at hand you can let a new laptop in, or lock out a stolen one.

- The bot answers **your chat only**. Messages from anyone else are ignored and trigger an alert.
- Once an invite is used, its message changes to "used"; when it expires, the message disappears.
- Commands sent while the homelab was offline aren't executed later; the bot tells you.
- Telegram can read bot chats (they aren't end-to-end encrypted). That's why bot invites are
  short-lived and deleted after use. See [Security](security.md#who-can-see-or-do-what).

## Approve a new device from your phone (no invite)

Instead of copying an invite to the new device, the device asks to join and you approve it with
a tap. It's off until you open a **request window**, so nobody can bother you with requests the
rest of the time:

1. Open the window: `/requests on` in the Telegram bot, or `catflap requests on` on the homelab.
   It closes by itself after 1 hour (`/requests on 30m`, `catflap requests on --for 30m`).
2. On the new device, name the homelab and its relay:

   ```sh
   catflap enroll --request homelab@relay.example.com
   ```

   It shows a 6-digit code, e.g. `731 773`. The device name defaults to its hostname; choose
   another with `--name laptop`.
3. Your Telegram bot shows the request with the same code and **Approve** / **Deny** buttons.
   Compare the codes, then tap **Approve**. Without the bot: `catflap requests` on the homelab
   lists the requests, `catflap approve 731773` (or `catflap deny …`) decides;
   `catflap approve 731773 --expires 1d` makes it a [temporary device](#devices).
4. The device is enrolled as with an invite: ssh config, tunnel check, done. Close the window
   early with `/requests off` or `catflap requests off`.

**Only approve if the codes match.** The code covers both sides' keys: if anything between the
device and your homelab (even the relay) swapped them, the codes differ.

- Approving works only in your Telegram chat or on the homelab, never through ntfy, Pushover or a
  link: reading a notification never lets anyone in. Every request is still reported on all
  your channels with its code.
- A request waits up to 10 minutes. The relay allows a few requests per hour per address and
  three at a time per homelab.
- `enroll --request` takes the same [options](#enroll-options) as an invite.

## Several homelabs

One client can reach several homelabs; each enrollment adds an entry named after the homelab
(`--alias` to choose another). Several homelabs can also share one relay: create a join code
for each with `sudo catflap relay join-code` on the relay. Sessions to several homelabs work at
the same time.

## Tab completion

catflap can complete commands, options, device names and aliases when you press Tab. Load it in
your shell's startup file:

| Shell | Add this |
|---|---|
| bash (`~/.bashrc`) | `source <(catflap completion bash)` |
| zsh (`~/.zshrc`, after `compinit`) | `source <(catflap completion zsh)` |
| fish | run once: `catflap completion fish > ~/.config/fish/completions/catflap.fish` |
| PowerShell (`notepad $PROFILE`) | `catflap completion powershell \| Out-String \| Invoke-Expression` |

Device names, channel names and ports come from the running homelab, aliases from the client, so
`catflap remove <Tab>` on the homelab and `catflap ping <Tab>` on a client offer the right names.

## Homelab settings file

Most settings are changed with commands (`catflap forward`, `catflap notify`). A few only live in
the homelab's settings file: `~/.config/catflap/homelab.json` on Linux and macOS,
`%LOCALAPPDATA%\catflap\homelab.json` on Windows. Edit it, then run `catflap restart`.

| Key | Default | What it does |
|---|---|---|
| `ssh_user` | your user name | the user devices log in as (written into their ssh config when they enroll) |
| `authorized_keys` | `~/.ssh/authorized_keys` (Windows admin accounts: `C:\ProgramData\ssh\administrators_authorized_keys`) | the file catflap adds device keys to; `"off"` stops catflap from managing SSH keys |
| `allow_ssh_keys_from_anywhere` | `false` | `true` lets device keys log in over your LAN too, not only through catflap (affects keys added from then on) |
| `check_updates` | `true` | once a day, ask GitHub for the latest catflap release and send an alert when there's a newer one; `false` turns it off |

Leave `relay` and `name` alone: they're set by `setup homelab` and `homelab move`, and devices
store them when they enroll.

## Upgrading

**You hear about new versions:** once a day the homelab asks GitHub for the latest release. When
there's a newer one, it sends an alert on your channels (once per version), and `catflap status`
and `/status` in the Telegram bot show it. Turn it off with `check_updates` in the
[settings file](#homelab-settings-file). On any machine, `catflap version` checks for a newer
release when you run it (set `CATFLAP_NO_UPDATE_CHECK=1` to skip that).

Run the install command again on each machine. It verifies the download and replaces catflap in
place; running catflap processes keep the old version until they restart.

```sh
curl -fsSL https://github.com/PaddeK/catflap/releases/latest/download/install.sh | sh        # homelab, client (Linux, macOS)
curl -fsSL https://github.com/PaddeK/catflap/releases/latest/download/install.sh | sudo sh   # relay (as root)
irm https://github.com/PaddeK/catflap/releases/latest/download/install.ps1 | iex            # Windows
```

On the relay, run it as root as shown: the relay's service uses `/usr/local/bin/catflap`, and a
run without root would install a second copy in your home directory instead.

Then restart the services:

```sh
catflap restart          # homelab (as your user)
sudo catflap restart     # relay
```

- **Homelab:** open sessions through catflap drop and reconnect in a few seconds. If you run
  `catflap restart` through catflap itself, it warns you that your session drops.
- **Relay:** connected devices reconnect on their own.
- **Clients:** nothing to do; the catflap agent switches to the new version on the next
  connection, after open sessions finish.
- `catflap status` shows the version the homelab is running; `catflap version` the installed one.

Releases and what changed: [releases page](https://github.com/PaddeK/catflap/releases) and the
[changelog](../CHANGELOG.md). Settings, devices and notification channels are kept across
upgrades. To check a download by hand, see [Security](security.md#verifying-a-download).

## What to back up

| Machine | What | If lost |
|---|---|---|
| relay | one file: `sudo catflap relay backup FILE` | with a backup: [restore it](#moving-the-relay-to-another-server) on a new server. Without one: set up the relay again, then the homelab (`catflap setup homelab --join … --force`), then re-enroll every device. |
| homelab | nothing catflap can restore; a backup of the whole machine brings it back with everything else | set up the homelab again with a new join code (`--force` if old files are left), re-enroll every device and add the notification channels again |
| client | nothing | enroll the device again |

Keep relay backups as private as the server itself: they contain its private key. Make a new
backup after you set up another homelab on the relay.

## Moving the relay to another server

### Same hostname

For a new VPS or provider, keeping the relay's name. Homelabs and devices know the relay by its
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

### New hostname or port

The relay's name and port are part of the address every device stores, so each device enrolls
once more. (A relay listens on port 443 unless it was set up with `--listen`.) The homelab keeps
its keys, devices and settings:

1. Set up the new relay ([Getting started](getting-started.md#2-set-up-the-relay)); keep the old
   one running for now. It prints a join code (later ones: `sudo catflap relay join-code`).
2. On the homelab, **directly or over your LAN** (not through catflap: the homelab restarts and
   your session would end before you see the invites):

   ```sh
   catflap homelab move --join cfj1-…@new-relay.example.com
   ```

   It shows what it will do, asks, then registers the homelab with the new relay, removes it from
   the old one, restarts it, and prints one re-enroll invite per device (valid 24 hours;
   `--ttl 3d` for longer). With the Telegram bot set up, the invites also arrive there.
3. On each device, run its `catflap enroll …` line. The device keeps its name and ssh alias; its
   entry now points to the new relay.
4. Once all devices are moved (`catflap invites` shows the unused invites), remove the old relay:
   `sudo catflap uninstall` there.

Devices can't connect between step 2 and their own step 3.

## Uninstalling

```sh
catflap uninstall          # on a client or the homelab, as your user
sudo catflap uninstall     # on the relay
```

It works out what's set up on the machine, shows what it will remove, and asks before doing
anything (`--yes` skips the question, `--keep-binary` keeps the catflap program itself):

- **Client:** forgets its homelabs, including their `~/.ssh/config` blocks, and deletes this
  device's keys. Then remove the device on the homelab too: `catflap remove NAME`.
- **Homelab:** removes the service, removes the homelab from the relay with its devices and
  invites, and removes the SSH keys catflap added. Its devices can't connect anymore. On Windows
  this asks for admin rights (UAC).
- **Relay:** removes the service, its system user, config, keys and certificates. Every homelab
  and device using it stops working. It lists the firewall ports it opened, so you can close
  them yourself.
- **The catflap program** goes too, unless something else on the machine still uses it.

To remove only one role on a machine that has several: `catflap uninstall client|homelab|relay`.

**A homelab that's gone without uninstall** (a dead disk, a reinstalled machine) stays listed on
the relay. Remove it there: `sudo catflap relay status` shows the homelabs,
`sudo catflap relay remove NAME` removes one. (`catflap setup homelab --force` replaces its old
relay record by itself.)
