#!/usr/bin/env bash
# Shared helpers for running lasso's frontend work inside an incus container
# instead of on titan. The container itself is declared in scripts/isb/*.yaml
# and driven by isb (https://github.com/execution-associates/isb), which talks
# to incusd over its unix socket: every step has a deadline, a stalled create
# is cancelled and retried instead of hanging, and the container is created
# with every mount and label in one request, before its first boot. The incus
# CLI is gone from this path on purpose: `incus init`/`launch` read instance
# YAML from stdin whenever stdin is not a terminal, so under an agent or a task
# runner whose stdin is an inherited socket that never closes, it blocked
# forever before ever reaching the server (seen: 11 minutes, no operation).
# This file only computes the per-worktree name and paths and wraps the calls.
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
# worktree is gone, which titan's weekly lasso-prune timer also does (keyed on
# user.lasso.worktree, so keep that label). To refresh the toolchain: start the
# stopped dev-base container, update it, `incus publish dev-base --alias
# dev-base --reuse -f`.
#
# Idle containers are left running, not stopped after each task, with one
# exception: `mise run dev` holds a foreground `isb up`, which stops this
# worktree's container when the dev server ends (container-dev-web.sh says
# why), and the next task starts it again. An idle one
# holds ~11 MiB of anonymous memory (measured on titan, 2026-09-27, cgroup
# memory.stat after a build); `incus info` shows over a GiB, but that is page
# cache from reading node_modules, which the kernel reclaims under pressure.
# The 8 CPU / 8 GiB below are caps, not reservations. Stopping when idle would
# need a cross-process refcount so a `mise run lint` exiting never stops the
# container under the same worktree's running `mise run dev`, plus a boot on
# every task after — all to save memory that isn't really spent. Stop one by
# hand (`isb stop <name>`) if you like; the next task starts it again.

HERE_CONTAINER="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ISB_DIR="$HERE_CONTAINER/isb"

# container_name <absolute host path to src/web>
#
# dev-lasso-<worktree slug>-<8 hex of sha256(path)>. The slug is only for the
# humans reading `isb ls`; the hash is what makes it unique, since two
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
# does not depend on how you cd'd in). CONTAINER is this worktree's container
# unless LASSO_DEV_CONTAINER pins one; pinning several checkouts onto one
# container brings the old sharing back with it (its `web` mount is re-pointed
# at whichever worktree ran last). All of these are exported because the isb
# compose files read them.
LASSO_WEB="$(cd "$HERE_CONTAINER/../src/web" && pwd -P)"
LASSO_WORKTREE="$(dirname "$(dirname "$LASSO_WEB")")"
CONTAINER="${LASSO_DEV_CONTAINER:-$(container_name "$LASSO_WEB")}"
LASSO_DEV_CONTAINER="$CONTAINER"
export LASSO_WEB LASSO_WORKTREE LASSO_DEV_CONTAINER

# Named volume for bun's global cache, shared by every dev container. Without
# it each new worktree's container starts cold and re-downloads the whole
# dependency tree (~600 MiB). Sharing is safe for correctness: the cache is
# content-addressed and written tmp-then-rename, and node_modules is still
# linked into each worktree's own directory. It is also a channel between
# worktrees — a postinstall in one could write to the cache another installs
# from — which is no wider than the single shared container it replaces, where
# that code had the whole rootfs. Set LASSO_DEV_BUN_CACHE= (empty) to opt out;
# the mount then simply isn't in the spec (scripts/isb/bun-cache.yaml).
export LASSO_DEV_BUN_CACHE="${LASSO_DEV_BUN_CACHE-lasso-bun-cache}"

# The compose files every dev-container call uses. Tasks that need more (the
# dev server's ports, the icon mount) add an overlay with container_with.
ISB_FILES=(-f "$ISB_DIR/dev.yaml")
[ -z "$LASSO_DEV_BUN_CACHE" ] || ISB_FILES+=(-f "$ISB_DIR/bun-cache.yaml")

# Mount points mirror the repo's own layout under a fake root (see
# scripts/isb/icon.yaml for why). Nothing else from the repo is there — no
# .git, no Go source, no docs.
GUEST_ROOT=/home/dev/repo
GUEST_WEB="$GUEST_ROOT/src/web"
# shellcheck disable=SC2034  # used by scripts that source this file
GUEST_ICON="$GUEST_ROOT/docs/icon"

# require_isb — fail with the install line rather than "command not found".
# mise.toml pins it to a prebuilt release binary, so `mise run` puts it on PATH.
require_isb() {
  command -v isb >/dev/null 2>&1 && return 0
  echo "error: isb is not on PATH. It is pinned in mise.toml: run \`mise install\`" >&2
  echo "       here, and invoke this through \`mise run\`." >&2
  return 1
}

