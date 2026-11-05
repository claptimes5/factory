#!/bin/sh
# check.sh checks the auto-merge breaker model with TLC (see SKILL.md).
#
#   check.sh [Breaker|BreakerCrash|BreakerBroken]   model-check one config
#   check.sh --sync     fail if the Go the model stands for changed since
#                       someone last confirmed the model (covered.sha256)
#   check.sh --record   after confirming the model still describes the
#                       code, record the Go as it is now
#
# TLC is pinned: tla2tools.jar v1.7.4, by SHA-256, kept in ~/.cache/tla.
# It needs Java 11 or later.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)

version=1.7.4
sum=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88
jar=${TLA2TOOLS:-$HOME/.cache/tla/$version/tla2tools.jar}

# The functions the model stands for, as "file function". A Go function's
# body runs from its "func" line to the next line that is a bare "}".
covered="internal/pipeline/guard.go checkBreaker
internal/pipeline/guard.go guardLock
internal/pipeline/guard.go breakerTripped
internal/pipeline/guard.go handBack
internal/pipeline/guard.go recordMergeOn
internal/pipeline/guard.go startMerge
internal/pipeline/guard.go enableAt
internal/pipeline/guard.go resync
internal/pipeline/stages.go autoMerge
internal/pipeline/stages.go autoMerged
internal/pipeline/stages.go cancelAutoMerge
internal/store/guard.go TripBreaker"

hashes() {
	echo "$covered" | while read -r file fn; do
		body=$(sed -n "/^func ([a-z]* \*[A-Za-z]*) $fn(/,/^}/p" "$root/$file")
		if [ -z "$body" ]; then
			echo "check.sh: $fn not found in $file; update the covered list and the model" >&2
			exit 1
		fi
		printf '%s  %s %s\n' "$(printf '%s\n' "$body" | sha256sum | cut -d' ' -f1)" "$file" "$fn"
	done
}

case "${1:-Breaker}" in
--sync)
	if ! hashes | diff -u "$here/covered.sha256" - >/dev/null; then
		echo "The Go the auto-merge breaker model stands for changed:" >&2
		hashes | diff "$here/covered.sha256" - | sed -n 's/^> /  /p' >&2
		echo "Check the model still describes it (.claude/skills/verify-breaker/SKILL.md), run" >&2
		echo ".claude/skills/verify-breaker/check.sh, then .claude/skills/verify-breaker/check.sh --record." >&2
		exit 1
	fi
	exit 0
	;;
--record)
	hashes >"$here/covered.sha256"
	echo "Recorded $(wc -l <"$here/covered.sha256") functions at $(git -C "$root" rev-parse --short HEAD)."
	exit 0
	;;
Breaker | BreakerCrash | BreakerBroken) cfg=$1 ;;
*)
	echo "usage: check.sh [Breaker|BreakerCrash|BreakerBroken] | --sync | --record" >&2
	exit 2
	;;
esac

if [ ! -f "$jar" ]; then
	mkdir -p "$(dirname "$jar")"
	curl -fsSL -o "$jar.part" "https://github.com/tlaplus/tlaplus/releases/download/v$version/tla2tools.jar"
	mv "$jar.part" "$jar"
fi
if [ "$(sha256sum "$jar" | cut -d' ' -f1)" != "$sum" ]; then
	echo "check.sh: $jar isn't tla2tools.jar v$version (SHA-256 differs). Delete it and rerun." >&2
	exit 1
fi
meta=$(mktemp -d)
trap 'rm -r "$meta"' EXIT
cd "$here"
java -XX:+UseParallelGC -cp "$jar" tlc2.TLC -config "$cfg.cfg" -metadir "$meta" -workers auto -cleanup Breaker.tla
