#!/usr/bin/env bash
# Shared helpers for running lasso's frontend work inside an incus container
# instead of on titan. The container itself is declared in scripts/isb/*.yaml
# and driven by isb (https://github.com/execution-associates/isb), which talks
# to incusd over its unix socket: every step has a deadline, a stalled create
# is cancelled and retried instead of hanging (a bare `incus init` once sat for
# 11 minutes on an operation the server never had), and the container is
# created with every mount and label in one request, before its first boot.
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
# Idle containers are left running, not stopped after each task. An idle one
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
# Installed from its git repo until it is on crates.io. Build it somewhere
# isolated if you rebuild: build.rs and proc-macros are arbitrary code.
require_isb() {
  command -v isb >/dev/null 2>&1 && return 0
  echo "error: isb is not on PATH. Install it with:" >&2
  echo "  mise use -g \"cargo:https://github.com/execution-associates/isb@branch:main\"" >&2
  return 1
}

# container_with <overlay.yaml> — add an overlay (a file in scripts/isb/) to
# every later isb call in this script.
container_with() { ISB_FILES+=(-f "$ISB_DIR/$1"); }

# container_isb <isb args...> — isb with this worktree's compose files.
container_isb() { isb -q "${ISB_FILES[@]}" "$@"; }

# container_ensure — create this worktree's container if missing, start it if
# stopped, and reconcile its devices and labels with the spec. isb holds a
# per-container lock around this, so two tasks started together in a fresh
# worktree don't both create it, and it only touches a device that is wrong:
# `mise run lint` next to a running `mise run dev` is a no-op, not a remount.
container_ensure() {
  require_isb || return 1
  container_isb up
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
