#!/usr/bin/env bash
# Re-render the icon set inside this worktree's dev container. See scripts/container.sh.
#
# This one is not about running the renderer, it is about installing it: uv
# resolves Pillow from PyPI on first run, which is the same class of surface as
# `bun install`. Rare is not the same as safe — a once-a-year task is the one
# nobody is watching.
#
# Two mounts (scripts/isb/icon.yaml adds the second), positioned to mirror the repo, because build.py finds its output
# directory by walking up from its own location to ../../src/web/public.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE/container.sh"

LASSO_ICON="$(cd "$HERE/../docs/icon" && pwd -P)"
export LASSO_ICON
container_with icon.yaml
container_ensure

container_run_in "$GUEST_ICON" "uv run --script ./build.py"
