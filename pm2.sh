#!/bin/bash

# Restart Apsthira under pm2: delete the old process, start the new binary.

set -e

APP_NAME="apsthira"

# Run from the repo root so the binary and .env resolve correctly
cd "$(dirname "$0")"

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
