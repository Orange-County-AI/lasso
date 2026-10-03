#!/usr/bin/env bash
# Build the dev-base image that every per-worktree dev container (and isb's
# own sandboxes) starts from: Ubuntu 24.04, a `dev` user at uid 1000 with
# passwordless sudo, build tools, and mise with node, go, bun and uv.
#
#   scripts/dev-base.sh            build it if the image is missing
#   scripts/dev-base.sh --force    rebuild and republish over the old one
#
# The source container stays behind, stopped, under the same name, as
# container.sh describes: to refresh a toolchain, start it, change it, and
# `incus publish dev-base --alias dev-base --reuse -f`. Running this script
# again recreates it from scratch instead.
#
# It runs on the host because it drives incus, but everything it installs is
# installed inside the container; nothing here executes on the host.
set -euo pipefail

NAME=dev-base
SOURCE=images:ubuntu/24.04
NODE=26.7.0
GO=1.25.3
PACKAGES=(build-essential curl git ca-certificates unzip python3 xz-utils sudo)

force=false
[[ ${1:-} == --force ]] && force=true

# Plain incus commands take </dev/null, or init/exec can hang on stdin.
inc() { incus "$@" </dev/null; }

if inc image alias list --format csv | grep -q "^${NAME},"; then
  if ! $force; then
    echo "image $NAME exists; --force rebuilds it"
    exit 0
  fi
fi
if inc info "$NAME" >/dev/null 2>&1; then
  if ! $force; then
    echo "error: a container named $NAME exists but the image does not;" >&2
    echo "       publish it (incus publish $NAME --alias $NAME) or pass --force" >&2
    exit 1
  fi
  inc delete -f "$NAME"
fi

inc launch "$SOURCE" "$NAME"
# Wait for the network: apt needs it.
for _ in $(seq 60); do
  inc exec "$NAME" -- getent hosts archive.ubuntu.com >/dev/null 2>&1 && break
  sleep 1
done

as_root() { inc exec "$NAME" -- "$@"; }
as_dev() { inc exec "$NAME" --user 1000 --group 1000 --cwd /home/dev --env HOME=/home/dev -- "$@"; }

as_root env DEBIAN_FRONTEND=noninteractive apt-get update -q
as_root env DEBIAN_FRONTEND=noninteractive apt-get install -yq --no-install-recommends "${PACKAGES[@]}"

# The image's own uid-1000 user, if it has one, becomes dev.
existing=$(as_root getent passwd 1000 | cut -d: -f1 || true)
if [[ -n $existing && $existing != dev ]]; then
  as_root usermod -l dev -d /home/dev -m "$existing"
  as_root groupmod -n dev "$existing" 2>/dev/null || true
elif [[ -z $existing ]]; then
  as_root useradd -m -u 1000 -U -s /bin/bash dev
fi
as_root usermod -aG sudo dev
as_root sh -c "printf 'dev ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/90-dev && chmod 0440 /etc/sudoers.d/90-dev"

# mise from its own installer (it verifies the download), into ~/.local/bin.
as_dev sh -c 'curl -fsSL https://mise.run | sh'
as_dev sh -c 'grep -q "mise activate" ~/.bashrc || echo '\''eval "$(~/.local/bin/mise activate bash)"'\'' >> ~/.bashrc'
as_dev /home/dev/.local/bin/mise use -g "node@$NODE" "go@$GO" bun@latest uv@latest
as_dev /home/dev/.local/bin/mise ls

as_root apt-get clean
inc stop "$NAME"
inc publish "$NAME" --alias "$NAME" --reuse -f \
  description="dev-base: Ubuntu 24.04, dev (uid 1000), mise: node $NODE, go $GO, bun, uv"
echo "published $NAME; the stopped container $NAME is its source"
