#!/usr/bin/env bash
# Render the architecture diagram (docs/architecture/*.reladraw -> .svg) inside
# the `sandbox` incus container. See scripts/sandbox.sh.
#
# reladraw comes from npm via bunx: a package we did not write, fetched and
# run. So it runs in the sandbox with only docs/architecture mounted, never on
# the host and never next to a dev container's frontend checkout.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/sandbox.sh
. "$HERE/sandbox.sh"

GUEST_DIAGRAM=/home/dev/work/architecture

sandbox_ensure
container_mount diagram "$(cd "$HERE/../docs/architecture" && pwd)" "$GUEST_DIAGRAM"

sandbox_run_in "$GUEST_DIAGRAM" 'for f in *.reladraw; do bunx reladraw "$f"; done'
