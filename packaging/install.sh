#!/bin/sh
# Install the Poker Tracker server on Linux: run its first-time setup (which
# generates the TLS certificate and pairing token and prints the connection
# string to paste into your poker PC), then register it as a systemd service
# that starts on boot and restarts on failure.
#
# Run this from the extracted tarball directory. It needs sudo for the service
# registration only.
set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$DIR/tracker-server"

if [ ! -x "$BIN" ]; then
	chmod +x "$BIN" 2>/dev/null || true
fi

echo "== Poker Tracker server setup =="
"$BIN" --setup

echo
echo "== Registering the boot service (needs sudo) =="
sudo "$BIN" --install

echo
echo "Done. The server is running and will start on boot."
echo "Message your Telegram bot to configure storage, alerts, and thresholds."
