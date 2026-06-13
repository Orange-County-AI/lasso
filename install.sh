#!/bin/sh
# lasso installer — installs lasso and its whole runtime through mise, then
# supervises the server with pitchfork.
#
#   curl -fsSL https://go.52labs.us/install-lasso | sh
#
# Everything (lasso, pitchfork, ttyd, tmux) is installed as a global mise tool, so
# `mise upgrade` / `lasso update` keep them current. Honored env vars:
#   LASSO_WITH_TAILSCALE=1   also install tailscale and expose lasso on the tailnet
#   LASSO_NO_START=1         install only; don't register/start the pitchfork daemon
set -eu

note() { printf 'lasso: %s\n' "$*"; }

# --- ensure mise (the installer for everything) ----------------------------
if ! command -v mise >/dev/null 2>&1 && [ ! -x "$HOME/.local/bin/mise" ]; then
  note "installing mise …"
  curl -fsSL https://mise.run | sh
fi
MISE="$(command -v mise 2>/dev/null || echo "$HOME/.local/bin/mise")"
[ -x "$MISE" ] || { echo "lasso: mise install failed — see https://mise.jdx.dev" >&2; exit 1; }

# Make this run's mise shims callable for the rest of the script.
MISE_DATA="${MISE_DATA_DIR:-$HOME/.local/share/mise}"
export PATH="$MISE_DATA/shims:$HOME/.local/bin:$PATH"

# --- install lasso + runtime as global mise tools --------------------------
note "installing lasso + runtime via mise (lasso, pitchfork, ttyd) …"
"$MISE" use -g \
  "ubi:knowsuchagency/lasso" \
  "pitchfork" \
  "aqua:tsl0922/ttyd"

# tmux compiles from source under mise (asdf plugin) and needs a C toolchain, so
# it's best-effort: fall back to a system package if the build can't run here.
if ! command -v tmux >/dev/null 2>&1; then
  note "installing tmux via mise (compiles from source) …"
  "$MISE" use -g tmux 2>/dev/null || true
fi
if ! command -v tmux >/dev/null 2>&1; then
  note "tmux still missing — install it with your package manager:"
  echo "      Debian/Ubuntu:  sudo apt install tmux"
  echo "      macOS:          brew install tmux"
fi

# --- optional: tailscale + tailnet exposure --------------------------------
if [ "${LASSO_WITH_TAILSCALE:-0}" = "1" ]; then
  if ! command -v tailscale >/dev/null 2>&1; then
    note "installing tailscale (official installer) …"
    curl -fsSL https://tailscale.com/install.sh | sh || \
      note "tailscale install failed — see https://tailscale.com/download"
  fi
  # `tailscale serve` writes need operator permission once; otherwise the lasso
  # daemon (non-root) can't publish the tailnet route.
  if command -v tailscale >/dev/null 2>&1; then
    note "granting this user 'tailscale serve' permission (sudo) …"
    sudo tailscale set --operator="$USER" 2>/dev/null || \
      note "couldn't set the operator — run once:  sudo tailscale set --operator=\$USER"
  fi
fi

note "installed → $("$MISE" which lasso 2>/dev/null || echo lasso) ($(lasso version 2>/dev/null || echo unknown))"

# --- supervise with pitchfork ----------------------------------------------
if [ "${LASSO_NO_START:-0}" = "1" ]; then
  note "skipping daemon start (LASSO_NO_START=1) — run 'lasso start' when ready"
else
  note "enabling the pitchfork supervisor + starting lasso …"
  pitchfork boot enable 2>/dev/null || true
  pitchfork supervisor start 2>/dev/null || true
  if [ "${LASSO_WITH_TAILSCALE:-0}" = "1" ]; then
    lasso start --tailscale
  else
    lasso start
  fi
fi

# --- PATH hint -------------------------------------------------------------
case ":${PATH}:" in
  *":$MISE_DATA/shims:"*) ;;
  *)
    echo
    note "add mise's shims to your shell so 'lasso' is always found:"
    echo "      echo 'eval \"\$($MISE activate bash)\"' >> ~/.bashrc   # or your shell's rc"
    ;;
esac

echo
note "ready — open http://127.0.0.1:8090   (run 'lasso doctor' to verify, 'lasso status' for the daemon)"
