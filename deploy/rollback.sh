#!/usr/bin/env bash
# Roll back to the binary that was installed before the last upgrade.
#
#   sudo ./deploy/rollback.sh
#
# The previous binary is kept as /usr/local/bin/flapd.prev by install.sh. If
# the upgrade migrated the database to a newer schema, the old binary refuses
# to start; restore the backup taken before the upgrade (see
# docs/OPERATIONS.md, "Rollback").
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
[ -f /usr/local/bin/flapd.prev ] || { echo "no previous binary (/usr/local/bin/flapd.prev) to roll back to" >&2; exit 1; }

echo "current:  $(/usr/local/bin/flapd version 2>/dev/null || echo unknown)"
echo "previous: $(/usr/local/bin/flapd.prev version 2>/dev/null || echo unknown)"
mv -f /usr/local/bin/flapd /usr/local/bin/flapd.rolled-back
mv -f /usr/local/bin/flapd.prev /usr/local/bin/flapd
systemctl restart flapd
for _ in $(seq 1 30); do
	if runuser -u flapd -- /usr/local/bin/flapd healthcheck -config /etc/flapd/flapd.toml >/dev/null 2>&1; then
		echo "rolled back and healthy. The newer binary is kept as /usr/local/bin/flapd.rolled-back."
		exit 0
	fi
	sleep 1
done
journalctl -u flapd -n 30 --no-pager >&2 || true
echo "flapd is not healthy after the rollback; if the log mentions the schema, restore a backup from /var/lib/flapd/backups" >&2
exit 1
