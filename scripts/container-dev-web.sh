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
# Two incus proxy devices carry the traffic. They are point-to-point TCP
# forwards for exactly one port each, which is why this does NOT need
# `--network host` — the container keeps its own network namespace.
#
#   backend  (bind=guest) container 127.0.0.1:8190 -> host 127.0.0.1:<port>
#            so the host backend stays on loopback and is never exposed on the
#            incus bridge, yet Vite's proxy can still reach /api and the ttyd
#            websockets.
#   vite     (bind=host)  host <ip>:<free port> -> container 127.0.0.1:5173
#            so a browser on the tailnet reaches the dev server. Vite binds
#            loopback INSIDE the container, so nothing else is published.
#
# Inside the container Vite is always on 5173: a fresh network namespace has
# nothing else in it, so --strictPort is safe and the port is predictable. It is
# the HOST side that has to give way, so the tailnet listener bumps to the first
# free port the way the backend already bumps from 8190. Several dev instances
# run at once because each worktree has its own container (scripts/container.sh);
# the old dev-lasso-2, -3 overflow containers are gone with the shared one.
#
# Both devices are removed on exit; leaving them behind would hold the tailscale
# port and silently shadow the next run.
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
# closed terminal) is garbage to reclaim — `cleanup` below does that — not a
# session to make way for. `incus exec` fails on a container that does not
# exist yet, which is the fresh-worktree case and falls through.
if incus exec "$CONTAINER" -- pgrep -f 'bun run dev' >/dev/null 2>&1 </dev/null; then
  echo "error: a dev server is already running for this worktree in $CONTAINER" >&2
  echo "       (stop it first; other worktrees have their own containers)" >&2
  exit 1
fi

container_ensure

cleanup() {
  incus config device remove "$CONTAINER" backend >/dev/null 2>&1 || true
  incus config device remove "$CONTAINER" vite    >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
cleanup   # clear anything a previous run left behind

# First free tailnet port from 5173 up. This has to come AFTER cleanup: a stale
# `vite` device from a killed run still holds its listener, so scanning first
# would step around a port we are about to release and bump for no reason. The
# scan and the add are not atomic — another worktree's dev can take the same
# port in between — so a failed add just moves on to the next port.
incus config device add "$CONTAINER" backend proxy bind=guest \
  listen=tcp:127.0.0.1:8190 connect=tcp:127.0.0.1:"$port" >/dev/null
hostport=5173
for _ in $(seq 1 50); do
  while ss -tln | grep -qE "[[:space:]]${ip}:${hostport}[[:space:]]"; do
    hostport=$(( hostport + 1 ))
  done
  incus config device add "$CONTAINER" vite proxy bind=host \
    listen=tcp:"$ip":"$hostport" connect=tcp:127.0.0.1:5173 >/dev/null 2>&1 && break
  hostport=$(( hostport + 1 ))
done
incus config device get "$CONTAINER" vite listen >/dev/null 2>&1 ||
  { echo "error: could not publish Vite on any port of $ip from 5173" >&2; exit 1; }

echo "vite: http://$ip:$hostport  (in $CONTAINER, backend on host 127.0.0.1:$port)"

# Deps must be present before vite starts; unlike the build task this is the
# only place they get installed on a fresh container. Deliberately NOT frozen:
# the dev loop is where you add a dependency, and it should pick it up and
# update bun.lock rather than refuse. The build is the strict one.
container_run "bun install"
container_run "env LASSO_BACKEND=http://127.0.0.1:8190 bun run dev --host 127.0.0.1 --port 5173 --strictPort"
