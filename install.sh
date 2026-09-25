#!/usr/bin/env bash
# Build pcx and install it to ~/.local/bin.
# Override with PREFIX=/usr/local or BINDIR=/custom/bin.
set -euo pipefail

root="$(cd "$(dirname "$0")" && pwd)"
bindir="${BINDIR:-${PREFIX:-$HOME/.local}/bin}"
out="$root/pcx"

if ! command -v go >/dev/null; then
	echo "go is required to build pcx" >&2
	exit 1
fi

echo "building $out"
(cd "$root" && go build -o "$out" .)

mkdir -p "$bindir"
cp "$out" "$bindir/pcx"
chmod 755 "$bindir/pcx"

# A repository downloaded through a browser can carry Gatekeeper's quarantine
# attribute. It may be copied onto this locally built binary, making macOS
# report that Apple cannot verify it. Removing it here is limited to the
# executable we just compiled from this checkout.
if [[ "$(uname -s)" == "Darwin" ]] && command -v xattr >/dev/null 2>&1; then
	if xattr -p com.apple.quarantine "$bindir/pcx" >/dev/null 2>&1; then
		echo "removing macOS quarantine from locally built pcx"
		if ! xattr -d com.apple.quarantine "$bindir/pcx"; then
			cat >&2 <<EOF
warning: macOS quarantine could not be removed.
Run:
  xattr -d com.apple.quarantine "$bindir/pcx"

Or try pcx once, then allow it under:
  System Settings > Privacy & Security > Open Anyway
EOF
		fi
	fi
fi

echo "installed $bindir/pcx"

if ! command -v tmux >/dev/null; then
	echo "warning: tmux not found; pcx needs it at runtime" >&2
fi

case ":$PATH:" in
*"$bindir"*) ;;
*) echo "warning: $bindir is not on PATH" >&2 ;;
esac
