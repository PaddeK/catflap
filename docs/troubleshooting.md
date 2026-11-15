# Troubleshooting

Start with `catflap status` on the machine in question: it shows what's set up there and whether
the homelab is running. Then check the logs.

- [Logs](#logs)
- [Setup](#setup)
- [Moving the relay or homelab](#moving-the-relay-or-homelab)
- [Enrolling](#enrolling)
- [Connecting](#connecting)
- [Notifications](#notifications)
- [Upgrades](#upgrades)

## Logs

| Machine | Command |
|---|---|
| relay | `journalctl -u catflap-relay -f` |
| homelab | Linux: `journalctl --user -u catflap -f`; macOS: `tail -f ~/.local/state/catflap/homelab.log`; Windows: `Get-Content -Wait $env:LOCALAPPDATA\catflap\homelab.log` |
| client | the catflap agent's log: `~/.config/catflap/agent.log` (Linux), `~/Library/Application Support/catflap/agent.log` (macOS), `%AppData%\catflap\agent.log` (Windows) |

**More detail:** the environment variable `CATFLAP_DEBUG=1` makes catflap log the tunnel's
internals.
- Client: `CATFLAP_NO_AGENT=1 CATFLAP_DEBUG=1 ssh homelab` runs this one connection without the
  catflap agent and prints the details in your terminal. (Windows PowerShell:
  `$env:CATFLAP_NO_AGENT=1; $env:CATFLAP_DEBUG=1; ssh homelab`.)
- Homelab, Linux: `systemctl --user edit catflap`, add the two lines below, then
  `catflap restart`.
  ```
  [Service]
  Environment=CATFLAP_DEBUG=1
  ```
- Homelab, macOS: `launchctl setenv CATFLAP_DEBUG 1`, then `catflap restart`. Undo with
  `launchctl unsetenv CATFLAP_DEBUG`.
- Homelab, Windows: `setx CATFLAP_DEBUG 1`, then `catflap restart`. Undo with
  `reg delete HKCU\Environment /v CATFLAP_DEBUG /f`.

The homelab's log then shows `debug` lines. Turn it off again afterwards; the logs get long.

## Setup

**`catflap: command not found` right after installing**
The installer put catflap in a folder that isn't on your PATH yet (it says so: usually
`~/.local/bin`). Open a new terminal, add the folder to your PATH, or run catflap by its full
path, e.g. `~/.local/bin/catflap`.

**`setup relay`: "another program (a web server, another relay?) is using it"**
Something else listens on port 443. Stop it, or put the relay on its own VPS.

**The relay doesn't get a certificate**
Check `journalctl -u catflap-relay`. Let's Encrypt must reach the server under its DNS name:
the `A`/`AAAA` record must point to the VPS (`dig relay.example.com`), and port 443 (better: 80
and 443) must be open, also in the provider's firewall. DNS changes can take a while; the relay
retries on its own.

**`setup homelab`: "the relay refused the join code (wrong, already used or expired)"**
Join codes work once, for 1 hour. Create a new one on the relay: `sudo catflap relay join-code`.

**`setup homelab`: "this machine is already set up as a homelab"**
It is. To start over use `--force`, but note it disconnects all enrolled devices: they need new
invites.

**`sudo catflap relay status` lists a homelab that no longer exists**
It was removed without `catflap uninstall`. Remove it on the relay: `sudo catflap relay remove NAME`
(by id, if two homelabs have the same name).

**Linux: "could not enable lingering"**
Run the printed `sudo loginctl enable-linger USER` once. Without it the homelab stops when you
log out.

**macOS: "launchctl bootstrap: … log in on the Mac itself"**
The LaunchAgent runs in your login session, and over ssh without anyone logged in on the Mac
there is none. Log in on the Mac (or turn on automatic login), then run setup again. After a
reboot the homelab starts once you're logged in; with FileVault, unlocking at boot does that.

**Windows: "no admin rights: the prompt was declined, or can't be shown"**
Registering the boot task needs admin rights once. Run setup in a desktop session and confirm
the Windows prompt, or run it in an elevated PowerShell (also over ssh). `uninstall` asks again.

**"note: nothing answers on 127.0.0.1:22"**
No SSH server runs on the homelab. Linux: install `openssh-server`. macOS: System Settings >
General > Sharing > Remote Login. Windows: Settings > System > Optional features > OpenSSH Server,
then in an admin PowerShell `Start-Service sshd; Set-Service sshd -StartupType Automatic`.

**Windows: ssh asks for a password although the device was enrolled**
For administrator accounts, Windows' SSH server reads keys only from
`C:\ProgramData\ssh\administrators_authorized_keys`. catflap writes there when the homelab runs
with admin rights, which setup arranges for admin accounts; `catflap status` shows the running
homelab. If you changed `sshd_config`, check that its `Match Group administrators` block and
catflap agree, or set `authorized_keys` in `homelab.json` to the file sshd uses.

## Moving the relay or homelab

**`setup relay --restore`: "a relay is already set up here"**
Restoring replaces the relay on this server and every homelab registered with it. If that's what
you want (e.g. a second attempt), add `--force`.

**After moving the relay, nothing connects**
The DNS records may still point to the old server (`dig relay.example.com`), or still be cached;
homelab and devices retry by themselves. Check that the new server has the same ports open, and
`journalctl -u catflap-relay` there.

**`homelab move`: "you are connected through catflap"**
The move restarts the homelab, which would end your session before you see the new invites. Run
it on the homelab itself or over your LAN.

**After `homelab move`, a device can't connect**
It needs its re-enroll invite: run the `catflap enroll …` line printed for it. Lost the line?
`catflap invites --revoke NAME`, then `catflap invite NAME --replace` for a new one.

## Enrolling

**"relay: unknown, used or expired invite"**
The invite was used already, revoked, or older than its lifetime (15 minutes by default). Create
a new one: `catflap invite NAME`.

**"enrollment rejected: invalid invite code (verification failed)"**
The code was mistyped or changed in transit. Copy it again in one piece. (This also triggers a
"failed enrollment" alert if you have notifications set up.)

**"no answer from the homelab within 2 minutes"**
The homelab isn't running or can't reach the relay. On the homelab: `catflap status` and
the homelab's log (see [Logs](#logs)). The invite stays valid until it expires; just run `enroll`
again.

**"a device named … exists already"**
To enroll that device again, use a re-enroll invite: `catflap invite NAME --replace`. Otherwise
pick another name, or remove the old entry first (`catflap remove NAME`).

**"Checking the tunnel... not reachable yet"**
Enrollment worked, but the first connection didn't. Wait a few seconds and try `ssh homelab`. If
it keeps failing, see [Connecting](#connecting).

**`enroll --request`: "no homelab … accepts enrollment requests right now"**
The request window is closed (it closes by itself after an hour), or the homelab name is wrong.
Open it: `/requests on` in the Telegram bot or `catflap requests on` on the homelab. The name is
the homelab's (`catflap status` there), the part after `@` the relay's address.

**`enroll --request`: "the name … is taken"**
Another device already has that name. Ask again with `--name another-name`.

**`enroll --request`: "the reply doesn't come from the homelab the code was made for"**
Something between the device and the homelab tampered with the request. Nothing was saved. Don't
retry blindly: check the relay, and use an invite instead.

**No Telegram message for a request**
Is the bot set up (`catflap notify list`)? Without it, see the request with `catflap requests`
on the homelab and approve with `catflap approve CODE`.

## Connecting

**"cannot reach homelab port 22: … connection refused"**
The homelab answered but nothing is forwarded on that port, or nothing listens behind it. Check
`catflap forward list` on the homelab, and that its SSH server runs (Linux:
`systemctl status ssh`; macOS and Windows: see "nothing answers on 127.0.0.1:22" under
[Setup](#setup)).

**"cannot reach homelab port 22" after a timeout**
The tunnel can't reach the homelab. Check in this order:
1. Is the homelab running? `catflap status` there.
2. Can it reach the relay? Look for `agent: relay:` errors in its log.
3. Is the device still enrolled? `catflap list` on the homelab. A removed device gets nothing.
4. Is the relay running? `systemctl status catflap-relay` on the VPS.

**"the homelab is not running here"**
You ran a homelab command (`invite`, `list`, …) but the homelab isn't running, or you're on the
wrong machine or user. Start it with `catflap restart` and check its log (the message shows
where).

**`ssh homelab` asks for a password, or "Permission denied (publickey)"**
The tunnel works, but SSH doesn't accept your key. catflap adds the key you enrolled with to
`authorized_keys` on the homelab (`catflap list` shows which type). Make sure SSH uses it:
`ssh -v homelab` shows the keys it tries. If you enrolled with `--no-ssh-key`, add your key to
the homelab yourself. Windows homelab with an admin account: see
[above](#setup).

**Windows asks for the key passphrase every time**
The Windows SSH agent is off. In an admin PowerShell:
```powershell
Get-Service ssh-agent | Set-Service -StartupType Automatic
Start-Service ssh-agent
ssh-add
```

**The first connection is slow, later ones are fast**
Normal: the first connection starts the tunnel (a second or two, more if no direct path
exists). Following connections reuse it through the catflap agent.

**Everything goes through the relay, never direct**
Direct connections need UDP. If the relay's STUN port (3478/udp) is closed, or the network blocks
UDP, catflap uses the relay for everything. It still works, just with more latency.

**An open session hangs after the homelab restarted or lost its network**
New connections work again within seconds. A session that was open may hang until ssh notices
(about 2 minutes); press Enter, then `~.` to close it at once. Restarts with `catflap restart`
end open sessions cleanly instead.

**`enroll` warns: "… already has a "Host homelab" entry"**
Your `~/.ssh/config` had a `Host homelab` block before catflap. ssh uses the first match, so
remove the old block or enroll again with `--alias another-name`.

## Notifications

**`notify add`: "test message failed, channel not added"**
The service refused or wasn't reachable. The message after it says why: a wrong token, a wrong
ntfy topic permission (HTTP 403), or a wrong server address.

**Telegram: "another program is polling this bot"**
Only one program can receive a bot's messages. If catflap uses the bot already, remove that
channel first (`catflap notify remove NAME`). If another tool uses it, create a separate bot for
catflap.

**Telegram: "no /start message with the code arrived"**
Open the link catflap printed on the phone and tap **Start**, or send the `/start …` line to the
bot yourself, within 5 minutes. Then run `notify add telegram` again.

**The bot doesn't answer**
The homelab's log (see [Logs](#logs)) shows `bot:` lines. Commands older than 2 minutes
(sent while the homelab was offline) are skipped on purpose, and the bot says so.

**ntfy on iPhone: messages only appear when the app is opened**
With your own ntfy server: the app must use exactly the address in the server's `base-url`, and
the server needs `upstream-base-url: https://ntfy.sh` for iPhone push. ntfy.sh itself works out
of the box.

## Upgrades

**The homelab still runs the old version after an upgrade**
Run `catflap restart` on it. `catflap status` shows the running version.

**The relay still runs the old version after an upgrade**
The install script ran without root and put a second copy in your home directory. Run it again
as root (`curl -fsSL … | sudo sh`), then `sudo catflap restart`. Delete the extra copy
(`~/.local/bin/catflap`).

**Windows: a `catflap.exe.old-…` file next to catflap.exe**
The previous version was still running during the upgrade (the catflap agent). The file is
removed by the next upgrade, or delete it yourself once no catflap process runs.

## Still stuck?

Open an [issue](https://github.com/PaddeK/catflap/issues) with `catflap version`, what you ran,
the output, and the relevant log lines. Remove invite and join codes, tokens and IP addresses
you'd rather keep private. Security problems go through
[private reporting](https://github.com/PaddeK/catflap/security/advisories/new) instead.
