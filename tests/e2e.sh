#!/usr/bin/env bash
# Local end-to-end test: relay, homelab and two clients on this machine.
#
# - The relay serves a self-signed cert for 127.0.0.1; every process trusts
#   it via SSL_CERT_FILE (catflap has no "insecure" switch).
# - A fake SSH server (banner + echo) stands in for sshd behind the tunnel.
# - Nothing outside the temp dir is touched (CATFLAP_DIR, CATFLAP_SSH_CONFIG).
#
# Usage: tests/e2e.sh [-k]   (-k keeps the temp dir and logs)
#        CATFLAP_BIN=path tests/e2e.sh   tests that binary instead of building one
set -euo pipefail
cd "$(dirname "$0")/.."
KEEP=${1:-}
export PATH="$HOME/.local/go/bin:$PATH"

T=$(mktemp -d /tmp/cfe2e.XXXX)
PIDS=()
cleanup() {
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
  pkill -f "$T/catflap agent" 2>/dev/null || true   # detached client agents
  wait 2>/dev/null || true
  if [ "$KEEP" = -k ]; then echo "kept $T"; else rm -rf "$T"; fi
}
trap cleanup EXIT

pass=0 fail=0
ok()   { echo "  PASS  $*"; pass=$((pass+1)); }
bad()  { echo "  FAIL  $*"; fail=$((fail+1)); }
check(){ local d=$1; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }

if [ -n "${CATFLAP_BIN:-}" ]; then   # test a prebuilt binary, e.g. a release build
	echo "== using $CATFLAP_BIN"
	cp "$CATFLAP_BIN" "$T/catflap"
else
	echo "== build"
	go build -o "$T/catflap" ./cmd/catflap
fi
CF="$T/catflap"

echo "== certificate + fake sshd"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
  -subj /CN=127.0.0.1 -addext subjectAltName=IP:127.0.0.1 \
  -keyout "$T/key.pem" -out "$T/cert.pem" 2>/dev/null
export SSL_CERT_FILE="$T/cert.pem"

python3 -I - "$T/sshd.port" <<'PY' &
import socket, sys, threading
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 0)); s.listen(16)
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
def serve(c):
    try:
        c.sendall(b"SSH-2.0-catflap-e2e-fake\r\n")
        while True:
            d = c.recv(4096)
            if not d: break
            c.sendall(d)
    finally:
        c.close()
while True:
    c, _ = s.accept(); threading.Thread(target=serve, args=(c,), daemon=True).start()
PY
PIDS+=($!)
for _ in $(seq 50); do [ -s "$T/sshd.port" ] && break; sleep 0.1; done
SSHD_PORT=$(cat "$T/sshd.port")

echo "== relay"
mkdir -p "$T/relay"
cat > "$T/relay/relay.json" <<EOF
{"hostname":"127.0.0.1","listen":"127.0.0.1:18443","http_port":0,"stun_port":13478,
 "cert_mode":"manual","cert_file":"$T/cert.pem","key_file":"$T/key.pem",
 "state_dir":"$T/relay/state","admin_socket":"$T/relay/admin.sock"}
EOF
"$CF" relay run --config "$T/relay/relay.json" > "$T/relay.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do [ -S "$T/relay/admin.sock" ] && break; sleep 0.1; done
JOIN=$("$CF" relay join-code --config "$T/relay/relay.json" | grep -o 'cfj1-[^ ]*')
check "join code issued" test -n "$JOIN"

echo "== homelab"
H="env CATFLAP_DIR=$T/homelab"
$H "$CF" setup homelab --join "$JOIN" --name testlab --no-service --forward "22=127.0.0.1:$SSHD_PORT" > "$T/setup.log" 2>&1
check "setup homelab" grep -q 'registered homelab "testlab"' "$T/setup.log"
check "join code is one-use" bash -c "! env CATFLAP_DIR=$T/homelab2 '$CF' setup homelab --join '$JOIN' --no-service >/dev/null 2>&1"
python3 -I - "$T/homelab/homelab.json" "$T/homelab/authorized_keys" <<'PY'
import json, sys
p = sys.argv[1]; c = json.load(open(p)); c["authorized_keys"] = sys.argv[2]; json.dump(c, open(p, "w"))
PY
$H "$CF" homelab run > "$T/homelab.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do [ -S "$T/homelab/control.sock" ] && break; sleep 0.1; done
check "homelab control socket up" test -S "$T/homelab/control.sock"

client() { local n=$1; shift; env CATFLAP_DIR="$T/$n" CATFLAP_SSH_CONFIG="$T/$n/ssh_config" "$CF" "$@"; }
mkdir -p "$T/c1" "$T/c2" "$T/c3"
for n in c1 c2 c3; do ssh-keygen -q -t ed25519 -N "" -C "$n" -f "$T/$n/id" ; done

echo "== enroll c1 (happy path)"
INV=$($H "$CF" invite laptop | grep -o 'cfi1-[^ ]*')
check "invite created" test -n "$INV"
client c1 enroll "$INV" --ssh-key "$T/c1/id.pub" > "$T/c1.log" 2>&1 || true
check "c1 approved" grep -q 'Approved: this device is "laptop"' "$T/c1.log"
check "c1 tunnel check saw the ssh banner" grep -q 'ok (SSH-2.0-catflap-e2e-fake)' "$T/c1.log"
check "ssh config block written" grep -q 'ProxyCommand ".*catflap" proxy testlab' "$T/c1/ssh_config"
check "authorized_keys line, localhost-only" grep -q '^from="127.0.0.1,::1" ssh-ed25519 .* catflap:laptop$' "$T/homelab/authorized_keys"
OUT=$(printf 'meow\n' | timeout 30 env CATFLAP_DIR="$T/c1" "$CF" proxy testlab 22 2>>"$T/proxy.err" | head -2 | tr -d '\r' | tr '\n' '|')
check "proxy round trip ($OUT)" test "$OUT" = "SSH-2.0-catflap-e2e-fake|meow|"
check "relay lists the device" bash -c "'$CF' relay status --config '$T/relay/relay.json' | grep -q 'device laptop'"

