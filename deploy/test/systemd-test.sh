#!/usr/bin/env bash
# End-to-end deployment test in a disposable systemd container:
# install, sandbox checks, real SSH sessions, upgrade, rollback, uninstall,
# reinstall with preserved data, port-conflict refusal and purge.
#
#   make build && deploy/test/systemd-test.sh [debian:12|ubuntu:24.04]
#
# Requires Docker with permission to run privileged containers (systemd
# needs it for its sandboxing features). Nothing on the host is modified
# except a temporary container and image.
set -euo pipefail
BASE="${1:-debian:12}"
TAG="flap-systemd:$(echo "$BASE" | tr ':/' '--')"
NAME="flap-systemd-test-$$"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HOST_PORT="${HOST_PORT:-22922}"

pass() { printf '\033[1;32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*" >&2; docker exec "$NAME" journalctl -u flapd -n 40 --no-pager >&2 || true; exit 1; }
ct()   { docker exec "$NAME" "$@"; }

[ -x "$REPO/bin/flapd" ] || { echo "build first: make build" >&2; exit 1; }
docker build -q -t "$TAG" --build-arg BASE="$BASE" -f "$REPO/deploy/test/Dockerfile.systemd" "$REPO/deploy/test" >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
docker run -d --name "$NAME" --privileged --cgroupns=private \
	--tmpfs /run --tmpfs /run/lock -v "$REPO:/src:ro" \
	-p "127.0.0.1:${HOST_PORT}:2222" "$TAG" >/dev/null
for _ in $(seq 1 60); do
	state="$(ct systemctl is-system-running 2>/dev/null || true)"
	case "$state" in running|degraded) break ;; esac
	sleep 0.5
done
pass "systemd booted in $BASE ($state)"

# 1. Fresh install.
ct /src/deploy/install.sh >/tmp/flap-install.log 2>&1 || { cat /tmp/flap-install.log; fail "install.sh"; }
ct systemctl is-active --quiet flapd || fail "service not active after install"
pass "install.sh: service active"

# 2. Process identity and sandbox.
pid="$(ct systemctl show -p MainPID --value flapd)"
[ "$(ct ps -o user= -p "$pid" | tr -d ' ')" = "flapd" ] || fail "service not running as flapd"
ct grep -q '^NoNewPrivs:[[:space:]]*1' "/proc/$pid/status" || fail "NoNewPrivs not set"
ct grep -q '^CapEff:[[:space:]]*0000000000000000' "/proc/$pid/status" || fail "process has capabilities"
ct id flapd | grep -qv sudo || fail "flapd user is privileged"
[ "$(ct getent passwd flapd | cut -d: -f7)" = "/usr/sbin/nologin" ] || fail "flapd user has a login shell"
[ "$(ct stat -c %a /var/lib/flapd)" = "700" ] || fail "state directory permissions"
for f in /var/lib/flapd/ssh_host_ed25519_key /var/lib/flapd/identity_secret /var/lib/flapd/flap.db; do
	[ "$(ct stat -c '%a %U' "$f")" = "600 flapd" ] || fail "$f should be 600 flapd, is $(ct stat -c '%a %U' "$f")"
done
score="$(ct systemd-analyze security flapd.service 2>/dev/null | awk '/Overall exposure level/ {print $(NF-2), $(NF-1)}')"
pass "runs as flapd, NoNewPrivs, no capabilities, private files, exposure ${score}"

# Writes outside the state directory must fail even for the service user.
ct systemd-run --wait --quiet -p User=flapd -p ProtectSystem=strict -p StateDirectory=flapd -p ProtectHome=true \
	/bin/sh -c 'touch /etc/flap-probe 2>/dev/null && exit 1; touch /var/lib/flapd/probe && rm /var/lib/flapd/probe' \
	|| fail "sandbox write probe"
pass "sandbox: /etc read-only, /var/lib/flapd writable"

