#!/usr/bin/env bash
# Shared helpers for running lasso's frontend work inside the dev-lasso incus
# container instead of on titan.
#
# Why: `bun install` and the Vite dev server are the only places in this repo
# where code we didn't write executes. On the host that is the SSH key, the
# 1Password session and the tunnel credentials. In an unprivileged container it
# is an unprivileged uid in its own userns with one directory mounted.
#
# Only src/web and docs/icon are mounted, deliberately — NOT the repo root. A
# postinstall script that could reach ../.git could drop a hook, and the global
# post-commit hook auto-pushes main, so a writable .git is a path back out to
# the host.
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

# container_source <path-as-this-shell-sees-it>
#
# The path incusd will resolve for a bind mount, which is NOT always the path
# that named it. On titan the two are the same. Inside an agent-workspace box
# this shell's $HOME is a bind mount of a directory on the box's own host, and
# incusd — which runs in this box but resolves disk sources in its own mount
# view — only sees the host-side path. The box publishes that prefix in
# /etc/workspace/guest-home, so a mount of $HOME/... is translated onto it.
# Without this, `incus config device add` fails with "Missing source path" for a
# directory `ls` in the same shell lists happily.
container_source() {
  local p="$1" gh=""
  [ -r /etc/workspace/guest-home ] && gh="$(cat /etc/workspace/guest-home)"
  if [ -n "$gh" ] && [ -n "${HOME:-}" ] && [ "${p#"$HOME"/}" != "$p" ]; then
    printf '%s/%s\n' "${gh%/}" "${p#"$HOME"/}"
  else
    printf '%s\n' "$p"
  fi
}

# container_pool — the storage pool to create the container on. titan's is
# incus-zfs; a workspace box has whatever its own sandbox setup made (commonly
# a single `dir` pool). Naming a pool that does not exist fails the launch
# outright, so the existing set decides, with LASSO_DEV_STORAGE as the override.
container_pool() {
  if [ -n "${LASSO_DEV_STORAGE:-}" ]; then
    printf '%s\n' "$LASSO_DEV_STORAGE"
    return 0
  fi
  local pools p
  pools="$(incus storage list -f csv -c n 2>/dev/null)"
  for p in incus-zfs default; do
    printf '%s\n' "$pools" | grep -qx "$p" && { printf '%s\n' "$p"; return 0; }
  done
  printf '%s\n' "$pools" | head -n1
}

# container_needs_idmap — whether the launch has to ask for `raw.idmap`.
#
# The requirement is only ever that host uid/gid 1000 lands on the container's
# `dev` user, so the bind-mounted checkout is writable. Two ways to get there:
#
#   - titan: /etc/subuid gives root a range that does NOT contain 1000 (plus a
#     `root:1000:1` entry), so the default map puts the container elsewhere and
#     `raw.idmap both 1000 1000` is what pulls 1000 through.
#   - a workspace box: root's range STARTS at 0, so the default map is already
#     the identity map and guest 1000 IS host 1000. Asking for raw.idmap there
#     is not merely redundant, it is refused ("Host ID is in the range of
#     subids"), and forcing it by moving the range breaks the launch a second
#     way — a nested container cannot chown into uids outside the range its own
#     host gave it ("Failed to handle idmapped storage").
container_needs_idmap() {
  ! awk -F: '$1 == "root" && 1000 >= $2 && 1000 < $2 + $3 { f = 1 } END { exit !f }' \
    /etc/subuid 2>/dev/null
}

# container_mount <device> <host path> <guest path>
#
# Only touches the device when it is actually wrong. Re-adding a disk device
# remounts it inside the container, and that silently kills any inotify watch a
# running process holds: a live `mise run dev` keeps answering 200 while its
# HMR goes quiet, which is the worst way for this to fail. So `mise run lint`
# next to a running dev server must be a no-op here, not a remount.
container_mount() {
  local dev="$1" src dst="$3"
  src="$(container_source "$2")"
  if [ "$(incus config device get "$CONTAINER" "$dev" source 2>/dev/null)" = "$src" ] &&
     [ "$(incus config device get "$CONTAINER" "$dev" path   2>/dev/null)" = "$dst" ]; then
    return 0
  fi
  incus config device remove "$CONTAINER" "$dev" >/dev/null 2>&1 || true
  incus config device add "$CONTAINER" "$dev" disk source="$src" path="$dst" >/dev/null
}

# container_ensure <host-path-to-src/web>
# Creates the container if missing, starts it if stopped, and points its `web`
# mount at the checkout we're actually building — worktrees included — rather
# than whatever path was baked in when the container was first created.
container_ensure() {
  local web="$1"

  if ! incus image list -f csv -c l | tr ',' '\n' | grep -qx "$IMAGE"; then
    echo "error: incus image '$IMAGE' not found. See scripts/container.sh" >&2
    return 1
  fi

  if ! incus info "$CONTAINER" >/dev/null 2>&1; then
    echo "==> creating $CONTAINER from $IMAGE"
    local args=(--storage "$(container_pool)"
      -c limits.cpu=8 -c limits.memory=8GiB
      -c security.privileged=false)
    container_needs_idmap && args+=(-c raw.idmap="both 1000 1000")
    incus launch "$IMAGE" "$CONTAINER" "${args[@]}" >/dev/null
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
