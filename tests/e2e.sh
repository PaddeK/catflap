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
check "CATFLAP_NO_AGENT fallback works" bash -c "printf 'x\n' | CATFLAP_NO_AGENT=1 CATFLAP_DIR='$T/c1' timeout 30 '$CF' proxy testlab 22 2>/dev/null | head -1 | grep -q SSH-"

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

echo
echo "passed $pass, failed $fail"
if [ $fail -gt 0 ]; then
  echo "--- relay.log (tail)"; tail -20 "$T/relay.log"
  echo "--- homelab.log (tail)"; tail -20 "$T/homelab.log"
  for f in "$T"/c*.log; do echo "--- $f"; cat "$f"; done
  exit 1
fi
