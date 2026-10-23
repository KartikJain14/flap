#!/usr/bin/env bash
# Remove the flapd service and binary.
#
#   sudo ./deploy/uninstall.sh            # keeps /etc/flapd and /var/lib/flapd
#   sudo ./deploy/uninstall.sh --purge    # also deletes data, config and user
#
# By default nothing that holds player data is touched: the database, host
# key, identity secret and backups stay in /var/lib/flapd.
set -euo pipefail

PURGE=0
YES=0
for a in "$@"; do
	case "$a" in
		--purge) PURGE=1 ;;
		--yes) YES=1 ;;
		-h|--help) sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "unknown argument $a" >&2; exit 2 ;;
	esac
done
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }

if systemctl list-unit-files flapd.service >/dev/null 2>&1; then
	systemctl disable --now flapd 2>/dev/null || true
fi
rm -f /etc/systemd/system/flapd.service
systemctl daemon-reload
rm -f /usr/local/bin/flapd /usr/local/bin/flapd.prev /usr/local/bin/flapd.new
rm -rf /usr/local/share/doc/flapd
echo "Removed the flapd service and binary."

if [ "$PURGE" -ne 1 ]; then
	echo "Kept /etc/flapd and /var/lib/flapd (database, host key, identity secret, backups)."
	echo "Run with --purge to delete them."
	exit 0
fi

if [ "$YES" -ne 1 ]; then
	echo "This permanently deletes the leaderboard, host key and backups in /var/lib/flapd."
	read -r -p "Type 'delete' to continue: " answer
	[ "$answer" = "delete" ] || { echo "Aborted; data kept."; exit 1; }
fi
rm -rf /var/lib/flapd /etc/flapd
if id -u flapd >/dev/null 2>&1; then
	userdel flapd || true
fi
echo "Purged flapd data, configuration and user."
