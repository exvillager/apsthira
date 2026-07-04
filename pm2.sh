#!/bin/bash

# Restart Apsthira under pm2: delete the old process, start the new binary.

set -e

APP_NAME="apsthira"

# Run from the repo root so the binary and .env resolve correctly
cd "$(dirname "$0")"

# Non-interactive SSH sessions (e.g. GitHub Actions deploys) don't source
# ~/.bashrc, so bun-installed pm2 isn't on PATH. Add it explicitly.
export PATH="$PATH:$HOME/.bun/bin"

if ! command -v pm2 >/dev/null 2>&1; then
    echo "Error: pm2 not found. Install it or add it to PATH." >&2
    exit 1
fi

if [ ! -f "./apsthira" ]; then
    echo "Binary './apsthira' not found. Building first..."
    ./build.sh
fi

# Delete the previous process if it exists (ignore error if it doesn't)
pm2 delete "$APP_NAME" 2>/dev/null || true

pm2 start ./apsthira --name "$APP_NAME"

# Persist the process list so it survives VPS reboots
pm2 save

pm2 ls