# 3. Real SSH sessions from the host through the published port.
ct /usr/local/bin/flapd fingerprint -config /etc/flapd/flapd.toml | tail -1 > /tmp/flap-host.pub
fp1="$(ct /usr/local/bin/flapd fingerprint -config /etc/flapd/flapd.toml | head -1)"
"$REPO/bin/flapload" -addr "127.0.0.1:${HOST_PORT}" -n 3 -duration 6s -hostkey /tmp/flap-host.pub >/tmp/flap-load.log 2>&1 \
	|| { cat /tmp/flap-load.log; fail "flapload"; }
grep -q 'sessions completed: 3, failed: 0' /tmp/flap-load.log || { cat /tmp/flap-load.log; fail "sessions failed"; }
ct runuser -u flapd -- /usr/local/bin/flapd check-db -config /etc/flapd/flapd.toml | grep -q '3 players' || fail "players not stored"
pass "3 SSH game sessions from the host; players persisted"

# 4. Upgrade: backup + previous binary kept, service healthy.
ct /src/deploy/install.sh >/tmp/flap-upgrade.log 2>&1 || { cat /tmp/flap-upgrade.log; fail "upgrade"; }
ct sh -c 'ls /var/lib/flapd/backups/flap-*.db' >/dev/null || fail "no backup taken during upgrade"
ct test -f /usr/local/bin/flapd.prev || fail "previous binary not kept"
ct runuser -u flapd -- /usr/local/bin/flapd healthcheck -config /etc/flapd/flapd.toml >/dev/null || fail "unhealthy after upgrade"
pass "upgrade: backup written, previous binary kept, healthy"

# 5. Rollback.
ct /src/deploy/rollback.sh >/dev/null || fail "rollback.sh"
pass "rollback.sh: healthy on the previous binary"

# 6. Uninstall keeps data; reinstall reuses host key and scores.
ct /src/deploy/uninstall.sh >/dev/null || fail "uninstall.sh"
ct test ! -e /etc/systemd/system/flapd.service || fail "unit left behind"
ct test -f /var/lib/flapd/flap.db || fail "uninstall deleted the database"
ct /src/deploy/install.sh >/dev/null 2>&1 || fail "reinstall"
fp2="$(ct /usr/local/bin/flapd fingerprint -config /etc/flapd/flapd.toml | head -1)"
[ "$fp1" = "$fp2" ] || fail "host key changed across reinstall"
ct runuser -u flapd -- /usr/local/bin/flapd check-db -config /etc/flapd/flapd.toml | grep -q '3 players' || fail "scores lost across reinstall"
pass "uninstall keeps data; reinstall keeps host key ${fp2%% *} and players"

# 7. Restart preserves scores and the service restarts after a crash.
ct systemctl kill -s SIGKILL flapd
sleep 4
ct systemctl is-active --quiet flapd || fail "service did not restart after SIGKILL"
pass "Restart=on-failure brings the service back after SIGKILL"

# 8. A port conflict is refused clearly on a fresh install.
ct /src/deploy/uninstall.sh --purge --yes >/dev/null
ct sh -c 'printf "[server]\nlisten = \"127.0.0.1:2222\"\nhost_key_files = [\"/tmp/k\"]\nidentity_secret_file = \"/tmp/s\"\n[storage]\npath = \"/tmp/x.db\"\n" > /tmp/occupy.toml'
docker exec -d "$NAME" /src/bin/flapd serve -config /tmp/occupy.toml
sleep 1
if ct /src/deploy/install.sh >/tmp/flap-conflict.log 2>&1; then fail "install succeeded although the port was taken"; fi
grep -q 'already in use' /tmp/flap-conflict.log || { cat /tmp/flap-conflict.log; fail "unclear port conflict message"; }
pass "port conflict detected and explained"
ct pkill -f occupy.toml || true

# 9. Purge removes everything.
ct /src/deploy/uninstall.sh --purge --yes >/dev/null
ct test ! -e /var/lib/flapd || fail "purge left data"
if ct id flapd >/dev/null 2>&1; then fail "purge left the user"; fi
pass "uninstall --purge removes data, config and user"
echo "all deployment checks passed on $BASE"
