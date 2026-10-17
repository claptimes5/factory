#!/bin/sh
# Safe self-upgrade: back up the DB, drain, swap the binary (keeping the old
# one), migrate, watch one job, and roll back if it fails.
#
#   sudo deploy/upgrade.sh /path/to/new/factory
#
# Assumes the layout of deploy/factory.service: /opt/factory/factory, run
# as the factory user from /opt/factory, unit name "factory".
set -eu

NEW=${1:?usage: upgrade.sh NEW_BINARY}
DIR=${FACTORY_DIR:-/opt/factory}
UNIT=${FACTORY_UNIT:-factory}
USER_=${FACTORY_USER:-factory}
WATCH=${FACTORY_WATCH:-45m}
BIN="$DIR/factory"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP="$DIR/data/backups/pre-upgrade-$STAMP.db"

as_factory() { (cd "$DIR" && sudo -u "$USER_" --preserve-env "$@"); }

"$NEW" 2>&1 | grep -q usage || { echo "upgrade: $NEW does not run" >&2; exit 1; }

echo "== backing up the database to $BACKUP"
as_factory mkdir -p "$DIR/data/backups"
as_factory "$BIN" backup "$BACKUP"
LAST=$(as_factory "$BIN" last-job)

echo "== draining (running jobs get the unit's stop timeout)"
systemctl stop "$UNIT"

echo "== swapping the binary (old one kept as factory.old)"
cp -p "$BIN" "$BIN.old"
install -m 0755 "$NEW" "$BIN"

rollback() {
	echo "== rolling back: $1" >&2
	systemctl stop "$UNIT" || true
	install -m 0755 "$BIN.old" "$BIN"
	as_factory cp "$BACKUP" "$DIR/data/factory.db"
	rm -f "$DIR/data/factory.db-wal" "$DIR/data/factory.db-shm"
	systemctl start "$UNIT"
	exit 1
}

echo "== migrating"
as_factory "$BIN" migrate || rollback "migration failed"

echo "== starting and watching one job (up to $WATCH)"
systemctl start "$UNIT"
if ! as_factory "$BIN" wait-job "$LAST" "$WATCH"; then
	rollback "the first job after the upgrade failed"
fi
systemctl is-active --quiet "$UNIT" || rollback "the daemon is not running"
echo "== upgraded; previous binary at $BIN.old, database backup at $BACKUP"