echo "== shared client agent"
check "connections go through one agent" bash -c "grep -c 'agent: listening' '$T/c1/agent.log' | grep -qx 1 && test -S '$T/c1/agent.sock'"
OUT=$(CATFLAP_DIR="$T/c1" timeout 60 python3 -I tests/concurrent.py "$CF" testlab 2>&1)
check "two sessions at once both keep working ($OUT)" test "$OUT" = ok
S=$(date +%s.%N)
OUT=$(printf 'quick\n' | timeout 30 env CATFLAP_DIR="$T/c1" "$CF" proxy testlab 22 2>>"$T/proxy.err" | head -2 | tr -d '\r' | tr '\n' '|')
DT=$(awk -v s="$S" -v e="$(date +%s.%N)" 'BEGIN { printf "%.2f", e - s }')
fast() { [ "$OUT" = 'SSH-2.0-catflap-e2e-fake|quick|' ] && awk -v d="$DT" 'BEGIN { exit !(d < 3) }'; }
check "reconnect right after sessions is fast (${DT}s)" fast
# The fallback runs its own engine with the device's key; while the agent
# holds the same key, the two get in each other's way (the relay keeps one
# connection per key), so stop the agent first.
pkill -f "$T/catflap agent" 2>/dev/null || true
sleep 1
check "CATFLAP_NO_AGENT fallback works" bash -c "printf 'x\n' | CATFLAP_NO_AGENT=1 CATFLAP_DIR='$T/c1' timeout 30 '$CF' proxy testlab 22 2>/dev/null | head -1 | grep -q SSH-"

echo "== activity and ping"
check "list: the device is connected now" bash -c "$H '$CF' list | grep '^laptop ' | grep -qE 'now \((direct|relay)\)'"
check "status counts connected devices" bash -c "$H '$CF' status | grep -qE '^  devices +[0-9]+, 1 connected now'"
client c1 ping testlab --count 3 > "$T/ping.log" 2>&1 || true
check "ping: three replies with latency and path" bash -c "grep -q '3 of 3 pings answered' '$T/ping.log' && [ \$(grep -cE '^reply from testlab: [0-9.]+ ms, (direct|through the relay)' '$T/ping.log') = 3 ]"
check "ping goes through the agent" bash -c "[ \$(grep -c 'agent: listening' '$T/c1/agent.log') = 1 ]"
check "ping: unknown alias" bash -c "env CATFLAP_DIR='$T/c1' '$CF' ping nosuch 2>&1 | grep -q 'unknown homelab \"nosuch\" (known: testlab)'"

echo "== attacks and edge cases"
client c2 enroll "$INV" --ssh-key "$T/c2/id.pub" > "$T/c2a.log" 2>&1 && bad "reused invite accepted" || \
  check "reused invite refused" grep -q 'unknown, used or expired invite' "$T/c2a.log"

INV2=$($H "$CF" invite phone | grep -o 'cfi1-[^ ]*')
# Flip one character of the secret: the relay can't tell, the homelab must.
TAMPERED=$(python3 -I -c 'import sys; s=sys.argv[1]; b,r=s.split("@"); k,i,x=b.split("-"); x=("b" if x[0]!="b" else "c")+x[1:]; print(f"{k}-{i}-{x}@{r}")' "$INV2")
client c2 enroll "$TAMPERED" --ssh-key "$T/c2/id.pub" > "$T/c2b.log" 2>&1 && bad "tampered invite accepted" || \
  check "tampered invite refused by homelab" grep -q 'verification failed' "$T/c2b.log"
client c2 enroll "$INV2" --ssh-key "$T/c2/id.pub" > "$T/c2c.log" 2>&1 || true
check "real invite still works after a bad attempt" grep -q 'Approved: this device is "phone"' "$T/c2c.log"

INV3=$($H "$CF" invite short --ttl 2s | grep -o 'cfi1-[^ ]*')
sleep 3
client c3 enroll "$INV3" --ssh-key "$T/c3/id.pub" > "$T/c3a.log" 2>&1 && bad "expired invite accepted" || \
  check "expired invite refused" grep -qE 'unknown, used or expired invite|expired' "$T/c3a.log"

# A stranger who has the homelab address (e.g. a leaked client.json) but
# another node key: gate 1 (relay) and gate 2 (homelab) must refuse.
python3 -I - "$T/c1/client.json" "$T/c3/client.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
c["key"] = "privkey:" + "11" * 32
for s in c["servers"].values():
    s["key"] = c["key"]
json.dump(c, open(sys.argv[2], "w"))
PY
OUT=$(printf 'x\n' | timeout 25 env CATFLAP_DIR="$T/c3" "$CF" proxy testlab 22 2>>"$T/proxy.err" | head -1 | tr -d '\r' || true)
check "stranger with stolen address gets nothing" bash -c "[[ '$OUT' != SSH-* ]]"
check "relay logged a DENY" grep -q 'admit: DENY' "$T/relay.log"

