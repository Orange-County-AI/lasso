#!/usr/bin/env bash
# A general-purpose incus sandbox for one-off third-party tools (reladraw and
# the like), kept apart from the dev-lasso-* containers. Declared in
# scripts/isb/sandbox.yaml and driven by isb, like the dev containers.
#
# Why a separate container: the dev containers carry the frontend's toolchain
# and a src/web mount, and a tool fetched from npm for a docs task has no
# business sharing either. This one is plain Debian with bun, nothing else, and each
# task mounts only the directory it works on.
#
# It is disposable, like the dev containers: delete it and the next task
# recreates and re-provisions it in about a minute. Unlike theirs, its user has
# no sudo, so a compromised package stays an unprivileged uid inside an
# unprivileged container.

HERE_SANDBOX="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/container.sh
. "$HERE_SANDBOX/container.sh"

SANDBOX_BUN="1.3.14"
SANDBOX_FILES=(-f "$HERE_SANDBOX/isb/sandbox.yaml")

sandbox_isb() { isb -q "${SANDBOX_FILES[@]}" "$@"; }

# sandbox_ensure — create, reconcile and start the sandbox, then provision it
# once. The task script exports the variable its mount reads first (see
# sandbox.yaml). Provisioning is marked by a file inside the container, so an
# interrupted run resumes rather than leaving a half-built box that looks ready.
sandbox_ensure() {
  require_isb || return 1
  isb "${SANDBOX_FILES[@]}" up || return 1

  sandbox_isb exec -n -u root sandbox -- test -e /etc/lasso-sandbox-ready && return 0

  echo "==> provisioning the sandbox (bun $SANDBOX_BUN)"
  # Root gets root's HOME and sbin on PATH (useradd); sandbox.yaml's exec
  # defaults are the dev user's.
  sandbox_isb exec -n -u root -w / -e HOME=/root \
    -e PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin sandbox -- sh -euc '
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends ca-certificates curl unzip >/dev/null
    id -u dev >/dev/null 2>&1 || useradd -m -u 1000 -s /bin/bash dev
  '
  # bun from its release zip rather than an installer script: one pinned
  # binary, checked against the checksum file published beside it.
  sandbox_isb exec -n -w /tmp sandbox -- sh -euc "
    base=https://github.com/oven-sh/bun/releases/download/bun-v$SANDBOX_BUN
    curl -fsSLO \$base/bun-linux-x64.zip
    curl -fsSL \$base/SHASUMS256.txt | grep ' bun-linux-x64.zip\$' | sha256sum -c -
    unzip -qo bun-linux-x64.zip
    mkdir -p ~/.local/bin
    mv bun-linux-x64/bun ~/.local/bin/bun
    ln -sf bun ~/.local/bin/bunx
    rm -rf bun-linux-x64 bun-linux-x64.zip
  "
  sandbox_isb exec -n -u root sandbox -- touch /etc/lasso-sandbox-ready
}

# sandbox_run_in <dir> <command string> — run as the unprivileged `dev` user.
sandbox_run_in() {
  sandbox_isb exec -w "$1" sandbox -- bash -c "$2"
}
