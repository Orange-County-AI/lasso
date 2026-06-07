#!/bin/sh
# lasso installer — downloads the latest release binary for your platform.
#
#   curl -fsSL https://raw.githubusercontent.com/knowsuchagency/lasso/main/install.sh | sh
#
# Honors:
#   LASSO_INSTALL_DIR   where to install (default ~/.local/bin)
set -eu

REPO="knowsuchagency/lasso"
BASE="https://github.com/${REPO}/releases/latest/download"

# --- detect platform -------------------------------------------------------
os=$(uname -s)
arch=$(uname -m)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "lasso: unsupported OS '$os' (need Linux or macOS)" >&2; exit 1 ;;
esac
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "lasso: unsupported architecture '$arch'" >&2; exit 1 ;;
esac
asset="lasso-${os}-${arch}"

# --- download --------------------------------------------------------------
dl() { # url outfile
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    echo "lasso: need curl or wget to download" >&2
    exit 1
  fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "lasso: downloading ${asset} …"
dl "${BASE}/${asset}" "${tmp}/${asset}"
dl "${BASE}/checksums.txt" "${tmp}/checksums.txt" || true

# --- verify checksum (when published) --------------------------------------
if [ -s "${tmp}/checksums.txt" ]; then
  want=$(awk -v a="$asset" '$2==a{print $1}' "${tmp}/checksums.txt")
  if [ -n "$want" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      got=$(sha256sum "${tmp}/${asset}" | awk '{print $1}')
    else
      got=$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')
    fi
    if [ "$want" != "$got" ]; then
      echo "lasso: checksum mismatch for ${asset} (want ${want}, got ${got})" >&2
      exit 1
    fi
    echo "lasso: checksum verified"
  fi
fi

# --- install ---------------------------------------------------------------
dir="${LASSO_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dir"
chmod +x "${tmp}/${asset}"
mv "${tmp}/${asset}" "${dir}/lasso"
echo "lasso: installed → ${dir}/lasso ($("${dir}/lasso" version 2>/dev/null || echo unknown))"

# --- PATH hint -------------------------------------------------------------
case ":${PATH}:" in
  *":${dir}:"*) ;;
  *)
    echo
    echo "note: ${dir} is not on your PATH — add it, e.g.:"
    echo "  export PATH=\"${dir}:\$PATH\""
    ;;
esac

# --- runtime prerequisites -------------------------------------------------
# lasso runs its terminals and agents in tmux and serves them through ttyd, so
# both must be on PATH. They're system packages (not auto-installed here); warn
# if missing so the user can grab them with their package manager.
missing=""
command -v tmux >/dev/null 2>&1 || missing="${missing} tmux"
command -v ttyd >/dev/null 2>&1 || missing="${missing} ttyd"
if [ -n "$missing" ]; then
  echo
  echo "lasso: missing prerequisite(s):${missing}"
  echo "  install with your package manager, e.g.:"
  echo "    macOS:  brew install${missing}"
  echo "    Debian/Ubuntu:  sudo apt install${missing}"
  echo "  (then 'lasso doctor' to confirm)"
fi

echo
echo "next: run 'lasso start', then open http://127.0.0.1:8090"
