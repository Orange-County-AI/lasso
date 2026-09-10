#!/usr/bin/env bash
# Re-render the icon set inside the dev-lasso container. See scripts/container.sh.
#
# This one is not about running the renderer, it is about installing it: uv
# resolves cairosvg and Pillow from PyPI on first run and builds their C
# extensions, which is the same class of surface as `bun install`. Rare is not
# the same as safe — a once-a-year task is the one nobody is watching.
#
# Two mounts, positioned to mirror the repo, because build.py finds its output
# directory by walking up from its own location to ../../src/web/public.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE/container.sh"

container_ensure "$(cd "$HERE/../src/web" && pwd)"
container_mount icon "$(cd "$HERE/../docs/icon" && pwd)" "$GUEST_ICON"

container_run_in "$GUEST_ICON" "uv run --script ./build.py"
