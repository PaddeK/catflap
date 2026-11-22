#!/usr/bin/env bash
# The homelab as the real background service on macOS (LaunchAgent) or
# Windows (scheduled task, run from Git Bash): setup, a device through the
# tunnel, restart, move to another relay, uninstall. Linux: tests/systemd.sh.
#
# It changes the machine: it trusts a test certificate system-wide (macOS:
# sudo, Windows: an elevated shell) and installs the service for the current
# user. Meant for CI runners; elsewhere set CATFLAP_SERVICE_TEST=yes.
#
# Usage: tests/service.sh            CATFLAP_BIN=path tests/service.sh
set -euo pipefail
cd "$(dirname "$0")/.."
case "$(uname -s)" in
Darwin) OS=mac EXE= ;;
MINGW* | MSYS* | CYGWIN*)
	OS=win EXE=.exe
	;;
*) echo "macOS or Windows (Git Bash) only; on Linux use tests/systemd.sh" >&2; exit 2 ;;
esac
if [ "${CI:-}" != true ] && [ "${CATFLAP_SERVICE_TEST:-}" != yes ]; then
	echo "this trusts a test certificate and installs the homelab service on this machine;" >&2
	echo "it is meant for CI. Run it anyway with CATFLAP_SERVICE_TEST=yes." >&2
	exit 2
fi

trap 'echo "service.sh: line $LINENO failed: $BASH_COMMAND" >&2' ERR
T=$(mktemp -d)
PIDS=()
HL_LOG=
pass=0 fail=0
ok() { echo "  PASS  $*"; pass=$((pass + 1)); }
bad() { echo "  FAIL  $*"; fail=$((fail + 1)); }
check() { local d=$1; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }
nat() { if [ $OS = win ]; then cygpath -m "$1"; else printf '%s\n' "$1"; fi; } # path for catflap (C:/...)
not() { if "$@"; then return 1; fi; }
tmo() { perl -e 'alarm shift; exec @ARGV or die' "$@"; }                       # timeout(1) isn't on macOS
PY=$(command -v python3 || command -v python)
if [ $OS = win ]; then PY=$(command -v python || command -v python3); fi

