#!/usr/bin/env bash
# Install or upgrade flapd on a systemd-based Debian or Ubuntu server.
#
#   sudo ./deploy/install.sh [--binary ./bin/flapd] [--port 2222] [--no-start]
#
# Safe to run repeatedly. It never edits sshd, firewall rules or other
# services, never overwrites an existing /etc/flapd/flapd.toml, and keeps the
# previous binary and a database backup so an upgrade can be rolled back
# (automatically if the new version fails its health check).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY="${HERE}/../bin/flapd"
PORT=""
START=1
ALLOW_22=0

BIN_DIR=/usr/local/bin
DOC_DIR=/usr/local/share/doc/flapd
CONF_DIR=/etc/flapd
CONF="${CONF_DIR}/flapd.toml"
STATE_DIR=/var/lib/flapd
BACKUP_DIR="${STATE_DIR}/backups"
UNIT=/etc/systemd/system/flapd.service
SVC_USER=flapd

say()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
	sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

while [ $# -gt 0 ]; do
	case "$1" in
		--binary) BINARY="$2"; shift 2 ;;
		--port) PORT="$2"; shift 2 ;;
		--no-start) START=0; shift ;;
		--allow-port-22) ALLOW_22=1; shift ;;
		-h|--help) usage 0 ;;
		*) warn "unknown argument $1"; usage 2 ;;
	esac
done

# --- prerequisites ---------------------------------------------------------
[ "$(id -u)" -eq 0 ] || die "run as root (sudo $0 ...)"
command -v systemctl >/dev/null || die "systemd is required"
[ -d /run/systemd/system ] || die "systemd is not running as the init system"
command -v runuser >/dev/null || die "runuser (util-linux) is required"
if [ -r /etc/os-release ]; then
	# shellcheck source=/dev/null
	. /etc/os-release
	case "${ID:-} ${ID_LIKE:-}" in
		*debian*|*ubuntu*) ;;
		*) warn "tested on Debian and Ubuntu; continuing on ${PRETTY_NAME:-unknown}" ;;
	esac
fi
[ -f "$BINARY" ] || die "binary not found at $BINARY (build it with: make build)"
if [ ! -f "${HERE}/flapd.service" ] || [ ! -f "${HERE}/flapd.toml" ]; then
	die "run from a checkout: deploy/flapd.service and deploy/flapd.toml are missing"
fi
if [ -n "$PORT" ]; then
	if ! [[ "$PORT" =~ ^[0-9]+$ ]] || [ "$PORT" -lt 1 ] || [ "$PORT" -gt 65535 ]; then
		die "--port must be 1-65535"
	fi
fi
NEW_VERSION="$("$BINARY" version)" || die "$BINARY does not run on this machine (wrong architecture?)"
say "installing ${NEW_VERSION}"

# --- service account -------------------------------------------------------
if ! id -u "$SVC_USER" >/dev/null 2>&1; then
	say "creating system user ${SVC_USER}"
	useradd --system --user-group --home-dir "$STATE_DIR" --no-create-home \
		--shell /usr/sbin/nologin --comment "Flap SSH game server" "$SVC_USER"
fi
# Never give the service account a login shell or sudo rights.
usermod --shell /usr/sbin/nologin "$SVC_USER" >/dev/null
if id -nG "$SVC_USER" | tr ' ' '\n' | grep -qxE 'sudo|wheel|admin|adm'; then
	die "user ${SVC_USER} is in a privileged group; remove it first"
fi

# --- directories and configuration -----------------------------------------
install -d -m 0750 -o root -g "$SVC_USER" "$CONF_DIR"
install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$STATE_DIR"
install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$BACKUP_DIR"
if [ ! -f "$CONF" ]; then
	say "installing default configuration to ${CONF}"
	tmp="$(mktemp)"
	cp "${HERE}/flapd.toml" "$tmp"
	if [ -n "$PORT" ]; then
		sed -i "s|^listen = \"0.0.0.0:2222\"|listen = \"0.0.0.0:${PORT}\"|" "$tmp"
	fi
	install -m 0640 -o root -g "$SVC_USER" "$tmp" "$CONF"
	rm -f "$tmp"
else
	say "keeping existing configuration ${CONF}"
	[ -z "$PORT" ] || warn "--port ignored because ${CONF} already exists; edit server.listen there"
fi

# Validate the configuration with the new binary before touching anything
# that is running.
runuser -u "$SVC_USER" -- "$BINARY" check-config -config "$CONF" >/dev/null \
	|| die "configuration ${CONF} is invalid for ${NEW_VERSION}:
$(runuser -u "$SVC_USER" -- "$BINARY" check-config -config "$CONF" 2>&1 || true)"
LISTEN="$(runuser -u "$SVC_USER" -- "$BINARY" check-config -config "$CONF" -listen)"
LISTEN_PORT="${LISTEN##*:}"

if [ "$LISTEN_PORT" = "22" ] && [ "$ALLOW_22" -ne 1 ]; then
	die "server.listen uses port 22, the usual administrative SSH port. Pick another port
(for example 2222) so you cannot lock yourself out, or pass --allow-port-22 if sshd runs elsewhere."
fi

