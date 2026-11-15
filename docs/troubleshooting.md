# Troubleshooting

Start with `catflap status` on the machine in question: it shows what's set up there and whether
the homelab is running. Then check the logs.

## Logs

| Machine | Command |
|---|---|
| relay | `journalctl -u catflap-relay -f` |
| homelab | `journalctl --user -u catflap -f` |
| client | the agent log: `~/.config/catflap/agent.log` (Linux), `~/Library/Application Support/catflap/agent.log` (macOS), `%AppData%\catflap\agent.log` (Windows) |

**More detail:** set `CATFLAP_DEBUG=1` to log the tunnel's internals.
- Client: `CATFLAP_NO_AGENT=1 CATFLAP_DEBUG=1 ssh homelab` runs this one connection without the
  background agent and prints the details in your terminal. (On Windows PowerShell:
  `$env:CATFLAP_NO_AGENT=1; $env:CATFLAP_DEBUG=1; ssh homelab`.)
- Homelab: `systemctl --user edit catflap`, add the two lines below, then `catflap restart`.
  Remove them again afterwards; the logs get long.
  ```
  [Service]
  Environment=CATFLAP_DEBUG=1
  ```

## Setup

**`setup relay`: "another program (an old derper?) is using it"**
Something else listens on port 443 (a web server, an older relay). Stop it, or put the relay on
its own VPS.

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

**"could not enable lingering"**
Run the printed `sudo loginctl enable-linger USER` once. Without it the homelab stops when you
log out.

**`setup relay --restore`: "a relay is already set up here"**
Restoring replaces the relay on this server and every homelab registered with it. If that's what
you want (e.g. a second attempt), add `--force`.

**After moving the relay, nothing connects**
The DNS records may still point to the old server (`dig relay.example.com`), or still be cached;
homelab and devices retry by themselves. Check that the new server has the same ports open, and
`journalctl -u catflap-relay` there.

## Enrolling

**"relay: unknown, used or expired invite"**
The invite was used already, revoked, or older than its lifetime (15 minutes by default). Create
a new one: `catflap invite NAME`.

**"enrollment rejected: invalid invite code (verification failed)"**
The code was mistyped or changed in transit. Copy it again in one piece. (This also triggers a
"failed enrollment" alert if you have notifications set up.)

**"no answer from the homelab within 2 minutes"**
The homelab isn't running or can't reach the relay. On the homelab: `catflap status` and
`journalctl --user -u catflap`. The invite stays valid until it expires; just run `enroll` again.

**"a device named … exists already"**
Remove the old entry first (`catflap remove NAME`), or pick another name.

**"Checking the tunnel... not reachable yet"**
Enrollment worked, but the first connection didn't. Wait a few seconds and try `ssh homelab`. If
it keeps failing, see the next section.

## Connecting

**"cannot reach homelab port 22: … connection refused"**
The homelab answered but nothing is forwarded on that port. Check `catflap forward list` on the
homelab, and that the SSH server runs there (`systemctl status ssh`).

**"cannot reach homelab port 22" after a timeout**
The tunnel can't reach the homelab. Check in this order:
1. Is the homelab running? `catflap status` there.
2. Can it reach the relay? Look for `agent: relay:` errors in its log.
3. Is the device still enrolled? `catflap list` on the homelab. A removed device gets nothing.
4. Is the relay running? `systemctl status catflap-relay` on the VPS.

**"homelab is not running here (systemctl --user status catflap)"**
You ran a homelab command (`invite`, `list`, …) but the service isn't running, or you're on the
wrong machine or user. Start it with `systemctl --user start catflap` and check the log.

**`ssh homelab` asks for a password, or "Permission denied (publickey)"**
The tunnel works, but SSH doesn't accept your key. catflap adds the key you enrolled with to
`authorized_keys` on the homelab (`catflap list` shows which type). Make sure SSH uses it:
`ssh -v homelab` shows the keys it tries. If you enrolled with `--no-ssh-key`, add your key to
the homelab yourself.

**Windows asks for the key passphrase every time**
The Windows SSH agent is off. In an admin PowerShell:
```powershell
Get-Service ssh-agent | Set-Service -StartupType Automatic
Start-Service ssh-agent
ssh-add
```

**The first connection is slow, later ones are fast**
Normal: the first connection starts the tunnel (a second or two, more if no direct path
exists). Following connections reuse it through the background agent.

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
`journalctl --user -u catflap` on the homelab shows `bot:` lines. Commands older than 2 minutes
(sent while the homelab was offline) are skipped on purpose, and the bot says so.

**ntfy on iPhone: messages only appear when the app is opened**
The app must use exactly the address in the server's `base-url`, and the server needs
`upstream-base-url: https://ntfy.sh` for iPhone push. ntfy.sh itself works out of the box.

## Upgrades

**The homelab still runs the old version after an upgrade**
Run `catflap restart` on it. `catflap status` shows the running version.

**Windows: a `catflap.exe.old-…` file next to catflap.exe**
The previous version was still running during the upgrade (the background agent). The file is
removed by the next upgrade, or delete it yourself once no catflap process runs.

## Still stuck?

Open an [issue](https://github.com/PaddeK/catflap/issues) with `catflap version`, what you ran,
the output, and the relevant log lines. Remove invite and join codes, tokens and IP addresses
you'd rather keep private. Security problems go through
[private reporting](https://github.com/PaddeK/catflap/security/advisories/new) instead.
