#!/bin/sh
# Install the prosa binary from the GitHub releases, without cloning the repo.
#
#   curl -fsSL https://raw.githubusercontent.com/giammbo/prosa/main/install.sh | sh
#
# Options (environment variables):
#   PROSA_VERSION      release to install, e.g. v0.1.0 (default: the latest release)
#   PROSA_INSTALL_DIR  where to put the binary (default: /usr/local/bin if writable, else ~/.local/bin)
set -eu

repo="giammbo/prosa"

die() { echo "install: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
need curl
need tar
need uname

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s): releases exist for linux and darwin" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "unsupported architecture $(uname -m): releases exist for amd64 and arm64" ;;
esac

tag="${PROSA_VERSION:-}"
if [ -z "$tag" ]; then
  # The latest release redirects to /releases/tag/<tag>.
  tag="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")"
  tag="${tag##*/}"
  [ -n "$tag" ] && [ "$tag" != "latest" ] || die "cannot find the latest release of $repo"
fi
version="${tag#v}"
archive="prosa_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$tag"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading $archive ($tag)..."
curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "cannot download $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "cannot download $base/checksums.txt"

want="$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$archive" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)"
fi
[ "$got" = "$want" ] || die "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" prosa

dir="${PROSA_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/prosa" "$dir/prosa" 2>/dev/null || { cp "$tmp/prosa" "$dir/prosa" && chmod 0755 "$dir/prosa"; }

echo "Installed $("$dir/prosa" version) to $dir/prosa"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH to run prosa." ;;
esac
