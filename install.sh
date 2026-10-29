#!/bin/sh
# SessionMemory installer: downloads the program for this computer, checks it,
# puts it in ~/.local/bin and runs `session-memory setup`.
#   curl -fsSL https://raw.githubusercontent.com/ThisIsTiger/SessionMemory/main/install.sh | sh
# SM_BASE_URL, SM_BIN_DIR and SM_NO_SETUP exist for testing. SM_REPO overrides the GitHub repository; SM_VERSION pins a release (default: latest).
set -eu

REPO="${SM_REPO:-ThisIsTiger/SessionMemory}"
VERSION="${SM_VERSION:-latest}"
DEST="${SM_BIN_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "This installer supports macOS and Linux only." >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) echo "Unsupported processor: $(uname -m)" >&2; exit 1 ;;
esac

if [ -n "${SM_BASE_URL:-}" ]; then
  base="$SM_BASE_URL"
elif [ "$VERSION" = latest ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi
name="session-memory-$os-$arch"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
echo "Downloading $name ..."
curl -fsSL "$base/$name" -o "$tmp/$name"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

want="$(grep " $name\$" "$tmp/checksums.txt" | awk '{print $1}')"
if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$tmp/$name" | awk '{print $1}')"; else got="$(shasum -a 256 "$tmp/$name" | awk '{print $1}')"; fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "The download did not match its checksum; nothing was installed." >&2
  exit 1
fi

mkdir -p "$DEST"
mv "$tmp/$name" "$DEST/session-memory"
chmod 755 "$DEST/session-memory"
if [ "$os" = darwin ]; then xattr -d com.apple.quarantine "$DEST/session-memory" 2>/dev/null || true; fi
echo "Installed $DEST/session-memory"

[ -n "${SM_NO_SETUP:-}" ] && exit 0
exec "$DEST/session-memory" setup
