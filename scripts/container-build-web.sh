#!/usr/bin/env bash
# Build src/web inside the dev-lasso container. See scripts/container.sh for
# why, and for the mount and idmap details.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE/container.sh"

container_ensure "$(cd "$HERE/../src/web" && pwd)"
container_run "bun install && bun run build"
