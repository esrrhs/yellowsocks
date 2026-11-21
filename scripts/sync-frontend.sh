#!/bin/bash
# Syncs the Windows frontend (source of truth) to the macOS shell, so both
# platforms share the same UI. Run this after changing anything under
# windows/frontend (excludes build outputs and installed deps).
set -e
cd "$(dirname "$0")/.."

for f in $(cd windows/frontend && ls -A | grep -vE '^(node_modules|dist|package-lock.json)$'); do
    echo "syncing $f"
    rm -rf "macos/frontend/$f"
    cp -r "windows/frontend/$f" "macos/frontend/$f"
done
echo "macos/frontend is now in sync with windows/frontend (run npm install in macos/frontend)"
