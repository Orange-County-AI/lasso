#!/usr/bin/env bash
# Run one of src/web's bun scripts inside this worktree's dev container.
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

container_ensure
# A fresh worktree has no node_modules, and with one container per worktree
# that is now the common first run rather than a rare one. Install only when it
# is missing: a check is not the place to rewrite a tree that the same
# worktree's running `mise run dev` is serving from. Frozen, like the build —
# a check should run against the lockfile, not a re-resolution of it.
container_run "test -d node_modules || bun install --frozen-lockfile"
container_run "bun run $*"
