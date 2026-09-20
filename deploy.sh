#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HOME/bin"
SERVICE="usage-store.service"
BINARY="usage-store-server"

cd "$REPO_DIR"

export PATH="$HOME/.local/share/mise/shims:$PATH"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=${XDG_RUNTIME_DIR}/bus}"

echo "==> Building $BINARY..."
go build -o "$BINARY" ./cmd/usage-store-server
echo "    built: $(ls -lh "$BINARY" | awk '{print $5}')"

# A misspelled USAGE_STORE_ variable, or an interval that is not a duration,
# stops the new binary at boot. Ask before the old one is stopped: build the
# registry from the running service's own environment. The test prints a
# verdict, never a value.
echo "==> Checking the running service's environment against the declared settings..."
live_pid="$(systemctl --user show -p MainPID --value "$SERVICE")"
if [ -n "$live_pid" ] && [ "$live_pid" != "0" ]; then
  go test -count=1 -run '^TestTheLiveProcessEnvironmentBuildsARegistry$' ./internal/config -args -live-environment-file="/proc/$live_pid/environ"
else
  echo "    $SERVICE is not running, so there is no environment to check"
fi

echo "==> Stopping $SERVICE..."
systemctl --user stop "$SERVICE" 2>/dev/null || true
sleep 1

echo "==> Installing binary to $BIN_DIR..."
cp "$BINARY" "$BIN_DIR/$BINARY"

echo "==> Installing service file..."
cp "$REPO_DIR/$SERVICE" "$HOME/.config/systemd/user/$SERVICE"
systemctl --user daemon-reload

echo "==> Starting $SERVICE..."
systemctl --user start "$SERVICE"

echo "==> Verifying..."
sleep 2
if systemctl --user is-active --quiet "$SERVICE"; then
  echo "    $SERVICE is running"
  journalctl --user -u "$SERVICE" -n 5 --no-pager 2>&1 | grep -v '^--'
else
  echo "ERROR: $SERVICE failed to start"
  journalctl --user -u "$SERVICE" -n 15 --no-pager 2>&1
  exit 1
fi

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
