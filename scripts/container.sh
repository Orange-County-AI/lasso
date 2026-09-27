#!/usr/bin/env bash
# Shared helpers for running lasso's frontend work inside an incus container
# instead of on titan.
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
# ONE CONTAINER PER WORKTREE. lasso is developed in many git worktrees at once,
# each with its own agent. There used to be a single shared `dev-lasso` whose
# `web` device was re-pointed at whichever worktree ran a task last, so worktree
# B's `mise run lint` unmounted the tree under worktree A's running Vite
# ("Rolldown panicked ... Failed to get current dir", SIGABRT) — or, worse, let
# A's next build run against B's files. Now the name is derived from the
# checkout (container_name), each container only ever mounts its own worktree,
# and no task can touch another worktree's container. node_modules was never
# the problem: `bun install` writes it into the mounted src/web, i.e. into each
# worktree's own host directory. What IS shared is bun's download cache — see
# container_bun_cache.
#
# The containers are disposable. Delete one and the next task rebuilds it from
# the dev-base image in about a minute. `mise run dev:containers` lists them
# with the worktree each belongs to; `mise run dev:prune` removes the ones whose
# worktree is gone. To refresh the toolchain: start the stopped dev-base
# container, update it, `incus publish dev-base --alias dev-base --reuse -f`.
#
# Idle containers are left running, not stopped after each task. An idle one
# holds ~11 MiB of anonymous memory (measured on titan, 2026-09-27, cgroup
# memory.stat after a build); `incus info` shows over a GiB, but that is page
# cache from reading node_modules, which the kernel reclaims under pressure.
# The 8 CPU / 8 GiB below are caps, not reservations. Stopping when idle would
# need a cross-process refcount so a `mise run lint` exiting never stops the
# container under the same worktree's running `mise run dev`, plus a boot on
# every task after — all to save memory that isn't really spent. Stop one by
# hand (`incus stop <name>`) if you like; the next task starts it again.

IMAGE="${LASSO_DEV_IMAGE:-dev-base}"

# container_name <absolute host path to src/web>
#
# dev-lasso-<worktree slug>-<8 hex of sha256(path)>. The slug is only for the
# humans reading `incus list`; the hash is what makes it unique, since two
# checkouts can share a directory name (~/.lasso/worktrees/lasso/x and
# ~/.herdr/worktrees/lasso/x). incus wants <=63 chars of [a-z0-9-] starting
# with a letter, so the slug is lowercased, squeezed and cut to fit. The hash
# suffix also means these can never collide with the old shared names
# (dev-lasso, dev-lasso-2, ...).
container_name() {
  local web="$1" slug hash
  slug="$(basename "$(dirname "$(dirname "$web")")" | tr 'A-Z' 'a-z' |
    tr -c 'a-z0-9' '-' | tr -s '-' | sed 's/^-*//; s/-*$//')"
  slug="${slug:0:44}"; slug="${slug%-}"
  hash="$(printf %s "$web" | sha256sum | cut -c1-8)"
  printf 'dev-lasso-%s%s\n' "${slug:+$slug-}" "$hash"
}

# This checkout's src/web, as the host sees it (symlinks resolved so the name
# does not depend on how you cd'd in). Every task script derives its paths from
# here, and CONTAINER is this worktree's container unless LASSO_DEV_CONTAINER
# pins one — which sandbox.sh also overrides after sourcing this.
LASSO_WEB="$(cd "$(dirname "${BASH_SOURCE[0]}")/../src/web" && pwd -P)"
CONTAINER="${LASSO_DEV_CONTAINER:-$(container_name "$LASSO_WEB")}"

# Named custom volume for bun's global cache, shared by every dev container.
# Without it each new worktree's container starts cold and re-downloads the
# whole dependency tree (~600 MiB). Sharing is safe for correctness: the cache
# is content-addressed and written tmp-then-rename, and node_modules is still
# linked into each worktree's own directory. It is also a channel between
# worktrees — a postinstall in one could write to the cache another installs
# from — which is no wider than the single shared container it replaces, where
# that code had the whole rootfs. Set LASSO_DEV_BUN_CACHE= (empty) to opt out.
BUN_CACHE_VOLUME="${LASSO_DEV_BUN_CACHE-lasso-bun-cache}"
GUEST_BUN_CACHE=/home/dev/.bun/install/cache

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
#
# Only a real subid RANGE counts. titan's `root:1000:1` is the delegation that
# lets raw.idmap map 1000 at all, not a range the default map draws from, and
# matching it made this answer "no" on exactly the host that needs "yes" — so
# every container launched here came up with a read-only mount.
container_needs_idmap() {
  ! awk -F: '$1 == "root" && $3 > 1 && 1000 >= $2 && 1000 < $2 + $3 { f = 1 } END { exit !f }' \
    /etc/subuid 2>/dev/null
}