echo "== remove"
$H "$CF" remove laptop > "$T/rm.log" 2>&1
check "remove reported relay update" grep -q 'relay updated' "$T/rm.log"
check "authorized_keys line gone" bash -c "! grep -q 'catflap:laptop' '$T/homelab/authorized_keys'"
check "relay no longer lists it" bash -c "! '$CF' relay status --config '$T/relay/relay.json' | grep -q 'device laptop'"
OUT=$(printf 'x\n' | timeout 25 env CATFLAP_DIR="$T/c1" "$CF" proxy testlab 22 2>>"$T/proxy.err" | head -1 | tr -d '\r' || true)
check "removed device is cut off" bash -c "[[ '$OUT' != SSH-* ]]"
OUT=$(printf 'still\n' | timeout 30 env CATFLAP_DIR="$T/c2" "$CF" proxy testlab 22 2>>"$T/proxy.err" | head -2 | tr -d '\r' | tr '\n' '|')
check "other device unaffected" test "$OUT" = "SSH-2.0-catflap-e2e-fake|still|"

echo "== homelab restart keeps devices"
kill "${PIDS[-1]}"; wait "${PIDS[-1]}" 2>/dev/null || true; unset 'PIDS[-1]'
$H "$CF" homelab run >> "$T/homelab.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do [ -S "$T/homelab/control.sock" ] && break; sleep 0.1; done
OUT=$(printf 'again\n' | timeout 40 env CATFLAP_DIR="$T/c2" "$CF" proxy testlab 22 2>>"$T/proxy.err" | head -2 | tr -d '\r' | tr '\n' '|')
check "device works after homelab restart" test "$OUT" = "SSH-2.0-catflap-e2e-fake|again|"
check "list shows only phone" bash -c "$H '$CF' list | grep -q '^phone' && ! $H '$CF' list | grep -q '^laptop'"
check "last seen survives the restart" python3 -I -c 'import json, sys
s = json.load(open(sys.argv[1])); sys.exit(not all(d.get("last_seen") for d in s["devices"]))' "$T/homelab/homelab-state.json"

echo "== forwards (live, no restart)"
# firstline PORT [HOLD]: connect to localhost:PORT, print the first line;
# with HOLD, keep the connection open and print EOF when the server cuts it.
firstline() {
  python3 -I - "$@" <<'PY'
import socket, sys, time
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=20)
f = s.makefile("rb")
print(f.readline().decode().strip(), flush=True)
if len(sys.argv) > 2:
    s.settimeout(30)
    try:
        while s.recv(100): pass
        print("EOF", flush=True)
    except Exception as e:
        print("ERR", e, flush=True)
PY
}
OUT=$(printf 'x\n' | timeout 10 env CATFLAP_DIR="$T/c2" "$CF" proxy testlab 9999 2>>"$T/proxy.err" | head -1 | tr -d '\r' || true)
check "port 9999 unreachable before forward add" bash -c "[[ '$OUT' != SSH-* ]]"
$H "$CF" forward add "9999=127.0.0.1:$SSHD_PORT" > "$T/fwd.log" 2>&1
check "forward add" grep -q 'port 9999 -> 127.0.0.1:.* is reachable' "$T/fwd.log"
check "forward list shows 22 and 9999" bash -c "$H '$CF' forward list | grep -q '^22 ' && $H '$CF' forward list | grep -q '^9999 '"
check "homelab.json keeps hand edits and gains the forward" bash -c "grep -q '\"9999\"' '$T/homelab/homelab.json' && grep -q 'authorized_keys' '$T/homelab/homelab.json'"
# Run catflap itself in the background (not the client() function), so $! is catflap.
env CATFLAP_DIR="$T/c2" "$CF" forward testlab 18999:9999 > "$T/c2fwd.log" 2>&1 &
FWD_PID=$!
PIDS+=("$FWD_PID")
for _ in $(seq 100); do grep -q 'Tunnel ok' "$T/c2fwd.log" && break; sleep 0.2; done
check "client forward: tunnel check ok" grep -q 'Tunnel ok' "$T/c2fwd.log"
OUT=$(firstline 18999 | tr -d '\r')
check "client forward: localhost:18999 reaches homelab port 9999 ($OUT)" test "$OUT" = "SSH-2.0-catflap-e2e-fake"
firstline 18999 hold > "$T/hold.out" 2>&1 &
HOLD=$!
for _ in $(seq 50); do [ -s "$T/hold.out" ] && break; sleep 0.2; done
$H "$CF" forward remove 9999 > /dev/null
wait $HOLD || true
check "forward remove cuts the open connection" grep -qx EOF "$T/hold.out"
OUT=$(timeout 15 bash -c "$(declare -f firstline); firstline 18999" 2>/dev/null | tr -d '\r' || true)
check "forward remove: new connections fail" bash -c "[[ '$OUT' != SSH-* ]]"
check "forward remove: port 22 still works" bash -c "printf 'ok\n' | timeout 30 env CATFLAP_DIR='$T/c2' '$CF' proxy testlab 22 2>>'$T/proxy.err' | head -1 | grep -q SSH-"
kill $FWD_PID 2>/dev/null || true; wait $FWD_PID 2>/dev/null || true

