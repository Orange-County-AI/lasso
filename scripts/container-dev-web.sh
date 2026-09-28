#!/usr/bin/env bash
# Run the Vite dev server inside this worktree's dev container, wired to the
# lasso backend that is still running on the host.
#
#   usage: container-dev-web.sh <backend-port-on-host> <listen-ip>
#
# The backend deliberately stays on titan: it drives herdr, spawns panes and
# reads the real daemon socket, none of which belongs in a container. Only Vite
# moves, which is the half that executes third-party code — and it does so
# continuously, not just at install time, so it is if anything the more
# important half to isolate.
#
# Two incus proxy devices carry the traffic, declared in scripts/isb/dev-web.yaml
# (read it for the direction and binding of each). They are point-to-point TCP
# forwards for exactly one port each, which is why this does NOT need
# `--network host` — the container keeps its own network namespace.
#
# Inside the container Vite is always on 5173: a fresh network namespace has
# nothing else in it, so --strictPort is safe and the port is predictable. It is
# the HOST side that has to give way, so isb publishes the tailnet listener on
# the first free port from 5173 (`search`), the way the backend already bumps
# from 8190. Several dev instances run at once because each worktree has its own
# container (scripts/container.sh).
#
# Both devices are removed on exit; leaving them behind would hold the tailscale
# port. One left by a run that died without its trap is reclaimed by the next
# `up`: a stale `backend` differs and is replaced, and a stale `vite` still
# inside the search range is this worktree's own and is kept.
set -euo pipefail

port="${1:?backend port required}"
ip="${2:?listen ip required}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE/container.sh"

# One dev session per worktree. A second `mise run dev` here would share the
# container's 5173 and, worse, its `backend`/`vite` devices — its cleanup would
# delete the first session's. Refuse BEFORE the trap is armed so nothing of the
# live session is touched. Test for the live process, not for the `vite`
# device: a device left behind by a run that died without its trap (SIGKILL, a
# closed terminal) is garbage to reclaim, not a session to make way for. isb
# exits 125 for a container that does not exist yet, which is the
# fresh-worktree case and falls through.
require_isb
if container_isb exec -n -T web -- pgrep -f 'bun run dev' >/dev/null 2>&1; then
  echo "error: a dev server is already running for this worktree in $CONTAINER" >&2
  echo "       (stop it first; other worktrees have their own containers)" >&2
  exit 1
fi

export LASSO_BACKEND_PORT="$port" LASSO_LISTEN_IP="$ip"
container_with dev-web.yaml

cleanup() { isb -q port rm "$CONTAINER" backend vite >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM
container_ensure

# The listen address isb actually settled on, which is not 5173 when another
# worktree's dev server already holds it.
listen="$(isb -q port get "$CONTAINER" vite 2>/dev/null || true)"
hostport="${listen##*:}"
[ -n "$hostport" ] ||
  { echo "error: could not publish Vite on any port of $ip from 5173" >&2; exit 1; }

echo "vite: http://$ip:$hostport  (in $CONTAINER, backend on host 127.0.0.1:$port)"

# Deps must be present before vite starts; unlike the build task this is the
# only place they get installed on a fresh container. Deliberately NOT frozen:
# the dev loop is where you add a dependency, and it should pick it up and
# update bun.lock rather than refuse. The build is the strict one.
container_run "bun install"
container_run "env LASSO_BACKEND=http://127.0.0.1:8190 bun run dev --host 127.0.0.1 --port 5173 --strictPort"