# container_mount <device> <host path> <guest path>
#
# Only touches the device when it is actually wrong. Re-adding a disk device
# remounts it inside the container, and that silently kills any inotify watch a
# running process holds: a live `mise run dev` keeps answering 200 while its
# HMR goes quiet, which is the worst way for this to fail. So `mise run lint`
# next to a running dev server in the SAME worktree must be a no-op here, not a
# remount. (Across worktrees it cannot happen at all any more: each has its own
# container. The remount path is left for LASSO_DEV_CONTAINER, which can pin
# several checkouts onto one container and gets the old sharing back with it.)
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

# container_bun_cache — attach the shared bun cache volume (see
# BUN_CACHE_VOLUME), creating it on first use. Runs before the container's
# first boot, so it is a plain device add, never a remount under a live task.
container_bun_cache() {
  [ -n "$BUN_CACHE_VOLUME" ] || return 0
  local pool
  pool="$(container_pool)"
  # Two worktrees' first tasks can race to create it; losing that race is fine
  # as long as the volume exists afterwards.
  incus storage volume show "$pool" "$BUN_CACHE_VOLUME" >/dev/null 2>&1 ||
    incus storage volume create "$pool" "$BUN_CACHE_VOLUME" >/dev/null 2>&1 ||
    incus storage volume show "$pool" "$BUN_CACHE_VOLUME" >/dev/null
  incus config device add "$CONTAINER" bun-cache disk \
    pool="$pool" source="$BUN_CACHE_VOLUME" path="$GUEST_BUN_CACHE" >/dev/null
}

# container_ensure
# Creates this worktree's container if missing and starts it if stopped.
#
# Creation is init -> configure -> start, so every device is in place before
# the first boot. The user.lasso.* keys record which checkout the container
# belongs to: `mise run dev:containers` and `dev:prune` read them, since the
# name alone is a one-way hash. Held under a per-container flock so two tasks
# started together in a fresh worktree (typecheck and lint, say) don't both try
# to create it. The lock fd is scoped to the { } block — it must not leak into
# a long-lived child like Vite, or every later task here would block on it.
container_ensure() {
  if ! incus image list -f csv -c l | tr ',' '\n' | grep -qx "$IMAGE"; then
    echo "error: incus image '$IMAGE' not found. See scripts/container.sh" >&2
    return 1
  fi

  {
    flock 9

    if ! incus info "$CONTAINER" >/dev/null 2>&1; then
      echo "==> creating $CONTAINER from $IMAGE for $(dirname "$(dirname "$LASSO_WEB")")"
      local args=(--storage "$(container_pool)"
        -c limits.cpu=8 -c limits.memory=8GiB
        -c security.privileged=false
        -c user.lasso.worktree="$(dirname "$(dirname "$LASSO_WEB")")"
        -c user.lasso.web="$LASSO_WEB")
      container_needs_idmap && args+=(-c raw.idmap="both 1000 1000")
      incus init "$IMAGE" "$CONTAINER" "${args[@]}" >/dev/null
      container_mount web "$LASSO_WEB" "$GUEST_WEB"
      container_bun_cache
      incus start "$CONTAINER"
      # A fresh volume's root is owned by (container) root, and bun has to
      # write there as dev. Parents too: the mount point conjured ~/.bun and
      # ~/.bun/install if the image didn't have them, root-owned.
      [ -z "$BUN_CACHE_VOLUME" ] || incus exec "$CONTAINER" -- \
        chown dev:dev /home/dev/.bun /home/dev/.bun/install "$GUEST_BUN_CACHE"
      # The network comes up a beat after the container does, and the first
      # thing any task here does is `bun install`.
      incus exec "$CONTAINER" -- sh -c \
        'for i in $(seq 30); do ip route | grep -q default && exit 0; sleep 1; done; exit 1' ||
        echo "warning: $CONTAINER has no default route yet; bun install may fail" >&2
    fi

    [ "$(incus info "$CONTAINER" | awk '/^Status:/{print $2}')" = "RUNNING" ] \
      || incus start "$CONTAINER"

    container_mount web "$LASSO_WEB" "$GUEST_WEB"
  } 9>"${XDG_RUNTIME_DIR:-/tmp}/lasso-$CONTAINER.lock"
}
# container_run_in <dir> <command string> — run as the unprivileged `dev` user
# with the mise-managed toolchain (node, bun, uv) on PATH.
container_run_in() {
  incus exec "$CONTAINER" -- sudo -u dev -H bash -lc \
    "eval \"\$(\$HOME/.local/bin/mise activate bash --shims)\" && cd $1 && $2"
}

# container_run <command string> — the common case, in the web dir.
container_run() { container_run_in "$GUEST_WEB" "$1"; }