cleanup() {
	set +e
	if [ "$fail" -gt 0 ] || [ -n "${KEEP:-}" ]; then
		for f in "$T"/*.log "$HL_LOG"; do [ -f "$f" ] && { echo "--- $f"; tail -40 "$f"; }; done
	fi
	"$CF" uninstall homelab --yes > /dev/null 2>&1
	for p in ${PIDS[@]+"${PIDS[@]}"}; do kill "$p" 2> /dev/null; done
	if [ $OS = mac ]; then
		sudo security delete-certificate -c catflap-service-test /Library/Keychains/System.keychain > /dev/null 2>&1
		pkill -f "$T/" 2> /dev/null
	else
		certutil -delstore Root catflap-service-test > /dev/null 2>&1
		MSYS_NO_PATHCONV=1 taskkill /F /IM catflap.exe > /dev/null 2>&1 # leftover client agents
	fi
	wait 2> /dev/null
	rm -rf "$T"
}
trap cleanup EXIT

if [ -n "${CATFLAP_BIN:-}" ]; then
	cp "$CATFLAP_BIN" "$T/catflap$EXE"
else
	go build -o "$T/catflap$EXE" ./cmd/catflap
fi
CF="$T/catflap$EXE"
if [ $OS = mac ]; then
	HL_LOG="$HOME/.local/state/catflap/homelab.log"
	KEYS="$HOME/.ssh/authorized_keys"
else
	HL_LOG="$(cygpath -u "$LOCALAPPDATA")/catflap/homelab.log"
	KEYS="$(cygpath -u "${ProgramData:-C:\\ProgramData}")/ssh/administrators_authorized_keys" # the runner is an admin
fi

echo "== trusted test certificate + fake sshd"
MSYS_NO_PATHCONV=1 openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=catflap-service-test \
	-addext subjectAltName=IP:127.0.0.1 -addext extendedKeyUsage=serverAuth \
	-keyout "$(nat "$T/key.pem")" -out "$(nat "$T/cert.pem")" 2> "$T/openssl.log" || { cat "$T/openssl.log"; exit 1; }
if [ $OS = mac ]; then
	# Trust settings want a GUI confirmation since macOS 11, unless allowed:
	sudo security authorizationdb write com.apple.trust-settings.admin allow || true
	sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain "$T/cert.pem"
else
	certutil -addstore -f Root "$(cygpath -w "$T/cert.pem")" > "$T/certutil.log" 2>&1 || { cat "$T/certutil.log"; exit 1; }
fi

"$PY" -I - "$(nat "$T/sshd.port")" << 'PY' &
import socket, sys, threading
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 0)); s.listen(16)
open(sys.argv[1], "w").write(str(s.getsockname()[1]))
def serve(c):
    try:
        c.sendall(b"SSH-2.0-catflap-service-fake\r\n")
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
for _ in $(seq 100); do [ -s "$T/sshd.port" ] && break; sleep 0.1; done
SSHD_PORT=$(cat "$T/sshd.port")

relay() { # relay NAME PORT STUNPORT: start a relay, print a join code
	mkdir -p "$T/$1"
	cat > "$T/$1/relay.json" << EOF
{"hostname":"127.0.0.1","listen":"127.0.0.1:$2","http_port":0,"stun_port":$3,
 "cert_mode":"manual","cert_file":"$(nat "$T/cert.pem")","key_file":"$(nat "$T/key.pem")",
 "state_dir":"$(nat "$T/$1/state")","admin_socket":"$(nat "$T/$1/admin.sock")"}
EOF
	"$CF" relay run --config "$(nat "$T/$1/relay.json")" > "$T/$1.log" 2>&1 &
	PIDS+=($!)
	for _ in $(seq 100); do
		code=$("$CF" relay join-code --config "$(nat "$T/$1/relay.json")" 2> /dev/null | grep -o 'cfj1-[^ ]*') && break
		sleep 0.2
	done
	echo "$code"
}
client() { env CATFLAP_DIR="$(nat "$T/dev")" CATFLAP_SSH_CONFIG="$(nat "$T/dev/ssh_config")" "$CF" "$@"; }
tunnel() { # one round trip through the homelab's tunnel
	local out
	out=$(printf '%s\n' "$1" | tmo 30 env CATFLAP_DIR="$(nat "$T/dev")" "$CF" proxy svclab 22 2>> "$T/proxy.log" | head -2 | tr -d '\r' | tr '\n' '|')
	[ "$out" = "SSH-2.0-catflap-service-fake|$1|" ]
}
started() { "$CF" status 2> /dev/null | grep -o 'running catflap [^ ]*'; }
service_loaded() {
	if [ $OS = mac ]; then launchctl print "gui/$(id -u)/catflap.homelab" > /dev/null 2>&1; else MSYS_NO_PATHCONV=1 schtasks /Query /TN catflap-homelab > /dev/null 2>&1; fi
}

echo "== relay"
JOIN=$(relay relay1 18443 13478)
check "join code" test -n "$JOIN"

echo "== setup homelab (real service)"
"$CF" setup homelab --join "$JOIN" --name svclab --forward "22=127.0.0.1:$SSHD_PORT" > "$T/setup.log" 2>&1 || true
check "setup homelab: the service runs" grep -q 'Homelab is running' "$T/setup.log"
check "the service is registered" service_loaded
if [ $OS = mac ]; then
	check "LaunchAgent plist written" test -f "$HOME/Library/LaunchAgents/catflap.homelab.plist"
	check "LaunchAgent runs homelab run --service" bash -c "launchctl print gui/$(id -u)/catflap.homelab | grep -q -- '--service'"
else
	MSYS_NO_PATHCONV=1 schtasks /Query /TN catflap-homelab /XML > "$T/task.xml" 2>&1
	check "task starts at boot, as the user without a password (S4U)" bash -c "tr -d '\0' < '$T/task.xml' | grep -q BootTrigger && tr -d '\0' < '$T/task.xml' | grep -q '<LogonType>S4U</LogonType>'"
	check "task runs elevated for an admin account" bash -c "tr -d '\0' < '$T/task.xml' | grep -q '<RunLevel>HighestAvailable</RunLevel>'"
	check "the output names the admin keys file" grep -q 'administrators_authorized_keys' "$T/setup.log"
fi
check "catflap status: running" bash -c "'$CF' status > '$T/status.out' && grep -q '^Homelab \"svclab\"' '$T/status.out' && grep -q 'running catflap' '$T/status.out'"
check "the service logs to a file" grep -q 'homelab "svclab" up' "$HL_LOG"

echo "== a device through the service's tunnel"
mkdir -p "$T/dev"
ssh-keygen -q -t ed25519 -N "" -C dev -f "$T/dev/id"
INV=$("$CF" invite dev1 | grep -o 'cfi1-[^ ]*')
client enroll "$INV" --ssh-key "$(nat "$T/dev/id.pub")" --alias svclab > "$T/enroll.log" 2>&1 || true
check "device enrolled, tunnel check ok" bash -c "grep -q 'Approved: this device is \"dev1\"' '$T/enroll.log' && grep -q 'ok (SSH-2.0-catflap-service-fake)' '$T/enroll.log'"
check "proxy round trip" tunnel meow
check "SSH key added, localhost-only" grep -q '^from="127.0.0.1,::1" ssh-ed25519 .* catflap:dev1$' "$KEYS"
if [ $OS = win ]; then
	icacls "$(cygpath -w "$KEYS")" > "$T/acl.log" 2>&1
	check "admin keys file: SYSTEM and Administrators only" bash -c "grep -q 'NT AUTHORITY.SYSTEM:(F)' '$T/acl.log' && grep -q 'BUILTIN.Administrators:(F)' '$T/acl.log' && [ \$(grep -c ':(' '$T/acl.log') -eq 2 ]"
fi

echo "== restart"
BEFORE=$(started)
"$CF" restart > "$T/restart.log" 2>&1 || true
check "catflap restart: running again" grep -q 'running catflap .* again' "$T/restart.log"
check "the tunnel works after the restart" tunnel purr
if [ $OS = mac ]; then
	pkill -f 'catflap homelab run --service' || true
	for _ in $(seq 60); do started > /dev/null && break; sleep 0.5; done
	check "launchd restarts a killed homelab" bash -c "'$CF' status | grep -q 'running catflap'"
fi
check "status still running ($BEFORE)" bash -c "'$CF' status | grep -q 'running catflap'"

echo "== move to another relay (stop, switch, start)"
JOIN2=$(relay relay2 18444 13479)
"$CF" homelab move --join "$JOIN2" --yes > "$T/move.log" 2>&1 || true
check "move: now on the new relay" grep -q 'now uses relay 127.0.0.1:18444' "$T/move.log"
check "move: the service runs again" bash -c "'$CF' status > '$T/status.out' && grep -qE '^  relay +127.0.0.1:18444 ' '$T/status.out' && grep -q 'running catflap' '$T/status.out'"
INV2=$(grep -o 'cfi1-[^ ]*' "$T/move.log" | head -1)
client enroll "$INV2" > "$T/reenroll.log" 2>&1 || true
check "re-enrolled through the new relay" tunnel hiss

echo "== uninstall"
"$CF" uninstall homelab --yes > "$T/uninstall.log" 2>&1 || true
check "uninstall: done" grep -q '^done' "$T/uninstall.log"
check "the service is gone" not service_loaded
check "the homelab stopped" bash -c "! '$CF' status | grep -q '^Homelab'"
check "catflap's SSH key removed" bash -c "! grep -q 'catflap:dev1' '$KEYS'"
check "the service log deleted" test ! -e "$HL_LOG"
if [ $OS = mac ]; then
	check "LaunchAgent plist removed" test ! -f "$HOME/Library/LaunchAgents/catflap.homelab.plist"
fi

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
