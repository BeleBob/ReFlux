#!/bin/sh
# ReFlux installer: puts the reflux manager of a release into ~/.local/bin,
# checked against the release's SHA256SUMS, and starts its setup wizard.
#
#   curl -fsSL https://github.com/BeleBob/ReFlux/releases/latest/download/install.sh | sh
#
# Run it as the user who will manage ReFlux, not as root: reflux asks for
# sudo where it needs it. Environment:
#   REFLUX_VERSION   a release to install (1.0.0) instead of the latest
#   REFLUX_REPO      the GitHub repository (BeleBob/ReFlux)
#   REFLUX_NO_SETUP  1: install only, do not start the wizard
#   REFLUX_BASE      where release files are fetched from (tests)
set -eu

repo=${REFLUX_REPO:-BeleBob/ReFlux}
bin_dir=$HOME/.local/bin

say() { printf '%s\n' "$*"; }
fail() { printf 'reflux install: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -ne 0 ] || fail "run this as the user who will manage ReFlux, not as root (reflux uses sudo itself)"
[ "$(uname -s)" = Linux ] || fail "ReFlux servers run on Linux"
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "no reflux build for $(uname -m)" ;;
esac
command -v curl >/dev/null 2>&1 || fail "curl is needed: sudo apt-get install curl"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is needed (coreutils)"

if [ -n "${REFLUX_BASE:-}" ]; then
	base=$REFLUX_BASE
elif [ -n "${REFLUX_VERSION:-}" ]; then
	base=https://github.com/$repo/releases/download/reflux-v${REFLUX_VERSION#v}
else
	base=https://github.com/$repo/releases/latest/download
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
say "Downloading reflux for linux/$arch from $base"
curl -fsSL -o "$tmp/reflux-linux-$arch" "$base/reflux-linux-$arch" || fail "download failed: $base/reflux-linux-$arch"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || fail "download failed: $base/SHA256SUMS"
(cd "$tmp" && grep " reflux-linux-$arch\$" SHA256SUMS | sha256sum -c -) >/dev/null ||
	fail "reflux-linux-$arch does not match SHA256SUMS: not installed"

mkdir -p "$bin_dir"
install -m 0755 "$tmp/reflux-linux-$arch" "$bin_dir/reflux.new"
mv "$bin_dir/reflux.new" "$bin_dir/reflux"
say "Installed $("$bin_dir/reflux" version | head -n 1) to $bin_dir/reflux"

case ":$PATH:" in
*":$bin_dir:"*) ;;
*)
	if ! grep -qs '\.local/bin' "$HOME/.profile"; then
		printf '\n# reflux\nexport PATH="$HOME/.local/bin:$PATH"\n' >>"$HOME/.profile"
		say "Added ~/.local/bin to PATH in ~/.profile (new logins); for now: export PATH=\"\$HOME/.local/bin:\$PATH\""
	fi
	;;
esac

[ "${REFLUX_NO_SETUP:-}" = 1 ] && exit 0
say ""
# The wizard asks questions: give it the terminal even when this script
# came through a pipe.
if [ -r /dev/tty ]; then
	exec "$bin_dir/reflux" setup </dev/tty
fi
say "No terminal to ask questions on: run  reflux setup  yourself."