$H "$CF" forward add "9999=127.0.0.1:$SSHD_PORT" > /dev/null
kill "${PIDS[-2]}"; wait "${PIDS[-2]}" 2>/dev/null || true   # homelab
$H "$CF" homelab run >> "$T/homelab.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do [ -S "$T/homelab/control.sock" ] && $H "$CF" forward list >/dev/null 2>&1 && break; sleep 0.1; done
OUT=$(printf 'x\n' | timeout 40 env CATFLAP_DIR="$T/c2" "$CF" proxy testlab 9999 2>>"$T/proxy.err" | head -1 | tr -d '\r' || true)
check "forward survives a homelab restart ($OUT)" test "$OUT" = "SSH-2.0-catflap-e2e-fake"
check "client refuses reserved alias" bash -c "env CATFLAP_DIR=$T/c4 '$CF' enroll '$($H "$CF" invite c4 | grep -o 'cfi1-[^ ]*')' --alias list 2>&1 | grep -q reserved"
check "ping -h shows the ping options" bash -c "'$CF' ping -h 2>&1 | grep -q -- '-count'"

echo "== per-device ports"
# (No new enrollments here: the relay allows 10 per address in 10 minutes,
# and this test uses them all. "guest" is only invited, phone is enrolled.)
$H "$CF" invite guest > /dev/null
$H "$CF" forward add "9999=127.0.0.1:$SSHD_PORT" --only guest > "$T/only.log" 2>&1
check "forward add --only (may name a device that is only invited)" grep -q 'reachable through the tunnel now, for guest' "$T/only.log"
check "forward list shows who may use the port" bash -c "$H '$CF' forward list | grep '^9999 ' | grep -q guest && $H '$CF' forward list | grep '^22 ' | grep -q any && $H '$CF' invite any 2>&1 | grep -q \"can't be a device name\""
check "homelab.json gains forward_only" grep -q '"forward_only"' "$T/homelab/homelab.json"
check "status shows the restriction" bash -c "$H '$CF' status | grep -qE '^  9999 +127.0.0.1:[0-9]+ +guest$'"
check "--only refuses unknown devices" bash -c "$H '$CF' forward add 9999=127.0.0.1:$SSHD_PORT --only nosuch 2>&1 | grep -q 'no device or open invite named \"nosuch\"'"
OUT=$(printf 'x\n' | timeout 10 env CATFLAP_DIR="$T/c2" "$CF" proxy testlab 9999 2>>"$T/proxy.err" | head -1 | tr -d '\r' || true)
check "a device not on the list is refused" bash -c "[[ '$OUT' != SSH-* ]]"
check "the homelab logs the refusal" grep -q 'phone -> port 9999 refused' "$T/homelab.log"
check "port 22 stays open to every device" bash -c "printf 'ok\n' | timeout 30 env CATFLAP_DIR='$T/c2' '$CF' proxy testlab 22 2>>'$T/proxy.err' | head -1 | grep -q SSH-"
$H "$CF" forward add "9999=127.0.0.1:$SSHD_PORT" --only phone,guest > /dev/null
OUT=$(printf 'x\n' | timeout 30 env CATFLAP_DIR="$T/c2" "$CF" proxy testlab 9999 2>>"$T/proxy.err" | head -1 | tr -d '\r' || true)
check "a device on the list gets in ($OUT)" test "$OUT" = "SSH-2.0-catflap-e2e-fake"
# Taking phone off the list cuts its open connection.
env CATFLAP_DIR="$T/c2" "$CF" forward testlab 18998:9999 > "$T/c2fwd2.log" 2>&1 &
TFWD=$!
PIDS+=("$TFWD")
for _ in $(seq 100); do grep -q 'Tunnel ok' "$T/c2fwd2.log" && break; sleep 0.2; done
firstline 18998 hold > "$T/thold.out" 2>&1 &
HOLD=$!
for _ in $(seq 50); do [ -s "$T/thold.out" ] && break; sleep 0.2; done
$H "$CF" forward add "9999=127.0.0.1:$SSHD_PORT" --only guest > /dev/null
wait $HOLD || true
check "taking access away cuts the open connection" bash -c "head -1 '$T/thold.out' | grep -q SSH- && grep -qx EOF '$T/thold.out'"
kill $TFWD 2>/dev/null || true; wait $TFWD 2>/dev/null || true
$H "$CF" forward add "9999=127.0.0.1:$SSHD_PORT" > "$T/only2.log" 2>&1
check "forward add without --only opens it to all again" bash -c "grep -q 'for every device' '$T/only2.log' && ! grep -q forward_only '$T/homelab/homelab.json'"
$H "$CF" invites --revoke guest > /dev/null

echo "== notifications and the Telegram bot (fake services)"
python3 -I tests/fakeservices.py serve "$T/fake.port" > "$T/fake.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do [ -s "$T/fake.port" ] && break; sleep 0.1; done
FAKE="http://127.0.0.1:$(cat "$T/fake.port")"
fake() { python3 -I tests/fakeservices.py "$FAKE" "$@"; }

# Prompts are answered from stdin: server, topic, access token (none).
printf '%s\n%s\n\n' "$FAKE/ntfy" catflap-e2e | $H "$CF" notify add ntfy > "$T/ntfy.log" 2>&1
check "notify add ntfy: test message arrives" fake has ntfy "notifications are on"
check "a channel that fails its test is not added" bash -c "printf 'http://127.0.0.1:1\nx\n\n' | $H '$CF' notify add ntfy --name broken 2>&1 | grep -q 'test message failed' && ! $H '$CF' notify list | grep -q '^broken'"

