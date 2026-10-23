#!/bin/sh
# check-web-built.sh fails when the committed dashboard (internal/web/app)
# or its API types (web/src/api/schema.d.ts) differ from a fresh build of
# web/: what CI's "built app and types are up to date" step checks, for a
# tree with no git history, such as the factory's own gates. It leaves the
# fresh build in place. Run it after `cd web && npm ci`.
set -eu
cd "$(dirname "$0")/.."
was=$(mktemp -d)
trap 'rm -rf "$was"' EXIT
cp -R internal/web/app "$was/app"
cp web/src/api/schema.d.ts "$was/schema.d.ts"
(cd web && npm run build >/dev/null)
if ! diff -r "$was/app" internal/web/app >/dev/null || ! diff "$was/schema.d.ts" web/src/api/schema.d.ts >/dev/null; then
	echo "The committed dashboard build doesn't match web/: run 'cd web && npm run build' and commit internal/web/app and web/src/api." >&2
	diff -rq "$was/app" internal/web/app >&2 || true
	diff -q "$was/schema.d.ts" web/src/api/schema.d.ts >&2 || true
	exit 1
fi
echo "The committed dashboard build matches web/."
