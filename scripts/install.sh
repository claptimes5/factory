#!/bin/sh
# Checks what the factory needs on this machine, then builds ./factory.
#
#   sh scripts/install.sh          check Go, git and Docker, then build
#   sh scripts/install.sh --web    also check Node, to rebuild the dashboard
#
# For anything missing it prints the command that installs it and, in a
# terminal, asks before running it; say no to install it your own way
# (asdf, mise, your package manager). It runs nothing without a yes.
# Once built, ./factory doctor checks the rest.
# shellcheck disable=SC2016 # fix lines print $PATH and $USER as typed
set -u

web=0
for a in "$@"; do
	case $a in
	--web) web=1 ;;
	-h | --help) sed -n '2,10s/^# \{0,1\}//p' "$0"; exit 0 ;;
	*) echo "install.sh: unknown option $a (try --help)" >&2; exit 2 ;;
	esac
done
self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0") # to run it again after an install
cd "$(dirname "$0")/.." || exit 1

failed=0  # any ✗
nobuild=0 # a ✗ that stops the build
pass() { printf '  ✓ %s\n' "$1"; }
warn() { printf '  ! %s\n' "$1"; shift; fixes "$@"; }
bad() { printf '  ✗ %s\n' "$1"; shift; fixes "$@"; failed=1; }
fixes() { for l in "$@"; do printf '      %s\n' "$l"; done; }

# offer CMD: in a terminal, ask to run CMD, the fix just printed. A yes
# runs it and the checks run again once all are done.
ran=0
offer() {
	[ -n "$1" ] && [ -t 0 ] && [ -t 1 ] || return 0
	printf '      Run it now? [y/N] '
	read -r a || return 0
	case $a in
	[yY]*)
		printf '      $ %s\n' "$1"
		if sh -c "$1"; then ran=1; else echo "      That didn't work; run it yourself, or install it your own way."; fi ;;
	esac
}

# The version number in a tool's output: 27.5.1 in
# "Docker version 27.5.1, build 9f9e405", 1.26.3 in "go1.26.3".
ver() { printf '%s\n' "$1" | grep -Eo '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -n 1; }

# at_least VERSION MAJOR MINOR; false if VERSION isn't one.
at_least() {
	case ${1:-} in [0-9]*.[0-9]*) ;; *) return 1 ;; esac
	maj=${1%%.*}
	rest=${1#*.}
	min=${rest%%.*}
	[ "$maj" -gt "$2" ] || { [ "$maj" -eq "$2" ] && [ "$min" -ge "$3" ]; }
}

# Where we are, for the fix commands.
family=
wsl=0
if [ "$(uname -s)" = Darwin ]; then
	family=mac
elif [ -r "${INSTALL_OS_RELEASE:=/etc/os-release}" ]; then # tests point it elsewhere
	# shellcheck disable=SC1090
	ids=$(. "$INSTALL_OS_RELEASE" && echo " ${ID:-} ${ID_LIKE:-} ")
	case $ids in
	*" debian "* | *" ubuntu "*) family=debian ;;
	*" fedora "* | *" rhel "* | *" centos "*) family=fedora ;;
	*" arch "*) family=arch ;;
	esac
fi
grep -qi microsoft /proc/version 2>/dev/null && wsl=1

# pkgcmd NAME is the command that installs NAME here, "" if we don't know.
pkgcmd() {
	case $family in
	debian) echo "sudo apt install $1" ;;
	fedora) echo "sudo dnf install $1" ;;
	arch) echo "sudo pacman -S $1" ;;
	mac) echo "brew install $1" ;;
	esac
}
pkg() { c=$(pkgcmd "$1"); echo "${c:-install $1 with your system's package manager}"; }
getdocker='curl -fsSL https://get.docker.com | sh   (Docker'"'"'s own packages; a distro'"'"'s may be older than 28)'

echo "Checking what the factory needs:"
if [ "$family" = mac ]; then
	warn "macOS: this builds the binary, but jobs need a Linux host or WSL2 (docs/setup.md#dependencies)"
fi

# Go: go.mod asks for the version on its go line, and Go 1.21 or later
# fetches the one on its toolchain line by itself, which needs internet
# the first time.
want=$(sed -n 's/^go \([0-9.]*\).*/\1/p' go.mod)
tool=$(sed -n 's/^toolchain go\([0-9.]*\).*/\1/p' go.mod)
tool=${tool:-$want}
case $(uname -m) in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
armv6l | armv7l) arch=armv6l ;;
i386 | i686) arch=386 ;;
*) arch=$(uname -m) ;;
esac
# go_bad MESSAGE [old]: a ✗ for Go, with how to install it ("old" when an
# older Go is already there).
go_bad() {
	case $family in
	arch) bad "$1" "sudo pacman -Syu go"; offer "sudo pacman -Syu go" ;;
	mac) if [ "${2:-}" = old ]; then bad "$1" "brew upgrade go"; offer "brew upgrade go"; else bad "$1" "brew install go"; offer "brew install go"; fi ;;
	*)
		rm=
		if [ "${2:-}" = old ]; then
			case $family in
			debian) rm="and remove the distro's older Go if it came from apt: sudo apt remove golang-go" ;;
			fedora) rm="and remove the distro's older Go if it came from dnf: sudo dnf remove golang" ;;
			esac
		fi
		gocmd="sudo rm -rf /usr/local/go && curl -fsSL https://go.dev/dl/go$tool.linux-$arch.tar.gz | sudo tar -C /usr/local -xz"
		bad "$1" "$gocmd" \
			'export PATH=/usr/local/go/bin:$PATH   (first, so it wins over any other Go; add it to ~/.profile too)' \
			${rm:+"$rm"} "(more at https://go.dev/doc/install)"
		offer "$gocmd" ;;
	esac
	nobuild=1
}
if ! command -v go >/dev/null 2>&1; then
	if [ -x /usr/local/go/bin/go ]; then
		bad "Go is in /usr/local/go but not on your PATH" 'export PATH=/usr/local/go/bin:$PATH   (and add it to ~/.profile)'
		nobuild=1
	else
		go_bad "Go isn't installed (the factory needs $want or later)"
	fi