# container_with <overlay.yaml> — add an overlay (a file in scripts/isb/) to
# every later isb call in this script.
container_with() { ISB_FILES+=(-f "$ISB_DIR/$1"); }

# container_isb <isb args...> — isb with this worktree's compose files.
container_isb() { isb -q "${ISB_FILES[@]}" "$@"; }

# The dev session's lock. container-dev-web.sh holds an flock on this file for
# its whole life (container_dev_lock), and the kernel drops it when the last
# process holding the fd dies, SIGKILL included, which no trap survives. That
# makes it the one reliable answer to "is a dev server alive for this
# container": a pgrep inside the container cannot tell a dev run that is still
# in `bun install` from one that was killed and left its devices behind.
# One directory level, so `mkdir -p -m 700` below applies the mode to all of it.
DEV_LOCK_DIR="${XDG_RUNTIME_DIR:-/tmp}/lasso-dev-$(id -u)"
DEV_LOCK="$DEV_LOCK_DIR/$CONTAINER.lock"

# container_dev_lock — take the dev lock on fd 9 for the rest of this process
# (and its children: isb inherits the fd). Fails if a dev session holds it.
container_dev_lock() {
  command -v flock >/dev/null 2>&1 ||
    { echo "error: flock (util-linux) is required for mise run dev" >&2; exit 1; }
  # shellcheck disable=SC2174  # DEV_LOCK_DIR is a single level
  mkdir -p -m 700 "$DEV_LOCK_DIR" && exec 9>"$DEV_LOCK" && flock -n 9
}

# container_reap_dev_ports — remove the dev server's `backend` and `vite`
# devices when no dev session is alive. They are dev-web.yaml's, and only that
# script's trap removes them; a dev run killed outright (an agent's background
# task stopped with SIGKILL) leaves them on the container isb stopped, and the
# next task to start it would publish the tailnet port again with no Vite
# behind it. The lock is taken non-blocking, so a live session (or this very
# script, when it is the dev one) is simply skipped, and it is released before
# `up`, so a dev run starting meanwhile is never refused. `port rm` of a device
# that is not there is a no-op; a container that does not exist yet fails,
# which is fine to ignore.
container_reap_dev_ports() {
  command -v flock >/dev/null 2>&1 || return 0
  # shellcheck disable=SC2174  # DEV_LOCK_DIR is a single level
  mkdir -p -m 700 "$DEV_LOCK_DIR" 2>/dev/null || return 0
  (
    flock -n 9 || exit 0
    isb -q port rm "$CONTAINER" backend vite >/dev/null 2>&1 || true
  ) 9>"$DEV_LOCK"
}

# container_ensure — create this worktree's container if missing, start it if
# stopped, and reconcile its devices and labels with the spec. isb holds a
# per-container lock around this, so two tasks started together in a fresh
# worktree don't both create it, and it only touches a device that is wrong:
# `mise run lint` next to a running `mise run dev` is a no-op, not a remount.
# -d because since isb 0.4 a bare `up` stays in the foreground until its
# commands exit (like `docker compose up`); every task here runs its work with
# exec afterwards, so it needs `up` to return. Only the dev server holds one
# (container_up_foreground).
container_ensure() {
  require_isb || return 1
  container_reap_dev_ports
  # Not quiet: a first create copies the whole image into a `dir` pool and can
  # take a minute or more, which should not look like a hang.
  isb "${ISB_FILES[@]}" up -d
}

# container_up_foreground — `isb up` held in the foreground: run the overlays'
# `command:` and stop the container when it exits, on a signal, or when any
# process that started isb goes away (isb polls its ancestors every second).
# Call container_ensure first: that `up -d` does the create and reconcile, so
# this one finds everything correct and touches no device. Not quiet, so the
# log says why it stopped; no log prefix, since there is one service and Vite's
# output reads better without `web | `.
container_up_foreground() {
  require_isb || return 1
  isb "${ISB_FILES[@]}" up --no-log-prefix
}

# container_run_in <dir> <command string> — run as the unprivileged `dev` user
# with the mise-managed toolchain (node, bun, uv) on PATH (scripts/isb/dev.yaml).
# Output streams as it is produced and the exit status is the command's; isb
# itself failing (no container, incusd unreachable) exits 125.
container_run_in() {
  container_isb exec -w "$1" web -- bash -c "$2"
}

# container_run <command string> — the common case, in the web dir.
container_run() { container_run_in "$GUEST_WEB" "$1"; }
