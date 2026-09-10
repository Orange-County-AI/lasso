#!/usr/bin/env bash
# Run one of src/web's bun scripts inside the dev-lasso container.
#
#   usage: container-web.sh <script> [args...]   e.g. container-web.sh typecheck
#
# typecheck/lint/format all execute third-party binaries out of node_modules
# (tsc, biome). That is the same code `bun install` fetched, so running it on
# the host would hand back exactly the access the container exists to withhold —
# the isolation is only worth as much as the least-isolated command in the loop.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE/container.sh"

container_ensure "$(cd "$HERE/../src/web" && pwd)"
container_run "bun run $*"