else
	mine=$(ver "$(GOTOOLCHAIN=local go env GOVERSION 2>/dev/null)")
	wantmin=$(echo "$want" | cut -d. -f2)
	if ! at_least "$mine" 1 21; then
		# Too old to fetch a newer toolchain.
		go_bad "Go ${mine:-?} is too old: the factory needs $want or later" old
	elif ! out=$(go env GOVERSION 2>&1); then
		go_bad "Go $mine couldn't fetch Go $tool, which this build uses (offline?): $(printf '%s\n' "$out" | head -n 1)" old
	elif ! at_least "$(ver "$out")" "${want%%.*}" "$wantmin"; then
		# Go fetches the toolchain itself unless GOTOOLCHAIN says not to.
		gt=$(go env GOTOOLCHAIN 2>/dev/null)
		case $gt in
		local*) bad "Go $(ver "$out") is older than the $want the factory needs, and GOTOOLCHAIN=$gt stops Go fetching $tool" \
			"unset GOTOOLCHAIN (and go env -u GOTOOLCHAIN if go env -w set it), or install Go $tool"
			nobuild=1 ;;
		*) go_bad "Go $(ver "$out") is too old: the factory needs $want or later" old ;;
		esac
	elif [ "$(ver "$out")" != "$mine" ]; then
		pass "Go $(ver "$out") (fetched by your Go $mine)"
	else
		pass "Go $mine"
	fi
fi

if command -v git >/dev/null 2>&1; then
	pass "git $(ver "$(git --version)")"
else
	bad "git isn't installed" "$(pkg git)"
	offer "$(pkgcmd git)"
	nobuild=1
fi

if ! command -v docker >/dev/null 2>&1; then
	if [ $wsl = 1 ]; then
		bad "Docker isn't installed" "install Docker Desktop for Windows and turn on Settings > Resources > WSL integration for this distro," "or install Docker inside WSL: $getdocker" "(docs/setup.md#windows-wsl2)"
	elif [ "$family" = mac ]; then
		bad "Docker isn't installed" "brew install --cask docker-desktop"
		offer "brew install --cask docker-desktop"
	elif [ "$family" = arch ]; then
		bad "Docker isn't installed" "sudo pacman -S docker && sudo systemctl enable --now docker"
		offer "sudo pacman -S docker && sudo systemctl enable --now docker"
	else
		bad "Docker isn't installed" "$getdocker" "then: sudo systemctl enable --now docker"
		offer "curl -fsSL https://get.docker.com | sh && sudo systemctl enable --now docker"
	fi
elif ! out=$(docker version --format '{{.Server.Version}}' 2>&1); then
	case $out in
	*[Pp]"ermission denied"*)
		bad "Docker refuses you: permission denied on its socket" 'sudo usermod -aG docker $USER && newgrp docker' "(or log out and back in; on WSL, close and reopen the terminal)" ;;
	*)
		if [ $wsl = 1 ]; then
			bad "Docker isn't running" "start Docker Desktop, with WSL integration on for this distro (docs/setup.md#windows-wsl2)," "or, for Docker installed inside WSL: sudo service docker start"
		elif [ "$family" = mac ]; then
			bad "Docker isn't running" "open -a Docker"
		else
			bad "Docker isn't running" "sudo systemctl start docker"
		fi ;;
	esac
else
	v=$(ver "$out")
	if [ -n "$v" ] && ! at_least "$v" 28 0; then
		warn "Docker $v: the egress allowlist and services need 28 or later" "upgrade: $getdocker" "or, without services, set runner.egress.mode: open in factory.yaml (jobs then reach the whole internet)"
	else
		pass "Docker ${v:-works}"
	fi
fi

if [ $web = 1 ]; then
	if ! command -v node >/dev/null 2>&1; then
		nodefix="nvm install 22   (https://github.com/nvm-sh/nvm; apt's nodejs is too old)"
		[ "$family" = debian ] || nodefix="$(pkg nodejs)   (or: nvm install 22)"
		bad "Node isn't installed (the dashboard needs 22.12 or later)" "$nodefix"
	elif v=$(ver "$(node --version)") && at_least "$v" 22 12; then
		pass "Node $v (rebuild the dashboard with: cd web && npm ci && npm run build)"
	else
		bad "Node ${v:-?} is too old: the dashboard needs 22.12 or later" "nvm install 22   (https://github.com/nvm-sh/nvm), or https://nodejs.org/en/download"
	fi
fi

echo
if [ $ran = 1 ]; then
	echo "Checking again with what was just installed:"
	echo
	PATH=/usr/local/go/bin:$PATH exec sh "$self" "$@"
fi
if [ $nobuild = 1 ]; then
	echo "Fix the ✗ lines above, then run sh scripts/install.sh again."
	exit 1
fi
if [ "${INSTALL_NO_BUILD:-}" = 1 ]; then # for tests
	exit $failed
fi
echo "Building ./factory ..."
CGO_ENABLED=0 go build -o factory ./cmd/factory || exit 1
if [ $failed = 1 ]; then
	echo "Built ./factory, but fix the ✗ lines above first."
	echo "Then ./factory doctor checks your whole setup, and ./factory setup starts it."
	exit 1
fi
echo "Built ./factory. Next:"
echo
echo "    ./factory setup"
echo
echo "Something not working later? ./factory doctor checks your whole setup."
