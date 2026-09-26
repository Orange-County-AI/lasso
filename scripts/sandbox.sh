#!/usr/bin/env bash
# A general-purpose incus sandbox for one-off third-party tools (reladraw and
# the like), kept apart from dev-lasso.
#
# Why a second container: dev-lasso carries the frontend's toolchain and the
# src/web mount, and a tool fetched from npm for a docs task has no business
# sharing either. This one is plain Debian with bun, nothing else, and each
# task mounts only the directory it works on (see container_mount).
#
# It is disposable, like dev-lasso: delete it and the next task recreates and
# re-provisions it in about a minute. Unlike dev-lasso its user has no sudo, so
# a compromised package stays an unprivileged uid inside an unprivileged
# container.

HERE_SANDBOX="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE_SANDBOX/container.sh"

# container.sh's helpers act on $CONTAINER, so repoint it at the sandbox.
CONTAINER="${LASSO_SANDBOX_CONTAINER:-sandbox}"
SANDBOX_IMAGE="${LASSO_SANDBOX_IMAGE:-images:debian/13}"
SANDBOX_BUN="1.3.14"

# sandbox_ensure — create, provision and start the sandbox as needed.
# Provisioning is marked by a file inside the container, so an interrupted run
# resumes rather than leaving a half-built box that looks ready.
sandbox_ensure() {
  if ! incus info "$CONTAINER" >/dev/null 2>&1; then
    echo "==> creating $CONTAINER from $SANDBOX_IMAGE"
    local args=(--storage "$(container_pool)"
      -c limits.cpu=4 -c limits.memory=4GiB
      -c security.privileged=false)
    container_needs_idmap && args+=(-c raw.idmap="both 1000 1000")
    incus launch "$SANDBOX_IMAGE" "$CONTAINER" "${args[@]}" >/dev/null
  fi

  [ "$(incus info "$CONTAINER" | awk '/^Status:/{print $2}')" = "RUNNING" ] \
    || incus start "$CONTAINER"

  # The network comes up a beat after the container does, and apt and bunx
  # both fail fast without it, so wait for a default route on every start.
  incus exec "$CONTAINER" -- sh -c \
    'for i in $(seq 30); do ip route | grep -q default && exit 0; sleep 1; done; exit 1'

  incus exec "$CONTAINER" -- test -e /etc/lasso-sandbox-ready && return 0

  echo "==> provisioning $CONTAINER (bun $SANDBOX_BUN)"
  incus exec "$CONTAINER" -- sh -euc '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends ca-certificates curl unzip >/dev/null
    id -u dev >/dev/null 2>&1 || useradd -m -u 1000 -s /bin/bash dev
  '
  # bun from its release zip rather than an installer script: one pinned
  # binary, checked against the checksum file published beside it.
  incus exec "$CONTAINER" --user 1000 --group 1000 --env HOME=/home/dev -- sh -euc "
    cd /tmp
    base=https://github.com/oven-sh/bun/releases/download/bun-v$SANDBOX_BUN
    curl -fsSLO \$base/bun-linux-x64.zip
    curl -fsSL \$base/SHASUMS256.txt | grep ' bun-linux-x64.zip\$' | sha256sum -c -
    unzip -qo bun-linux-x64.zip
    mkdir -p ~/.local/bin
    mv bun-linux-x64/bun ~/.local/bin/bun
    ln -sf bun ~/.local/bin/bunx
    rm -rf bun-linux-x64 bun-linux-x64.zip
  "
  incus exec "$CONTAINER" -- touch /etc/lasso-sandbox-ready
}

# sandbox_run_in <dir> <command string> — run as the unprivileged `dev` user.
sandbox_run_in() {
  incus exec "$CONTAINER" --cwd "$1" --user 1000 --group 1000 \
    --env HOME=/home/dev --env PATH=/home/dev/.local/bin:/usr/bin:/bin -- \
    bash -c "$2"
}