# Telegram: token from stdin, then the bot waits for "/start <code>".
printf '123:TESTTOKEN\ny\n' | $H "$CF" notify add telegram --api-url "$FAKE/tg" > "$T/tg.log" 2>&1 &
TGADD=$!
for _ in $(seq 100); do grep -q 'start=' "$T/tg.log" && break; sleep 0.1; done
CODE=$(grep -o 'start=[a-z0-9]*' "$T/tg.log" | head -1 | cut -d= -f2)
fake update 999 "/start wrongcode"      # someone else racing to link first
fake update 4242 "/start $CODE"
wait $TGADD || true
check "telegram links the chat that sent the code" grep -q 'linked to @u4242 (id 4242)' "$T/tg.log"
check "token never shown" bash -c "! grep -q TESTTOKEN '$T/tg.log' && ! $H '$CF' notify list | grep -q TESTTOKEN"
check "notify list shows the bot" bash -c "$H '$CF' notify list | grep -q '@testbot, chat 4242, answers /invite'"
check "notify.json is private" bash -c "[ \$(stat -c %a '$T/homelab/notify.json') = 600 ]"

fake update 4242 "/invite tgdev"
check "bot replies with an invite" fake has tg "catflap enroll cfi1-"
TGINV=$(fake invite)
mkdir -p "$T/c5"; ssh-keygen -q -t ed25519 -N "" -C c5 -f "$T/c5/id"
client c5 enroll "$TGINV" --ssh-key "$T/c5/id.pub" > "$T/c5.log" 2>&1 || true
check "device enrolls with the bot's invite" grep -q 'Approved: this device is "tgdev"' "$T/c5.log"
check "bot replaces the code once it's used" fake has tg "was used"
check "alert on the other channel: new device" fake has ntfy "new device: tgdev"
check "alert: invite created via the bot" fake has ntfy "created via the Telegram bot"
check "no echo of the bot's own invite to Telegram" bash -c "! python3 -I tests/fakeservices.py '$FAKE' has tg 'invite created for tgdev' --now"

fake update 999 "/invite evil"
check "alert: a stranger wrote to the bot" fake has ntfy "someone else wrote to your Telegram bot"
check "the stranger got no invite" bash -c "! $H '$CF' invites | grep -q evil"

INV6=$($H "$CF" invite c6 | grep -o 'cfi1-[^ ]*')
TAMPERED6=$(python3 -I -c 'import sys; s=sys.argv[1]; b,r=s.split("@"); k,i,x=b.split("-"); x=("b" if x[0]!="b" else "c")+x[1:]; print(f"{k}-{i}-{x}@{r}")' "$INV6")
client c3 enroll "$TAMPERED6" --ssh-key "$T/c3/id.pub" > /dev/null 2>&1 || true
check "alert: failed enrollment" fake has ntfy "failed enrollment"

fake update 4242 "/remove tgdev"
check "bot asks before removing" fake has tg "loses access immediately"
fake button 4242 "$(fake callback rm:)"
check "bot edits the question into the result" fake has tg "Removed <b>tgdev</b>: disconnected"
check "device removed from the phone" bash -c "! $H '$CF' list | grep -q '^tgdev'"
check "alert: device removed via the bot" fake has ntfy "tgdev was removed via the Telegram bot"
check "notify test reaches every channel" $H "$CF" notify test

echo "== enrollment requests (tap to approve)"
RQ="testlab@127.0.0.1:18443"
waitcode() { for _ in $(seq 150); do c=$(grep -o 'Code:  [0-9]\{3\} [0-9]\{3\}' "$1" 2>/dev/null | tr -dc 0-9 || true); [ -n "$c" ] && { echo "$c"; return; }; sleep 0.1; done; }
mkdir -p "$T/c8" "$T/c9" "$T/c10" "$T/c11"
check "requests are refused while the window is closed" bash -c "env CATFLAP_DIR='$T/c8' CATFLAP_SSH_CONFIG='$T/c8/ssh_config' '$CF' enroll --request $RQ --no-ssh-key 2>&1 | grep -q 'accepts enrollment requests right now'"
$H "$CF" requests on --for 10m > "$T/rqon.log" 2>&1
check "requests on: open, with the command for the new device" bash -c "grep -q 'open until' '$T/rqon.log' && grep -q 'catflap enroll --request testlab@' '$T/rqon.log'"
check "alert: the window was opened" fake has ntfy "enrollment requests open"

# Approve on the homelab with the code.
env CATFLAP_DIR="$T/c8" CATFLAP_SSH_CONFIG="$T/c8/ssh_config" "$CF" enroll --request "$RQ" --name req1 --no-ssh-key > "$T/c8.log" 2>&1 &
C8=$!
CODE=$(waitcode "$T/c8.log")
check "the new device shows a 6-digit code" test -n "$CODE"
check "catflap requests lists it with the same code" bash -c "$H '$CF' requests | grep '^req1' | tr -d ' ' | grep -q '$CODE'"
check "Telegram asks with the same code and buttons" bash -c "python3 -I tests/fakeservices.py '$FAKE' has tg 'Approve req1' && python3 -I tests/fakeservices.py '$FAKE' has tg '${CODE:0:3} ${CODE:3:3}'"
check "the other channels get it with the code" fake has ntfy "catflap approve $CODE"
check "a wrong code approves nothing" bash -c "$H '$CF' approve 000000 2>&1 | grep -q 'no pending request'"
$H "$CF" approve "$CODE" > "$T/approve.log" 2>&1
wait $C8 || true
check "catflap approve: the device is enrolled" bash -c "grep -q 'approved \"req1\"' '$T/approve.log' && grep -q 'Approved: this device is \"req1\"' '$T/c8.log'"
OUT=$(printf 'tap\n' | timeout 30 env CATFLAP_DIR="$T/c8" "$CF" proxy testlab 22 2>>"$T/proxy.err" | head -2 | tr -d '\r' | tr '\n' '|')
check "the approved device has a working tunnel ($OUT)" test "$OUT" = "SSH-2.0-catflap-e2e-fake|tap|"
check "the Telegram message turns into the result" fake has tg "Approved: <b>req1</b> is enrolled"
check "alert: new device approved on the homelab" fake has ntfy "req1 (hostname"

