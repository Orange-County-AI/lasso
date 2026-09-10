#!/usr/bin/env bash
# Shared helpers for running lasso's frontend work inside the dev-lasso incus
# container instead of on titan.
#
# Why: `bun install` and the Vite dev server are the only places in this repo
# where code we didn't write executes. On the host that is the SSH key, the
# 1Password session and the tunnel credentials. In an unprivileged container it
# is an unprivileged uid in its own userns with one directory mounted.
#
# Only src/web is mounted, deliberately — NOT the repo root. A postinstall
# script that could reach ../.git could drop a hook, and the global post-commit
# hook auto-pushes main, so a writable .git is a path back out to the host.
#
# The container is disposable. Delete it and the next task rebuilds it from the
# dev-base image in about a minute. To refresh the toolchain: start the stopped
# dev-base container, update it, `incus publish dev-base --alias dev-base --reuse -f`.

CONTAINER="${LASSO_DEV_CONTAINER:-dev-lasso}"
IMAGE="${LASSO_DEV_IMAGE:-dev-base}"

# Mount points mirror the repo's own layout under a fake root. docs/icon/build.py
# walks up to `parents[1]/src/web/public` to find where to write, so the two
# subtrees have to sit at their real relative positions or that path escapes the
# mounts. Nothing else from the repo is here — no .git, no Go source, no docs.
GUEST_ROOT=/home/dev/repo
GUEST_WEB="$GUEST_ROOT/src/web"
# shellcheck disable=SC2034  # used by scripts that source this file
GUEST_ICON="$GUEST_ROOT/docs/icon"

# container_mount <device> <host path> <guest path>
container_mount() {
  incus config device remove "$CONTAINER" "$1" >/dev/null 2>&1 || true
  incus config device add "$CONTAINER" "$1" disk source="$2" path="$3" >/dev/null
}

# container_ensure <host-path-to-src/web>
# Creates the container if missing, starts it if stopped, and points its `web`
# mount at the checkout we're actually building — worktrees included — rather
# than whatever path was baked in when the container was first created.
container_ensure() {
  local web="$1"

  if ! incus image list -f csv -c l | grep -qx "$IMAGE"; then
    echo "error: incus image '$IMAGE' not found. See scripts/container.sh" >&2
    return 1
  fi

  if ! incus info "$CONTAINER" >/dev/null 2>&1; then
    echo "==> creating $CONTAINER from $IMAGE"
    # raw.idmap maps host uid/gid 1000 straight through so the bind-mounted
    # source is writable as the container's `dev` user. It needs `root:1000:1`
    # in /etc/subuid and /etc/subgid on the host.
    incus launch "$IMAGE" "$CONTAINER" --storage incus-zfs \
      -c limits.cpu=8 -c limits.memory=8GiB \
      -c security.privileged=false \
      -c raw.idmap="both 1000 1000" >/dev/null
  fi

  [ "$(incus info "$CONTAINER" | awk '/^Status:/{print $2}')" = "RUNNING" ] \
    || incus start "$CONTAINER"

  container_mount web "$web" "$GUEST_WEB"
}

# container_run_in <dir> <command string> — run as the unprivileged `dev` user
# with the mise-managed toolchain (node, bun, uv) on PATH.
container_run_in() {
  incus exec "$CONTAINER" -- sudo -u dev -H bash -lc \
    "eval \"\$(\$HOME/.local/bin/mise activate bash --shims)\" && cd $1 && $2"
}

# container_run <command string> — the common case, in the web dir.
container_run() { container_run_in "$GUEST_WEB" "$1"; }
