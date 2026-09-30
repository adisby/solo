#!/usr/bin/env bash
# Start the make-managed Solo stack unattended — intended to be invoked by a
# login LaunchAgent (see the plist documented in docs/autostart.md).
#
# Why not just `make start`? A login agent can fire before Docker Desktop has
# finished booting, and scripts/ensure-postgres.sh only waits 30 seconds before
# failing with "PostgreSQL not ready after 30s". This wrapper waits for the
# Docker daemon first, then hands off to the normal make-managed lifecycle.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DOCKER_TIMEOUT="${SOLO_AUTOSTART_DOCKER_TIMEOUT:-300}"
stamp() { date '+%Y-%m-%d %H:%M:%S'; }

echo "[$(stamp)] autostart: waiting for docker daemon (timeout ${DOCKER_TIMEOUT}s)"
deadline=$((SECONDS + DOCKER_TIMEOUT))
until docker info >/dev/null 2>&1; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "[$(stamp)] autostart: docker daemon not ready after ${DOCKER_TIMEOUT}s" >&2
    exit 1
  fi
  sleep 5
done

echo "[$(stamp)] autostart: docker ready; running make start"
exec make start