# Deny and approve with the Telegram buttons.
env CATFLAP_DIR="$T/c9" CATFLAP_SSH_CONFIG="$T/c9/ssh_config" "$CF" enroll --request "$RQ" --name req2 --no-ssh-key > "$T/c9.log" 2>&1 &
C9=$!
waitcode "$T/c9.log" > /dev/null
for _ in $(seq 50); do python3 -I tests/fakeservices.py "$FAKE" has tg 'Approve req2' --now && break; sleep 0.1; done
fake button 4242 "$(fake callback rd:)"
wait $C9 || true
check "Deny in Telegram: the device is told" grep -q 'request denied' "$T/c9.log"
env CATFLAP_DIR="$T/c10" CATFLAP_SSH_CONFIG="$T/c10/ssh_config" "$CF" enroll --request "$RQ" --name req3 --no-ssh-key > "$T/c10.log" 2>&1 &
C10=$!
waitcode "$T/c10.log" > /dev/null
for _ in $(seq 50); do python3 -I tests/fakeservices.py "$FAKE" has tg 'Approve req3' --now && break; sleep 0.1; done
fake button 4242 "$(fake callback ra:)"
wait $C10 || true
check "Approve in Telegram: the device is enrolled" grep -q 'Approved: this device is "req3"' "$T/c10.log"
check "a taken name is refused" bash -c "env CATFLAP_DIR='$T/c11' CATFLAP_SSH_CONFIG='$T/c11/ssh_config' timeout 60 '$CF' enroll --request $RQ --name req1 --no-ssh-key 2>&1 | grep -q 'is taken'"

# A temporary device: removed a minute after approval (checked further down).
mkdir -p "$T/ct"; ssh-keygen -q -t ed25519 -N "" -C ct -f "$T/ct/id"
env CATFLAP_DIR="$T/ct" CATFLAP_SSH_CONFIG="$T/ct/ssh_config" "$CF" enroll --request "$RQ" --name tmpdev --ssh-key "$T/ct/id.pub" > "$T/ct.log" 2>&1 &
CT=$!
TCODE=$(waitcode "$T/ct.log")
$H "$CF" approve "$TCODE" --expires 1m > "$T/tapprove.log" 2>&1
wait $CT || true
check "approve --expires: a temporary device" bash -c "grep -q 'removed automatically in 1 minute' '$T/tapprove.log' && grep -q 'Approved: this device is \"tmpdev\"' '$T/ct.log'"
check "alert: it is a temporary device" fake has ntfy "It is a temporary device"
check "list: EXPIRES column with tmpdev's time, - for the others" bash -c "$H '$CF' list > '$T/tlist.out' && grep -q EXPIRES '$T/tlist.out' && grep '^tmpdev ' '$T/tlist.out' | grep -qE '[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}' && grep '^phone ' '$T/tlist.out' | grep -q ' - '"
check "invite --expires: the device will be temporary" bash -c "$H '$CF' invite later --expires 7d | grep -q 'removed 7 days after it enrolls' && $H '$CF' invites --revoke later > /dev/null"
check "expire: a device can be made temporary" bash -c "$H '$CF' expire phone 30d | grep -q 'is removed automatically at'"
check "expire never: and permanent again" bash -c "$H '$CF' expire phone never | grep -q 'no longer expires' && $H '$CF' list | grep '^phone ' | grep -q ' - '"
check "expire: unknown device" bash -c "$H '$CF' expire nosuch 1d 2>&1 | grep -q 'no device named \"nosuch\"'"

fake update 4242 "/requests off"
check "/requests off in Telegram closes the window" fake has tg "Enrollment requests are closed"
check "requests are refused again" bash -c "env CATFLAP_DIR='$T/c11' CATFLAP_SSH_CONFIG='$T/c11/ssh_config' '$CF' enroll --request $RQ --name late --no-ssh-key 2>&1 | grep -q 'accepts enrollment requests right now'"
fake update 4242 "/requests on 5m"
check "/requests on 5m in Telegram opens it" fake has tg "Enrollment requests are open until"
$H "$CF" requests off > /dev/null
$H "$CF" notify remove telegram > /dev/null
check "notify remove" bash -c "! $H '$CF' notify list | grep -q telegram"

