#!/bin/sh
# Installs n6kd from a GitHub release:
#   curl -fsSL https://n6k-io.github.io/n6kd/get.sh | sh
#
# N6KD_VERSION  release tag to install (default: the latest release)
# N6KD_HOME     where releases are unpacked (default: ~/.n6kd)
# N6KD_BIN_DIR  where the n6kd symlink goes (default: ~/.local/bin)

set -eu

repo="n6k-io/n6kd"
home="${N6KD_HOME:-$HOME/.n6kd}"
bin_dir="${N6KD_BIN_DIR:-$HOME/.local/bin}"

fail() {
	echo "get.sh: $*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

need curl
need tar
need uname

case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) fail "unsupported OS $(uname -s)" ;;
esac

case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported architecture $(uname -m)" ;;
esac

# A Rosetta shell reports x86_64 on Apple silicon.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
	arch=arm64
fi

version="${N6KD_VERSION:-}"
if [ -z "$version" ]; then
	# The redirect target names the tag without spending GitHub API rate limit.
	latest="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")" ||
		fail "could not resolve the latest release"
	version="${latest##*/}"
	case "$version" in
		"" | latest | releases) fail "no release published yet" ;;
	esac
fi

name="n6kd-$version-$os-$arch"
base="https://github.com/$repo/releases/download/$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $name"
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz" || fail "no $name.tar.gz in release $version"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || fail "no SHA256SUMS in release $version"

want="$(awk -v f="$name.tar.gz" '$2 == f { print $1 }' "$tmp/SHA256SUMS")"
[ -n "$want" ] || fail "$name.tar.gz is not listed in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
	got="$(sha256sum "$tmp/$name.tar.gz" | awk '{ print $1 }')"
else
	got="$(shasum -a 256 "$tmp/$name.tar.gz" | awk '{ print $1 }')"
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $name.tar.gz"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

# The binary finds its extensions beside its real path, so the unpacked
# directory stays whole and only a symlink goes on PATH.
mkdir -p "$home" "$bin_dir"
rm -rf "${home:?}/$version"
mv "$tmp/$name" "$home/$version"
ln -sf "$home/$version/n6kd" "$bin_dir/n6kd"

echo "Installed n6kd $version to $bin_dir/n6kd"

case ":$PATH:" in
	*":$bin_dir:"*) ;;
	*) echo "Add $bin_dir to your PATH: export PATH=\"$bin_dir:\$PATH\"" ;;
esac
