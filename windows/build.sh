#!/usr/bin/env bash
# Builds the desktop app with the version metadata the About page shows.
#
# Wails hardcodes -buildvcs=false in its build command, so the commit is never
# embedded by Go itself and has to come in through ldflags. Building with a
# plain `wails build` still shows a build time (the executable's timestamp,
# see core/version), but the commit will read "unknown".
set -euo pipefail

cd "$(dirname "$0")"

version=$(cat ../VERSION 2>/dev/null || echo "")
commit=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
# Untracked files (logs, scratch output) are not part of what was compiled, so
# only tracked edits mark the build dirty.
if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
    commit="${commit}-dirty"
fi
built=$(date '+%Y-%m-%d %H:%M:%S')

ldflags="-X github.com/esrrhs/yellowsocks/core/version.GitCommit=${commit}"
ldflags="${ldflags} -X \"github.com/esrrhs/yellowsocks/core/version.BuildTime=${built}\""
if [ -n "${version}" ]; then
    ldflags="-X github.com/esrrhs/yellowsocks/core/version.Version=${version} ${ldflags}"
fi

echo "==> yellow socks GUI ${version:-(default version)} (commit: ${commit}, built: ${built})"
exec wails build -ldflags "${ldflags}" "$@"
