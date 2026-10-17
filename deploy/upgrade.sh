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

# Whatever stops this script before the new binary is running, the
# daemon must not be left stopped.
STARTED=0
on_exit() { [ "$STARTED" = 1 ] || systemctl start "$UNIT" || echo "!! $UNIT did not start; check journalctl -u $UNIT" >&2; }
trap on_exit EXIT

echo "== draining (running jobs get the unit's stop timeout)"
systemctl stop "$UNIT"

# Taken with the daemon stopped, so the backup is exactly the state the
# old binary left. Read the last job id and schema version now too, for
# the same reason. (Binaries before db-path keep the database here.)
DB=$(as_factory "$BIN" db-path 2>/dev/null) || DB="$DIR/data/factory.db"
OLD_SCHEMA=$(as_factory "$BIN" migrate | awk '{print $NF}')
BACKUP="$(dirname "$DB")/backups/pre-upgrade-$STAMP.db"
echo "== backing up the database to $BACKUP"
as_factory mkdir -p "$(dirname "$BACKUP")"
as_factory "$BIN" backup "$BACKUP"
LAST=$(as_factory "$BIN" last-job)

echo "== swapping the binary (old one kept as factory.old)"
cp -p "$BIN" "$BIN.old"
install -m 0755 "$NEW" "$BIN" || { install -m 0755 "$BIN.old" "$BIN"; exit 1; }

# The old binary refuses a database migrated past it, so a rollback after a
# schema change must restore the backup. That forgets what the new binary
# recorded since; the startup GitHub catch-up re-reads labels and PRs.
# Without a schema change the database is kept.
NEW_SCHEMA=$OLD_SCHEMA
rollback() {
	set +e
	echo "== rolling back: $1" >&2
	systemctl stop "$UNIT"
	install -m 0755 "$BIN.old" "$BIN" || echo "!! could not restore $BIN from $BIN.old" >&2
	if [ "${2:-}" = restore_db ] || [ "$NEW_SCHEMA" != "$OLD_SCHEMA" ]; then
		echo "== restoring the database from $BACKUP" >&2
		as_factory cp "$BACKUP" "$DB" && rm -f "$DB-wal" "$DB-shm"
	fi
	STARTED=1
	systemctl start "$UNIT" || echo "!! $UNIT did not start; check journalctl -u $UNIT" >&2
	exit 1
}

echo "== migrating"
NEW_SCHEMA=$(as_factory "$BIN" migrate | awk '{print $NF}') || rollback "migration failed" restore_db
[ -n "$NEW_SCHEMA" ] || rollback "migration failed" restore_db

echo "== starting and watching the next job (up to $WATCH)"
systemctl start "$UNIT" || rollback "the new binary did not start"
STARTED=1
as_factory "$BIN" wait-job "$LAST" "$WATCH" || rollback "the first job after the upgrade failed before doing any work"
systemctl is-active --quiet "$UNIT" || rollback "the daemon is not running"
echo "== upgraded; previous binary at $BIN.old, database backup at $BACKUP"