echo "== one client, two homelabs on the same relay"
J3=$("$CF" relay join-code --config "$T/relay/relay.json" | grep -o 'cfj1-[^ ]*')
H3="env CATFLAP_DIR=$T/homelab3"
$H3 "$CF" setup homelab --join "$J3" --name lab3 --no-service --forward "22=127.0.0.1:$SSHD_PORT" > /dev/null 2>&1
$H3 "$CF" homelab run > "$T/homelab3.log" 2>&1 &
H3PID=$!
PIDS+=("$H3PID")
for _ in $(seq 100); do [ -S "$T/homelab3/control.sock" ] && break; sleep 0.1; done
mkdir -p "$T/c6"
client c6 enroll "$($H "$CF" invite multi | grep -o 'cfi1-[^ ]*')" --no-ssh-key > "$T/c6a.log" 2>&1 || true
client c6 enroll "$($H3 "$CF" invite multi | grep -o 'cfi1-[^ ]*')" --no-ssh-key > "$T/c6b.log" 2>&1 || true
check "client enrolls in both homelabs" bash -c "grep -q Approved '$T/c6a.log' && grep -q Approved '$T/c6b.log'"
check "each enrollment has its own key" python3 -I -c 'import json, sys
c = json.load(open(sys.argv[1]))
k = [s.get("key") for s in c["servers"].values()]
sys.exit(0 if len(k) == 2 and all(k) and len(set(k)) == 2 and "key" not in c else 1)' "$T/c6/client.json"
OUT=$(CATFLAP_DIR="$T/c6" timeout 60 python3 -I tests/concurrent.py "$CF" testlab lab3 2>&1)
check "sessions to both homelabs at once keep working ($OUT)" test "$OUT" = ok
# A client enrolled before v0.3 has one shared key at the top of client.json.
mkdir -p "$T/c7"
python3 -I - "$T/c6/client.json" "$T/c7/client.json" <<'PY'
import json, sys
c = json.load(open(sys.argv[1]))
s = c["servers"]["testlab"]
legacy = {"key": s.pop("key"), "servers": {"testlab": s}}
json.dump(legacy, open(sys.argv[2], "w"))
PY
OUT=$(printf 'old\n' | timeout 30 env CATFLAP_DIR="$T/c7" "$CF" proxy testlab 22 2>/dev/null | head -2 | tr -d '\r' | tr '\n' '|')
check "a client from before v0.3 (one shared key) still works" test "$OUT" = "SSH-2.0-catflap-e2e-fake|old|"

echo "== move a homelab to another relay (new address)"
mkdir -p "$T/relay2"
cat > "$T/relay2/relay.json" <<EOF
{"hostname":"127.0.0.1","listen":"127.0.0.1:18444","http_port":0,"stun_port":13479,
 "cert_mode":"manual","cert_file":"$T/cert.pem","key_file":"$T/key.pem",
 "state_dir":"$T/relay2/state","admin_socket":"$T/relay2/admin.sock"}
EOF
"$CF" relay run --config "$T/relay2/relay.json" > "$T/relay2.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do [ -S "$T/relay2/admin.sock" ] && break; sleep 0.1; done
R2="--config $T/relay2/relay.json"
JM=$("$CF" relay join-code $R2 | grep -o 'cfj1-[^ ]*')
check "move refuses while the homelab runs" bash -c "$H3 '$CF' homelab move --join '$JM' --yes 2>&1 | grep -q 'homelab is running'"
kill "$H3PID" 2>/dev/null || true; wait "$H3PID" 2>/dev/null || true
$H3 "$CF" homelab move --join "$JM" --yes > "$T/move.log" 2>&1 || true
check "move: the homelab uses the new relay" grep -q 'now uses relay 127.0.0.1:18444' "$T/move.log"
check "move: gone from the old relay, registered with the new one" bash -c "! '$CF' relay status --config '$T/relay/relay.json' | grep -q 'homelab \"lab3\"' && '$CF' relay status $R2 | grep -q 'homelab \"lab3\"'"
RINV=$(grep -A1 '^  multi:' "$T/move.log" | grep -o 'cfi1-[^ ]*' || true)
check "move: prints a re-enroll invite per device" test -n "$RINV"
$H3 "$CF" homelab run >> "$T/homelab3.log" 2>&1 &
H3PID=$!
PIDS+=("$H3PID")
for _ in $(seq 100); do [ -S "$T/homelab3/control.sock" ] && break; sleep 0.1; done
client c6 enroll "$RINV" --no-ssh-key > "$T/c6m.log" 2>&1 || true
check "the device re-enrolls with it" grep -q 'Approved: this device is "multi"' "$T/c6m.log"
check "the device is replaced, not added twice" bash -c "[ \$($H3 '$CF' list | grep -c '^multi ') = 1 ]"
check "the client's entry points to the new relay, the other homelab is untouched" python3 -I -c 'import json, sys
s = json.load(open(sys.argv[1]))["servers"]
sys.exit(0 if s["lab3"]["relay"].endswith(":18444") and s["testlab"]["relay"].endswith(":18443") else 1)' "$T/c6/client.json"
OUT=$(printf 'moved\n' | timeout 30 env CATFLAP_DIR="$T/c6" "$CF" proxy lab3 22 2>>"$T/proxy.err" | head -2 | tr -d '\r' | tr '\n' '|')
check "the tunnel works through the new relay ($OUT)" test "$OUT" = "SSH-2.0-catflap-e2e-fake|moved|"
JM2=$("$CF" relay join-code $R2 | grep -o 'cfj1-[^ ]*')
check "move refuses the relay it uses already" bash -c "$H3 '$CF' homelab move --join '$JM2' --yes 2>&1 | grep -q 'uses relay 127.0.0.1:18444 already'"
kill "$H3PID" 2>/dev/null || true; wait "$H3PID" 2>/dev/null || true

echo "== invite --replace"
check "a plain invite for an existing name is refused" bash -c "$H '$CF' invite multi 2>&1 | grep -q 'exists already'"
check "--replace for an unknown name is refused" bash -c "$H '$CF' invite nosuch --replace 2>&1 | grep -q 'no device named'"
RI=$($H "$CF" invite multi --replace | grep -o 'cfi1-[^ ]*')
client c6 enroll "$RI" --no-ssh-key > "$T/c6r.log" 2>&1 || true
check "invite --replace re-enrolls an existing device" bash -c "grep -q Approved '$T/c6r.log' && [ \$($H '$CF' list | grep -c '^multi ') = 1 ]"
OUT=$(printf 'again\n' | timeout 30 env CATFLAP_DIR="$T/c6" "$CF" proxy testlab 22 2>>"$T/proxy.err" | head -2 | tr -d '\r' | tr '\n' '|')
check "the re-enrolled device works ($OUT)" test "$OUT" = "SSH-2.0-catflap-e2e-fake|again|"

