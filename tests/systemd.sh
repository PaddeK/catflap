#!/usr/bin/env bash
# The parts e2e.sh can't cover, in a throwaway Docker container that runs
# systemd: `setup relay` (system service + user), `setup homelab` (user
# service + lingering), `restart`, and `uninstall` of all three roles,
# including the binaries.
#
# Usage: tests/systemd.sh [CATFLAP_BINARY]   (default: builds linux/amd64)
# Needs Docker with privileged containers (systemd as PID 1).
# The ~ in the scripts below expands inside the container, as the test user.
# shellcheck disable=SC2088
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$HOME/.local/go/bin:$PATH"
BIN=${1:-}
T=$(mktemp -d /tmp/cfsysd.XXXX)
NAME=cfsysd-$$
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$T"; }
trap cleanup EXIT

if [ -z "$BIN" ]; then
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$T/catflap" ./cmd/catflap
	BIN=$T/catflap
fi

docker build -q -t catflap-systemd-test - > /dev/null <<'EOF'
FROM ubuntu:24.04
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
      systemd systemd-sysv dbus openssh-client openssh-server openssl ca-certificates sudo > /dev/null \
 && rm -rf /var/lib/apt/lists/*
CMD ["/sbin/init"]
EOF
docker run -d --name "$NAME" --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
	--tmpfs /run --tmpfs /run/lock catflap-systemd-test > /dev/null
docker cp "$BIN" "$NAME:/opt/catflap"
for _ in $(seq 60); do docker exec "$NAME" systemctl is-system-running 2>/dev/null | grep -qE 'running|degraded' && break; sleep 1; done

pass=0 fail=0
ok()  { echo "  PASS  $*"; pass=$((pass+1)); }
bad() { echo "  FAIL  $*"; fail=$((fail+1)); }
# check NAME SCRIPT: run SCRIPT as root in the container
check() { if docker exec "$NAME" bash -c "$2" > "$T/out" 2>&1; then ok "$1"; else bad "$1"; sed 's/^/        /' "$T/out" | tail -15; fi; }
# as USER SCRIPT: run SCRIPT as USER with a systemd user session
as() { docker exec "$NAME" bash -c "uid=\$(id -u $1); sudo -u $1 env XDG_RUNTIME_DIR=/run/user/\$uid DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/\$uid/bus HOME=/home/$1 bash -c '$2'"; }
checkas() { if as "$2" "$3" > "$T/out" 2>&1; then ok "$1"; else bad "$1"; sed 's/^/        /' "$T/out" | tail -15; fi; }

echo "== setup"
docker exec "$NAME" bash -c '
set -e
echo "127.0.0.1 relay.test relay2.test" >> /etc/hosts
mkdir -p /opt/cert && cd /opt/cert
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=relay.test \
  -addext subjectAltName=DNS:relay.test,DNS:relay2.test -keyout key.pem -out cert.pem 2>/dev/null
chmod 644 key.pem   # test only: the relay service user must read it
cp cert.pem /usr/local/share/ca-certificates/relay-test.crt && update-ca-certificates > /dev/null 2>&1
useradd -m -s /bin/bash alice && useradd -m -s /bin/bash bob
loginctl enable-linger alice && loginctl enable-linger bob
systemctl start ssh   # behind the homelab: a real SSH server to reach through the tunnel
'
for _ in $(seq 30); do as alice 'systemctl --user is-system-running' 2>/dev/null | grep -qE 'running|degraded' && break; sleep 1; done

docker exec "$NAME" bash -c '/opt/catflap setup relay --hostname relay.test --cert-mode manual --cert /opt/cert/cert.pem --key /opt/cert/key.pem --http-port 0 --no-firewall' > "$T/relay.log" 2>&1 || true
JOIN=$(grep -o 'cfj1-[^ ]*' "$T/relay.log" | head -1)
check "setup relay: system service runs" "systemctl is-active --quiet catflap-relay && test -x /usr/local/bin/catflap && id catflap"
checkas "setup homelab: user service runs" alice "/opt/catflap setup homelab --join $JOIN --name lab && systemctl --user is-active --quiet catflap && test -x ~/.local/bin/catflap"
checkas "restart (homelab) waits and reports the version" alice "~/.local/bin/catflap restart | grep -q \"running catflap .* again\""
check "restart (relay) as root" "catflap restart | grep -q 'relay running again'"
checkas "restart (relay) without root says sudo" alice "/usr/local/bin/catflap restart relay 2>&1 | grep -q 'sudo catflap restart'"

INV=$(as alice '~/.local/bin/catflap invite bobs-laptop' | grep -o 'cfi1-[^ ]*')
docker exec "$NAME" bash -c 'mkdir -p /home/bob/.local/bin && cp /opt/catflap /home/bob/.local/bin/catflap && chown -R bob: /home/bob/.local'
checkas "client enrolls" bob "ssh-keygen -q -t ed25519 -N \"\" -f ~/.ssh/id_ed25519 && ~/.local/bin/catflap enroll $INV | grep -q Approved"
check "relay lists homelab and device" "catflap relay status | grep -q 'homelab \"lab\"' && catflap relay status | grep -q 'device bobs-laptop'"
checkas "client reaches the homelab's SSH server through the tunnel" bob "for i in \$(seq 15); do echo | timeout 10 ~/.local/bin/catflap proxy lab 22 2>/dev/null | head -1 | grep -q SSH-2.0 && exit 0; sleep 2; done; exit 1"

echo "== move the relay: backup, wipe, restore"
check "relay backup" "catflap relay backup /root/relay-backup.json | grep -q '1 homelab' && [ \$(stat -c %a /root/relay-backup.json) = 600 ]"
# A device enrolled after the backup: the restored relay doesn't know it
# until the homelab re-sends its device list after the outage.
INV2=$(as alice '~/.local/bin/catflap invite late-device' | grep -o 'cfi1-[^ ]*')
check "a device enrolls after the backup" "CATFLAP_DIR=/root/late /opt/catflap enroll $INV2 --no-ssh-key --no-ssh-config | grep -q Approved"
check "wipe the relay completely" "catflap uninstall relay --yes > /dev/null && test ! -e /var/lib/catflap && ! systemctl is-active --quiet catflap-relay"
check "restore from the backup" "/opt/catflap setup relay --restore /root/relay-backup.json --no-firewall > /tmp/restore.log 2>&1 && systemctl is-active --quiet catflap-relay && catflap relay status | grep -q 'homelab \"lab\"' && catflap relay status | grep -q 'device bobs-laptop'"
check "restore refuses to replace a running relay without --force" "! /opt/catflap setup relay --restore /root/relay-backup.json --no-firewall > /dev/null 2>&1"
check "the homelab reconnects and re-sends devices enrolled after the backup" "for i in \$(seq 60); do catflap relay status | grep -q 'device late-device' && exit 0; sleep 2; done; exit 1"
checkas "the homelab works with the restored relay (invites)" alice "~/.local/bin/catflap invite after-move | grep -q cfi1-"
checkas "the client reaches the homelab again" bob "for i in \$(seq 30); do echo | timeout 10 ~/.local/bin/catflap proxy lab 22 2>/dev/null | head -1 | grep -q SSH-2.0 && exit 0; sleep 2; done; exit 1"

echo "== move the homelab to another relay (new hostname)"
docker exec "$NAME" bash -c 'mkdir -p /opt/relay2 && cat > /opt/relay2/relay.json <<EOF
{"hostname":"relay2.test","listen":":8443","http_port":0,"stun_port":0,"cert_mode":"manual",
 "cert_file":"/opt/cert/cert.pem","key_file":"/opt/cert/key.pem","state_dir":"/opt/relay2/state","admin_socket":"/opt/relay2/admin.sock"}
EOF'
docker exec -d "$NAME" bash -c '/opt/catflap relay run --config /opt/relay2/relay.json > /tmp/relay2.log 2>&1'
for _ in $(seq 30); do docker exec "$NAME" test -S /opt/relay2/admin.sock && break; sleep 0.5; done
JM=$(docker exec "$NAME" /opt/catflap relay join-code --config /opt/relay2/relay.json | grep -o 'cfj1-[^ ]*')
checkas "move: the service is stopped, switched and started again" alice "~/.local/bin/catflap homelab move --join $JM --yes > /tmp/move.log 2>&1 && grep -q \"now uses relay relay2.test:8443\" /tmp/move.log && systemctl --user is-active --quiet catflap && ~/.local/bin/catflap status | grep -q relay2.test"
check "move: gone from the old relay, on the new one" "! catflap relay status | grep -q 'homelab \"lab\"' && /opt/catflap relay status --config /opt/relay2/relay.json | grep -q 'homelab \"lab\"'"
RINV=$(docker exec "$NAME" grep -A1 '^  bobs-laptop:' /tmp/move.log | grep -o 'cfi1-[^ ]*' || true)
checkas "the client re-enrolls and reaches the homelab through the new relay" bob "~/.local/bin/catflap enroll $RINV | grep -q Approved && for i in \$(seq 30); do echo | timeout 10 ~/.local/bin/catflap proxy lab 22 2>/dev/null | head -1 | grep -q SSH-2.0 && exit 0; sleep 2; done; exit 1"
checkas "the device was replaced, and its SSH key too" alice "[ \$(~/.local/bin/catflap list | grep -c \"^bobs-laptop \") = 1 ] && [ \$(grep -c catflap:bobs-laptop ~/.ssh/authorized_keys) = 1 ]"

echo "== uninstall"
checkas "client: uninstall removes everything incl. the binary" bob "~/.local/bin/catflap uninstall --yes > /tmp/bob.log && ! grep -q \"Host lab\" ~/.ssh/config && test ! -e ~/.config/catflap && test ! -e ~/.local/bin/catflap"
checkas "homelab: the plan mentions the relay needs root" alice "~/.local/bin/catflap uninstall < /dev/null 2>&1 | grep -q \"Not included: the relay\""
checkas "homelab: uninstall removes service, files and binary" alice "~/.local/bin/catflap uninstall --yes > /tmp/alice.log 2>&1; ! systemctl --user cat catflap >/dev/null 2>&1 && test ! -e ~/.config/systemd/user/catflap.service && test ! -e ~/.local/state/catflap && test ! -e ~/.config/catflap && test ! -e ~/.local/bin/catflap"
check "homelab removed itself from the relay" "! catflap relay status | grep -q 'homelab \"lab\"'"
check "relay: uninstall removes service, dirs, user and binary" "catflap uninstall --yes > /tmp/root.log 2>&1; ! systemctl cat catflap-relay >/dev/null 2>&1 && test ! -e /etc/catflap && test ! -e /var/lib/catflap && ! id catflap 2>/dev/null && test ! -e /usr/local/bin/catflap"
check "nothing listens on 443 anymore" "! (exec 3<>/dev/tcp/127.0.0.1/443) 2>/dev/null"

if [ "$fail" -gt 0 ]; then
	for f in bob alice root restore move relay2; do echo "--- $f log"; docker exec "$NAME" cat /tmp/$f.log 2>/dev/null || true; done
	echo "--- relay setup log"; cat "$T/relay.log"
fi
echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
