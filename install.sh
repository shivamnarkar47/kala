#!/bin/sh
# kaal installer — fetches the prebuilt release binary for this platform
# from GitHub Releases (the same `kaal-<os>-<arch>` assets the release
# workflow builds and `kaal update`'s release-fetch path looks up).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/shivamnarkar47/kaal/main/install.sh | sh
#   # development branch (before the merge to main):
#   curl -fsSL https://raw.githubusercontent.com/shivamnarkar47/kaal/docs/go-migration-plan/install.sh | sh
#
# Overrides:
#   KAAL_VERSION  release tag to fetch (default: latest)
#   INSTALL_DIR   install directory   (default: ~/.local/bin)
set -eu

repo="shivamnarkar47/kaal"
version="${KAAL_VERSION:-latest}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) echo "kaal: unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
	linux | darwin) ;;
	*) echo "kaal: unsupported OS: $os — use install.ps1 on Windows" >&2; exit 1 ;;
esac

if [ "$version" = "latest" ]; then
	url="https://github.com/$repo/releases/latest/download/kaal-$os-$arch"
else
	url="https://github.com/$repo/releases/download/$version/kaal-$os-$arch"
fi

if ! command -v curl >/dev/null 2>&1; then
	echo "kaal: curl is required to install — install curl and retry" >&2
	exit 1
fi

dest="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dest"
tmp="$dest/.kaal-install.$$"
trap 'rm -f "$tmp"' EXIT

echo "kaal: fetching $url"
curl -fsSL "$url" -o "$tmp"
chmod +x "$tmp"
# The downloaded file must answer --version like a kaal before we install it.
if ! "$tmp" --version >/dev/null 2>&1; then
	echo "kaal: downloaded binary failed a --version probe — is the release published?" >&2
	exit 1
fi
mv "$tmp" "$dest/kaal"

echo "kaal installed at $dest/kaal"
case ":$PATH:" in
	*":$dest:"*) ;;
	*)
		echo "Add $dest to your PATH (in ~/.profile or ~/.bashrc):"
		echo "  export PATH=\"$dest:\$PATH\""
		;;
esac
echo "Run 'kaal' to start; 'kaal update' self-updates the binary."
