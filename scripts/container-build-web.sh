#!/usr/bin/env bash
# Build src/web inside the dev-lasso container. See scripts/container.sh for
# why, and for the mount and idmap details.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE/container.sh"

container_ensure "$(cd "$HERE/../src/web" && pwd)"
# --frozen-lockfile here, not in the dev loop: this build feeds src/web/dist,
# which go:embed bakes into the shipped binary. A resolution that quietly drifts
# from bun.lock should fail loudly at that point, not get rewritten in passing.
container_run "bun install --frozen-lockfile && bun run build"