RUNNING=0
if systemctl is-active --quiet flapd; then RUNNING=1; fi

# The port must be free unless flapd itself is the one listening on it.
if [ "$RUNNING" -eq 0 ] && command -v ss >/dev/null; then
	if ss -H -ltn "sport = :${LISTEN_PORT}" | grep -q .; then
		die "port ${LISTEN_PORT} is already in use:
$(ss -ltnp "sport = :${LISTEN_PORT}" 2>/dev/null || true)
Choose another port in ${CONF} (server.listen) or stop the other service."
	fi
fi

# --- upgrade safety: backup and keep the previous binary -------------------
if [ -f "${BIN_DIR}/flapd" ]; then
	OLD_VERSION="$("${BIN_DIR}/flapd" version 2>/dev/null || echo unknown)"
	say "upgrading from ${OLD_VERSION}"
	stamp="$(date -u +%Y%m%dT%H%M%SZ)"
	if runuser -u "$SVC_USER" -- "${BIN_DIR}/flapd" backup -config "$CONF" -out "${BACKUP_DIR}/flap-${stamp}.db" >/dev/null 2>&1; then
		say "database backed up to ${BACKUP_DIR}/flap-${stamp}.db"
	else
		warn "could not back up the database (first install or unreadable database); continuing"
	fi
	# Keep the ten newest backups (names sort chronologically).
	find "$BACKUP_DIR" -maxdepth 1 -name 'flap-*.db' -printf '%f\n' | sort -r | tail -n +11 |
		while read -r old; do rm -f -- "${BACKUP_DIR}/${old}"; done
	cp -p "${BIN_DIR}/flapd" "${BIN_DIR}/flapd.prev"
fi

# --- binary, docs and unit -------------------------------------------------
install -m 0755 -o root -g root "$BINARY" "${BIN_DIR}/flapd.new"
mv -f "${BIN_DIR}/flapd.new" "${BIN_DIR}/flapd"
install -d -m 0755 "$DOC_DIR"
for f in README.md docs/OPERATIONS.md docs/SECURITY.md docs/ARCHITECTURE.md; do
	[ -f "${HERE}/../${f}" ] && install -m 0644 "${HERE}/../${f}" "${DOC_DIR}/$(basename "$f")"
done

# Host key and identity secret, created once and then kept forever.
runuser -u "$SVC_USER" -- "${BIN_DIR}/flapd" init -config "$CONF"

install -m 0644 -o root -g root "${HERE}/flapd.service" "$UNIT"
if command -v systemd-analyze >/dev/null; then
	systemd-analyze verify "$UNIT" 2>&1 | grep -v -e 'RestartSteps' -e 'RestartMaxDelaySec' >&2 || true
fi
systemctl daemon-reload

health() {
	for _ in $(seq 1 30); do
		if systemctl is-active --quiet flapd && runuser -u "$SVC_USER" -- "${BIN_DIR}/flapd" healthcheck -config "$CONF" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	return 1
}

if [ "$START" -eq 1 ]; then
	systemctl enable --quiet flapd
	if [ "$RUNNING" -eq 1 ]; then
		say "restarting flapd (players get a notice and their runs are saved)"
		systemctl restart flapd
	else
		say "starting flapd"
		systemctl start flapd
	fi
	if ! health; then
		journalctl -u flapd -n 30 --no-pager >&2 || true
		if [ -f "${BIN_DIR}/flapd.prev" ] && [ "$RUNNING" -eq 1 ]; then
			warn "new version failed its health check; rolling back"
			mv -f "${BIN_DIR}/flapd.prev" "${BIN_DIR}/flapd"
			systemctl restart flapd
			if health; then
				die "rolled back to the previous version; see the log above"
			fi
			die "rollback also failed; see: journalctl -u flapd"
		fi
		die "flapd did not become healthy; see: journalctl -u flapd"
	fi
	say "flapd is running"
fi

# --- summary ---------------------------------------------------------------
echo
"${BIN_DIR}/flapd" version
echo "Host key fingerprint(s) to publish for players:"
runuser -u "$SVC_USER" -- "${BIN_DIR}/flapd" fingerprint -config "$CONF" | grep '^SHA256' | sed 's/^/  /'
echo
echo "Players connect with:"
echo "  ssh -p ${LISTEN_PORT} flappy@$(hostname -f 2>/dev/null || hostname)"
echo
echo "Firewall: flapd does not change firewall rules. Allow TCP ${LISTEN_PORT} yourself, e.g."
if command -v ufw >/dev/null; then
	echo "  sudo ufw allow ${LISTEN_PORT}/tcp comment 'flapd'"
elif command -v firewall-cmd >/dev/null; then
	echo "  sudo firewall-cmd --permanent --add-port=${LISTEN_PORT}/tcp && sudo firewall-cmd --reload"
else
	echo "  sudo nft add rule inet filter input tcp dport ${LISTEN_PORT} accept"
fi
echo "and open the port in your cloud provider's security group if it has one."
echo "Logs: journalctl -u flapd -f    Status: systemctl status flapd"
