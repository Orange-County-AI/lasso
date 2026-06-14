# lasso

A web UI for running and orchestrating coding agents — a single Go binary that
serves a terminal-centric workspace over your tailnet (or a Cloudflare tunnel):
a sidebar of your git repos and their worktrees, live agents (Claude Code /
Codex) with their status, a fast terminal, and a git diff + file browser
alongside. It also exposes an [MCP](https://modelcontextprotocol.io) server at
`/mcp` so an agent can spawn and drive other agents through lasso.

Terminals and agents run in **tmux** (on a dedicated server socket), so they
survive lasso restarts and updates.

## Install

```bash
curl -fsSL https://go.52labs.us/install-lasso | sh
```

Then:

```bash
lasso status # the pitchfork daemon's status
open http://127.0.0.1:8090
```

## Using the CLI

The binary is both the server and its own control surface. lasso is supervised by
**pitchfork**, so `start`/`stop`/`restart`/`status` drive a pitchfork daemon:

| command | what it does |
| --- | --- |
| `lasso` (no args) | print help — a bare invocation does **not** start the server |
| `lasso start` (alias `up`) | register + start the pitchfork-supervised server |
| `lasso stop` (alias `down`) | stop the supervised server |
| `lasso restart` | restart it (re-applying any flags) |
| `lasso status` | show the pitchfork daemon's status |
| `lasso update` | update to the latest release (`mise upgrade` or self-replace), then restart the daemon |
| `lasso doctor` | check tmux, ttyd, pitchfork, the port, and whether an update is available |
| `lasso version` | print the version |

`start`/`restart` accept the server flags (`--tailscale`, `-listen`,
`-insecure-no-auth`, …) and persist them into the daemon's run line, so `lasso
start --tailscale` keeps exposing the tailnet across restarts and reboots until
you `lasso start` without it. (The pitchfork daemon runs the foreground server by
passing those flags to the binary; there's no `serve` subcommand.)

## Exposing it

The terminal is a **writable shell** (and `/mcp` is unauthenticated), so never
bind to `0.0.0.0` — on a VPS that's the public internet. Two safe ways to reach
it off-box:

### Over your tailnet

Pass `--tailscale`: lasso stays bound to loopback and publishes itself on your
tailnet through [**Tailscale Serve**](https://tailscale.com/kb/1242/tailscale-serve),
which terminates **HTTPS** with real certs at `https://<node>.<tailnet>.ts.net`.

```bash
lasso start --tailscale
```

This needs the one-time `sudo tailscale set --operator=$USER` so the (non-root)
daemon can write the serve config — the installer does that for you (tailscale is
installed and `--tailscale` enabled by default). Only your tailnet can reach the URL, and WireGuard already encrypts
and authenticates it; lasso never binds a non-loopback port, so no
`-insecure-no-auth` is involved. The route is held only while lasso runs and is
torn down on stop. Because it's served over HTTPS, the browser runs in a secure
context, so Files-tab downloads work.

If `:443` is already taken on the host (e.g. a Docker/Traefik publish owns it),
Tailscale Serve on 443 is shadowed — serve on another HTTPS port instead:

```bash
lasso start --tailscale -tailscale-https-port 8443
# → https://<node>.<tailnet>.ts.net:8443
```

Note `/api/file` reads any absolute path as the running user, and `/mcp` is open —
fine on a private tailnet or behind Access, but confine it before widening access.
