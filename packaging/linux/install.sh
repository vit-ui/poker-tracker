#!/bin/sh
set -e

ARCH="$(uname -m)"
case "$ARCH" in
	x86_64 | amd64) BIN="tracker-server-amd64" ;;
	aarch64 | arm64) BIN="tracker-server-arm64" ;;
	*)
		echo "Unsupported CPU architecture: $ARCH (this server needs x86_64 or aarch64)." >&2
		exit 1
		;;
esac

if [ ! -f "$BIN" ]; then
	echo "Installer is missing $BIN — the download may be incomplete or corrupt." >&2
	exit 1
fi

DEST="${XDG_DATA_HOME:-$HOME/.local/share}/poker-tracker"
mkdir -p "$DEST"
STAGED_SERVER="$(mktemp "$DEST/.tracker-server.XXXXXX")"
trap 'rm -f "$STAGED_SERVER"' 0
trap 'exit 1' HUP INT TERM
cp "$BIN" "$STAGED_SERVER"
chmod +x "$STAGED_SERVER"
mv -f "$STAGED_SERVER" "$DEST/tracker-server"

echo "Installed the Poker Tracker server to $DEST/tracker-server"
echo

"$DEST/tracker-server" --setup
"$DEST/tracker-server" --install
echo
echo "Tracker is installed and running."
if "$DEST/tracker-server" --connect-telegram-if-needed; then
    echo "Telegram pairing saved."
fi
