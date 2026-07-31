#!/bin/sh
# Startup script for the PokerTracker-Server-linux self-extracting installer
# (built with makeself in .github/workflows/release.yml). makeself unpacks the
# archive to a temporary directory and runs this script from there, with both
# server binaries (tracker-server-amd64, tracker-server-arm64) alongside it. We
# pick the one for this CPU, install it into the user's home (no root), run
# first-time setup, and register the per-user service. The systemd unit + the
# one privileged "start on boot" step live in the Go binary's --install (a single
# home for that logic), not duplicated here.
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
cp "$BIN" "$DEST/tracker-server"
chmod +x "$DEST/tracker-server"

echo "Installed the Poker Tracker server to $DEST/tracker-server"
echo

# First-time setup: generate the TLS certificate + pairing token and show the
# connection string. Opens a window on a graphical desktop; prints to the
# terminal on a headless box.
"$DEST/tracker-server" --setup

# Register the per-user systemd service (no root) and offer to enable start-on-boot.
"$DEST/tracker-server" --install

echo
echo "Done. Message your Telegram bot to configure storage, alerts, and thresholds."
