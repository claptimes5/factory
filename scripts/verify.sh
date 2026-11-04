#!/bin/sh
# verify.sh runs what CI runs, in one command, and stops at the first
# failure: gofmt, go vet, the pinned golangci-lint, go build, go test -race,
# then the dashboard's npm ci, build (types, styles), stale-build check and
# Playwright. Humans, agents and CI all run this, so "it passed" means the
# same thing everywhere.
#
# --quick is the inner loop: the Go checks without -race, and the
# dashboard's type check when its packages are installed. It is not a
# substitute for the full run before a pull request.
set -eu
cd "$(dirname "$0")/.."

quick=false
[ "${1:-}" = "--quick" ] && quick=true

step() { printf '\n== %s\n' "$*"; }

# On WSL a Windows npm can run even when `which npm` names a Linux one
# (a version manager's shim hands off to the next npm on the PATH). It
# installs a node_modules the build can't use and fails with errors that
# don't mention npm, so catch it first.
linux_npm() {
	case "$(npm config get prefix 2>/dev/null)" in
	[A-Za-z]:\\*)
		printf "npm here is the Windows one (%s). Put a Linux Node's bin directory first on the PATH, then rerun; verify reinstalls web/node_modules.\n" "$(npm config get prefix)" >&2
		# asdf's shim is what hands off to the Windows npm, so name the
		# real directory behind it.
		if dir=$(asdf where nodejs 2>/dev/null); then
			printf 'Here: export PATH="%s/bin:$PATH"\n' "$dir" >&2
		fi
		exit 1
		;;
	esac
}

step gofmt
# New files too, not only tracked ones: a file not yet added is still in
# the commit about to be made.
unformatted=$(git ls-files --cached --others --exclude-standard '*.go' | xargs gofmt -l)
if [ -n "$unformatted" ]; then
	echo "These files need gofmt -w:" >&2
	echo "$unformatted" >&2
	exit 1
fi

step go vet
go vet ./...

step golangci-lint
# The version CI pins, so a local pass means a CI pass.
want=$(sed -n 's/^ *version: v//p' .github/workflows/ci.yml | head -n1)
if ! command -v golangci-lint >/dev/null; then
	echo "golangci-lint isn't installed: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$want" >&2
	exit 1
fi
have=$(golangci-lint --version | sed -n 's/.*version \([0-9.]*\).*/\1/p')
if [ "$have" != "$want" ]; then
	echo "golangci-lint is $have but CI runs $want: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$want" >&2
	exit 1
fi
golangci-lint run ./...

step go build
go build ./...

if $quick; then
	step go test
	go test ./...
	if [ -d web/node_modules ]; then
		step dashboard type check
		linux_npm
		(cd web && npm run --silent typecheck)
	else
		printf '\nSkipped the dashboard type check: web/node_modules is missing (run the full verify once).\n'
	fi
	printf '\nverify --quick passed.\n'
	exit 0
fi

step go test -race
go test -race ./...

step npm ci
linux_npm
(cd web && npm ci --no-audit --no-fund)

step dashboard build, and the committed build matches it
scripts/check-web-built.sh

step playwright
(cd web && npm run e2e)

printf '\nverify passed.\n'