echo "== the temporary device expires"
for _ in $(seq 120); do $H "$CF" list | grep -q '^tmpdev ' || break; sleep 1; done
check "the temporary device is removed when its time is up" bash -c "! $H '$CF' list | grep -q '^tmpdev '"
check "... with its SSH key" bash -c "! grep -q 'catflap:tmpdev' '$T/homelab/authorized_keys'"
check "... and the relay forgets it" bash -c "! '$CF' relay status --config '$T/relay/relay.json' | grep -q 'device tmpdev'"
OUT=$(printf 'x\n' | timeout 10 env CATFLAP_DIR="$T/ct" "$CF" proxy testlab 22 2>/dev/null | head -1 | tr -d '\r' || true)
check "... and it is cut off" bash -c "[[ '$OUT' != SSH-* ]]"
check "alert: the temporary device was removed" fake has ntfy "temporary device removed: tmpdev"

echo "== relay remove, setup --force, uninstall"
RC="--config $T/relay/relay.json"
lab2() { "$CF" relay status $RC | grep -c 'homelab "lab2"' || true; }
J1=$("$CF" relay join-code $RC | grep -o 'cfj1-[^ ]*')
env CATFLAP_DIR="$T/homelab2" "$CF" setup homelab --join "$J1" --name lab2 --no-service > /dev/null 2>&1
check "a second homelab registers" test "$(lab2)" = 1
J2=$("$CF" relay join-code $RC | grep -o 'cfj1-[^ ]*')
env CATFLAP_DIR="$T/homelab2" "$CF" setup homelab --join "$J2" --name lab2 --no-service --force > /dev/null 2>&1
check "setup --force replaces its relay record instead of adding one" test "$(lab2)" = 1
check "relay remove asks first (no terminal: nothing happens)" bash -c "! '$CF' relay remove lab2 $RC < /dev/null > /dev/null 2>&1 && [ \$('$CF' relay status $RC | grep -c 'homelab \"lab2\"') = 1 ]"
"$CF" relay remove lab2 $RC --yes > "$T/rrm.log" 2>&1
check "relay remove forgets the homelab" bash -c "grep -q 'removed homelab \"lab2\"' '$T/rrm.log' && [ \$('$CF' relay status $RC | grep -c 'homelab \"lab2\"') = 0 ]"
check "relay remove: unknown name" bash -c "'$CF' relay remove nosuch $RC --yes 2>&1 | grep -q 'no homelab \"nosuch\"'"

# Client: c2 is enrolled in testlab; use it once so its agent runs.
printf 'x\n' | timeout 30 env CATFLAP_DIR="$T/c2" "$CF" proxy testlab 22 2>/dev/null | head -1 > /dev/null || true
check "uninstall without --yes and terminal changes nothing" bash -c "! env CATFLAP_DIR='$T/c2' CATFLAP_SSH_CONFIG='$T/c2/ssh_config' '$CF' uninstall < /dev/null > /dev/null 2>&1; test -f '$T/c2/client.json'"
env CATFLAP_DIR="$T/c2" CATFLAP_SSH_CONFIG="$T/c2/ssh_config" "$CF" uninstall --yes > "$T/cun.log" 2>&1
check "client uninstall: ssh config block removed" bash -c "! grep -q 'Host testlab' '$T/c2/ssh_config'"
check "client uninstall: keys and agent gone" bash -c "[ ! -e '$T/c2/client.json' ] && [ ! -e '$T/c2/agent.sock' ]"
check "client uninstall: tells how to remove the device on the homelab" grep -q 'catflap remove phone' "$T/cun.log"
check "client uninstall keeps a binary outside the install locations" bash -c "grep -q 'kept $CF' '$T/cun.log' && [ -x '$CF' ]"

# Homelab: refuses while the daemon runs, then removes everything.
check "homelab uninstall refuses while the daemon runs" bash -c "$H '$CF' uninstall homelab --yes 2>&1 | grep -q 'still running'"
check "... and leaves the SSH keys alone" grep -q "catflap:" "$T/homelab/authorized_keys"
pkill -f "$T/catflap homelab run" || true
for _ in $(seq 50); do [ -S "$T/homelab/control.sock" ] || break; sleep 0.1; done
$H "$CF" uninstall homelab --yes > "$T/hun.log" 2>&1
check "homelab uninstall removes it from the relay" bash -c "grep -q 'removed from relay' '$T/hun.log' && ! '$CF' relay status $RC | grep -q 'homelab \"testlab\"'"
check "homelab uninstall removes catflap's SSH keys" bash -c "! grep -q 'catflap:' '$T/homelab/authorized_keys'"
check "homelab uninstall deletes config, state and channels" bash -c "[ ! -e '$T/homelab/homelab.json' ] && [ ! -e '$T/homelab/homelab-state.json' ] && [ ! -e '$T/homelab/notify.json' ]"
check "nothing left to uninstall" bash -c "$H '$CF' uninstall homelab --yes 2>&1 | grep -q 'no homelab set up'"

echo
echo "passed $pass, failed $fail"
if [ $fail -gt 0 ]; then
  echo "--- relay.log (tail)"; tail -20 "$T/relay.log"
  echo "--- homelab.log (tail)"; tail -20 "$T/homelab.log"
  for f in "$T"/c*.log; do echo "--- $f"; cat "$f"; done
  exit 1
fi
