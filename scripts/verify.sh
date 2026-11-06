#!/bin/sh
# verify.sh runs what CI runs, in one command, and stops at the first
# failure: gofmt, the tests of the Claude Code hooks and the PR
# description check, that the auto-merge breaker model is in step with
# its code, go vet, the pinned golangci-lint, go build,
# go test -race, then the dashboard's npm ci, build (types, styles),
# stale-build check and Playwright. Humans, agents and CI all run this,
# so "it passed" means the same thing everywhere.
#
# It first checks for every tool it needs and names all that are missing
# at once. The ones this repo pins (golangci-lint, Playwright's Chromium)
# it installs itself.
#
# --quick is the inner loop: the Go checks without -race, and the
# dashboard's type check when its packages are installed. It is not a
# substitute for the full run before a pull request.
#
# --part lint|go|web runs one third of the full run: everything up to
# go build; go test -race; or the dashboard. CI runs the three side by
# side, so it checks exactly what verify does.
set -eu
cd "$(dirname "$0")/.."

quick=false
part=all
case "${1:-}" in
"") ;;
--quick) quick=true ;;
--part)
	part=${2:-}
	case "$part" in lint | go | web) ;; *) part="" ;; esac
	;;
*) part="" ;;
esac
if [ -z "$part" ]; then
	echo "usage: scripts/verify.sh [--quick | --part lint|go|web]" >&2
	exit 2
fi
runs() { [ "$part" = all ] || [ "$part" = "$1" ]; }

step() { printf '\n== %s\n' "$*"; }

# On WSL a Windows npm can run even when `which npm` names a Linux one
# (a version manager's shim hands off to the next npm on the PATH). It
# installs a node_modules the build can't use and fails with errors that
# don't mention npm, so catch it first. asdf's shim is what hands off to
# the Windows npm, so when asdf has a Linux Node, use it: hooks and other
# programs that start verify don't get the PATH a shell profile sets.
linux_npm() {
	case "$(npm config get prefix 2>/dev/null)" in
	[A-Za-z]:\\*)
		prefix=$(npm config get prefix)
		if dir=$(asdf where nodejs 2>/dev/null) && [ -x "$dir/bin/npm" ]; then
			PATH="$dir/bin:$PATH"
			export PATH
			printf "npm here was the Windows one (%s); using asdf's %s instead.\n" "$prefix" "$dir" >&2
			return
		fi
		printf "npm here is the Windows one (%s). Put a Linux Node's bin directory first on the PATH, then rerun; verify reinstalls web/node_modules.\n" "$prefix" >&2
		exit 1
		;;
	esac
}

version() { "$1" --version 2>/dev/null | sed -n 's/.*version \([0-9.]*\).*/\1/p'; }

step tools
# A new machine should learn everything it lacks in one run, not one
# failure per run. Only what needs a system install is left to a person.
web=false
if runs web && ! $quick || { $quick && [ -d web/node_modules ]; }; then web=true; fi
missing=""
need() { command -v "$1" >/dev/null 2>&1 || missing="$missing
  $1: $2"; }
need git "https://git-scm.com/downloads"
need go "https://go.dev/dl/ (any recent Go; go.mod fetches the toolchain it names)"
need python3 "Python 3, for the Claude Code hooks' tests"
engines=$(sed -n 's/.*"node": *"\(.*\)".*/\1/p' web/package.json)
if $web; then need npm "Node $engines, https://nodejs.org"; fi
if [ -n "$missing" ]; then
	printf 'verify needs these installed first, then rerun:%s\n' "$missing" >&2
	exit 1
fi
if $web; then
	linux_npm
	if ! node -e '
		const want = process.argv[1].replace(/^>=/, "").split(".").map(Number);
		const have = process.versions.node.split(".").map(Number);
		for (let i = 0; i < 3; i++) if (have[i] !== want[i]) process.exit(have[i] > want[i] ? 0 : 1);
	' "$engines"; then
		printf 'Node here is %s, but the dashboard needs %s: https://nodejs.org\n' "$(node --version)" "$engines" >&2
		exit 1
	fi
fi
# The version CI pins, so a local pass means a CI pass. A version manager
# may not put go install's directory on the PATH, so use it directly.
want=$(sed -n 's/^ *version: v//p' .github/workflows/ci.yml | head -n1)
lint=golangci-lint
if runs lint && [ "$(version golangci-lint)" != "$want" ]; then
	bin=$(go env GOBIN)
	[ -n "$bin" ] || bin=$(go env GOPATH)/bin
	lint=$bin/golangci-lint
	if [ "$(version "$lint")" != "$want" ]; then
		echo "Installing golangci-lint v$want, the version CI runs, into $bin."
		go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$want"
	fi
fi
if runs go && ! docker info >/dev/null 2>&1; then
	echo "Docker isn't running here, so the tests that need it will be skipped."
fi
echo "All there."

if runs lint; then
	step gofmt
	# New files too, not only tracked ones: a file not yet added is still in
	# the commit about to be made.
	unformatted=$(git ls-files --cached --others --exclude-standard '*.go' | xargs gofmt -l)
	if [ -n "$unformatted" ]; then
		echo "These files need gofmt -w:" >&2
		echo "$unformatted" >&2
		exit 1
	fi

	step claude hooks
	# The hooks in .claude/ enforce AGENTS.md for interactive agents; a broken
	# one fails open or blocks everything, so its tests run every time.
	python3 .claude/hooks/guard_test.py

	step pull request description check
	python3 scripts/check_pr_body_test.py

	step workflow actions pinned
	# A tag can be moved to other code; a commit can't.
	unpinned=$(grep -n 'uses:' .github/workflows/*.yml | grep -v '@[0-9a-f]\{40\}' || true)
	if [ -n "$unpinned" ]; then
		echo "Pin these actions to a full commit SHA (keep the tag as a comment):" >&2
		echo "$unpinned" >&2
		exit 1
	fi

	step auto-merge breaker model in step with the code
	# The model was checked against these functions as they were; a change
	# to them needs someone to check the model still fits (its SKILL.md).
	.claude/skills/verify-breaker/check.sh --sync

	step go vet
	go vet ./...

	step golangci-lint
	"$lint" run ./...

	step go build
	go build ./...
fi

if $quick; then
	step go test
	go test ./...
	if [ -d web/node_modules ]; then
		step dashboard type check
		(cd web && npm run --silent typecheck)
	else
		printf '\nSkipped the dashboard type check: web/node_modules is missing (run the full verify once).\n'
	fi
	printf '\nverify --quick passed.\n'
	exit 0
fi

if runs go; then
	step go test -race
	go test -race ./...
fi

if runs web; then
	step npm ci
	(cd web && npm ci --no-audit --no-fund)

	step dashboard build, and the committed build matches it
	scripts/check-web-built.sh

	step playwright
	# The browser at the version package.json pins; a no-op once it's there.
	(cd web && npx playwright install chromium && npm run e2e)
fi

if [ "$part" = all ]; then
	printf '\nverify passed.\n'
else
	printf '\nverify --part %s passed.\n' "$part"
fi
