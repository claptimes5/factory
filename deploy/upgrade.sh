#!/bin/sh
# Safe self-upgrade: drain, back up the DB, swap the binary (keeping the old
# one), migrate, watch the next job, and roll back if it fails.
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

as_factory() { (cd "$DIR" && sudo -u "$USER_" --preserve-env "$@"); }

"$NEW" 2>&1 | grep -q usage || { echo "upgrade: $NEW does not run" >&2; exit 1; }

echo "== draining (running jobs get the unit's stop timeout)"
systemctl stop "$UNIT"

# Taken with the daemon stopped, so the backup is exactly the state the
# old binary left. Read the last job id now too, for the same reason.
DB=$(as_factory "$BIN" db-path)
BACKUP="$(dirname "$DB")/backups/pre-upgrade-$STAMP.db"
echo "== backing up the database to $BACKUP"
as_factory mkdir -p "$(dirname "$BACKUP")"
as_factory "$BIN" backup "$BACKUP"
LAST=$(as_factory "$BIN" last-job)

echo "== swapping the binary (old one kept as factory.old)"
cp -p "$BIN" "$BIN.old"
install -m 0755 "$NEW" "$BIN"

# restore_db is only for a failed migration: after the new binary has run
# jobs, restoring the backup would forget work it did on GitHub and the
# spend it recorded, so a later rollback keeps the database.
rollback() {
	set +e
	echo "== rolling back: $1" >&2
	systemctl stop "$UNIT"
	install -m 0755 "$BIN.old" "$BIN" || echo "!! could not restore $BIN from $BIN.old" >&2
	if [ "${2:-}" = restore_db ]; then
		as_factory cp "$BACKUP" "$DB" && rm -f "$DB-wal" "$DB-shm"
	fi
	systemctl start "$UNIT" || echo "!! $UNIT did not start; check journalctl -u $UNIT" >&2
	exit 1
}

echo "== migrating"
as_factory "$BIN" migrate || rollback "migration failed" restore_db

echo "== starting and watching the next job (up to $WATCH)"
systemctl start "$UNIT" || rollback "the new binary did not start"
as_factory "$BIN" wait-job "$LAST" "$WATCH" || rollback "the first job after the upgrade failed before doing any work"
systemctl is-active --quiet "$UNIT" || rollback "the daemon is not running"
echo "== upgraded; previous binary at $BIN.old, database backup at $BACKUP"
