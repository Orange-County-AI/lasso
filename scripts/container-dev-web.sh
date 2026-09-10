#!/usr/bin/env bash
# Run the Vite dev server inside the dev-lasso container, wired to the lasso
# backend that is still running on the host.
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
# free port the way the backend already bumps from 8190. That keeps the existing
# property that several dev instances can run at once — each gets its own
# container, since two Vites cannot share one namespace's 5173.
#
# Both devices are removed on exit; leaving them behind would hold the tailscale
# port and silently shadow the next run.
set -euo pipefail

port="${1:?backend port required}"
ip="${2:?listen ip required}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE/container.sh"

# A container already serving a dev session has a `vite` device on it; step to
# the next one rather than stealing its port and re-pointing its mount.
while incus config device get "$CONTAINER" vite listen >/dev/null 2>&1; do
  n=$(( ${n:-1} + 1 )); CONTAINER="${LASSO_DEV_CONTAINER:-dev-lasso}-$n"
done

# First free tailnet port from 5173 up.
hostport=5173
while ss -tln | grep -qE "[[:space:]]${ip}:${hostport}[[:space:]]"; do
  hostport=$(( hostport + 1 ))
done

container_ensure "$(cd "$HERE/../src/web" && pwd)"

cleanup() {
  incus config device remove "$CONTAINER" backend >/dev/null 2>&1 || true
  incus config device remove "$CONTAINER" vite    >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM
cleanup   # clear anything a previous run left behind

incus config device add "$CONTAINER" backend proxy bind=guest \
  listen=tcp:127.0.0.1:8190 connect=tcp:127.0.0.1:"$port" >/dev/null
incus config device add "$CONTAINER" vite proxy bind=host \
  listen=tcp:"$ip":"$hostport" connect=tcp:127.0.0.1:5173 >/dev/null

echo "vite: http://$ip:$hostport  (in $CONTAINER, backend on host 127.0.0.1:$port)"

# Deps must be present before vite starts; unlike the build task this is the
# only place they get installed on a fresh container.
container_run "bun install --frozen-lockfile"
container_run "env LASSO_BACKEND=http://127.0.0.1:8190 bun run dev --host 127.0.0.1 --port 5173 --strictPort"
