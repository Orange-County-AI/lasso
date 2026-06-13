# lasso

A web UI for running and orchestrating coding agents — a single Go binary that
serves a terminal-centric workspace over your tailnet (or a Cloudflare tunnel):
a sidebar of your git repos and their worktrees, live agents (Claude Code /
Codex) with their status, a fast terminal, and a git diff + file browser
alongside. It also exposes an [MCP](https://modelcontextprotocol.io) server at
`/mcp` so an agent can spawn and drive other agents through lasso.

Terminals and agents run in **tmux** (on a dedicated server socket), so they
survive lasso restarts and updates.

## What's in it

Three resizable, collapsible columns:

- **Left — sidebar.** Your git repos as collapsible roots with their worktrees
  nested underneath, plus scratch (non-git) workspaces. Right-click to create a
  worktree, rename, or close; ⌘/ctrl-click to multi-select and bulk-delete. Each
  agent shows a live status dot (idle / working / blocked).
- **Center — terminal.** A tab strip per workspace over one persistent terminal
  (a `ttyd` session pointed at the selected tab's tmux session via
  `switch-client`, so switching tabs is instant). The **New Agent** button (⌘O)
  spins up a Claude Code or Codex agent in a fresh worktree or scratch dir.
- **Right — tools.** The git **Diff** of the active workspace (working tree, or
  the branch vs. its base when clean), a **Files** browser that follows the
  active directory and opens files in a markdown/code/image viewer, a **Browser**
  web-preview iframe for a dev server, a **Scratch** pad, and **Settings**
  (appearance + New-Agent defaults).

The UI is dark-first with a System / Light / Dark toggle (the
[Onyx](https://github.com/knowsuchagency/onyx) design system); the terminal
palette follows the same mode.

## Install

```bash
curl -fsSL https://go.52labs.us/install-lasso | sh
```

The installer uses [**mise**](https://mise.jdx.dev) to install lasso and its whole
runtime — lasso itself, **pitchfork** (the supervisor), **ttyd**, and **tmux** —
as global mise tools, then registers a pitchfork daemon and starts it. Run
`lasso doctor` to confirm. To also expose lasso on your tailnet, install with
tailscale support:

```bash
LASSO_WITH_TAILSCALE=1 curl -fsSL https://go.52labs.us/install-lasso | sh
```

That installs tailscale, grants this user `tailscale serve` permission, and starts
lasso with `--tailscale`. Then:

```bash
lasso status                 # the pitchfork daemon's status
open http://127.0.0.1:8090
```

## Using the CLI

The binary is both the server and its own control surface. lasso is supervised by
**pitchfork**, so `start`/`stop`/`restart`/`status` drive a pitchfork daemon:

| command | what it does |
| --- | --- |
| `lasso start` (alias `up`) | register + start the pitchfork-supervised server |
| `lasso stop` (alias `down`) | stop the supervised server |
| `lasso restart` | restart it (re-applying any flags) |
| `lasso status` | show the pitchfork daemon's status |
| `lasso update` | update to the latest release (see [Updating](#updating)) |
| `lasso doctor` | check tmux, ttyd, pitchfork, the port, and whether an update is available |
| `lasso version` | print the version |
| `lasso serve` | run in the **foreground** (what a bare `lasso`, and the daemon, does) |

`start`/`restart`/`serve` accept the server flags (`--tailscale`, `-listen`,
`-insecure-no-auth`, …); `lasso serve -h` lists them. Flags passed to
`start`/`restart` are persisted into the daemon's run line, so `lasso start
--tailscale` keeps exposing the tailnet across restarts and reboots until you
`lasso start` without it.

## Run from source

```bash
mise run build      # build the frontend (web/dist) then the binary
./lasso             # serves on 127.0.0.1:8090
mise run dev        # Vite dev server (frontend HMR) + Go backend, on your tailnet
mise run test       # Go tests
```

The frontend is a React + Vite + Tailwind (shadcn/ui) app under `web/`, built to
`web/dist` and embedded into the binary via `go:embed` — so the shipped binary is
self-contained. `go build` therefore needs `web/dist` to exist; `mise run build`
produces it. `web/dist` is **gitignored** (not committed) — run `mise run build`
locally, and CI builds it for releases.

`mise run dev` serves the UI through Vite with hot reload and proxies the API and
terminal routes to the Go backend: frontend edits reload instantly, Go changes
need a task restart. It binds your tailscale interface and uses a dedicated dev
port (8190) that bumps if busy, so it never clashes with a production instance
(8090).

## Architecture

One Go binary that serves the embedded SPA, reverse-proxies the `ttyd` terminal
(WebSocket), owns the terminal/agent layer directly via **tmux**, and pushes
live state to the browser over SSE.

- **tmux** on a dedicated server socket (`~/.lasso/tmux.sock`, `-f /dev/null`)
  backs every terminal and agent — one session per tab, isolated from your
  default tmux and surviving lasso restarts. A SQLite db (`~/.lasso/lasso.db`)
  stores the workspace/tab tree so the layout is restored after a reboot (as
  fresh shells; agents aren't auto-relaunched).
- **Agent status** (idle / working / blocked) comes from screen-scraping
  `tmux capture-pane` plus process detection — only Claude Code and Codex.
- The data + terminal routes live under `/api/*` and `/tab-term/`, plus an
  unauthenticated MCP server at `/mcp`; see the route table in `main.go`.

The `/mcp` server's tools — `create_agent`, `list_agents`, `send_agent`,
`read_agent`, `wait_agent`, `close_agent`, `whoami` — let an agent spawn and
drive other agents end to end. An agent reads its own `$LASSO_TAB_ID` to identify
itself via `whoami`.

## Theming

The UI uses the **Onyx** design system (dark-first, indigo accent). Pick System,
Light, or Dark in **Settings → Appearance**; the choice persists per device and
repaints both the UI and the terminal live, no restart. There is no theme config
file.

## Updating

`lasso update` brings lasso up to date, then restarts the pitchfork daemon onto
the new version. It auto-detects the install:

- A **mise-managed install** (the standard `install.sh` path) defers to
  `mise upgrade`, which pulls the latest release.
- A **plain release binary** (a curl-installed copy elsewhere) downloads the
  latest GitHub release for your platform, verifies its checksum, and atomically
  replaces itself.

Either way, if a pitchfork `lasso` daemon is registered it's restarted onto the
new binary. The Settings tab surfaces "update available → vX.Y.Z" when a newer
release exists.

## Exposing it

The terminal is a **writable shell** (and `/mcp` is unauthenticated), so never
bind to `0.0.0.0` — on a VPS that's the public internet. Two safe ways to reach
it off-box:

### Over a Cloudflare tunnel (recommended)

Keep lasso on loopback and let a tunnel reach it, so no port is ever exposed:

```bash
lasso start -listen 127.0.0.1:8090
```

Point a [cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
tunnel's ingress at `http://127.0.0.1:8090` and gate the hostname with
**Cloudflare Access** (or equivalent) — that authentication is what guards the
writable shell and the MCP endpoint. A loopback bind needs no `-insecure-no-auth`.
Because the tunnel serves **HTTPS**, the browser runs in a secure context, so
Files-tab downloads work (see the caveat below).

OAuth-based MCP clients (Claude Desktop / claude.ai connectors) connecting to
`/mcp` over the public hostname require **Managed OAuth enabled on the Cloudflare
Access app** — Access then acts as the OAuth authorization server. The origin
implements no OAuth of its own.

### Over your tailnet

Pass `--tailscale`: lasso stays bound to loopback and publishes itself on your
tailnet through [**Tailscale Serve**](https://tailscale.com/kb/1242/tailscale-serve),
which terminates **HTTPS** with real certs at `https://<node>.<tailnet>.ts.net`.

```bash
lasso start --tailscale
```

This needs the one-time `sudo tailscale set --operator=$USER` so the (non-root)
daemon can write the serve config — the `LASSO_WITH_TAILSCALE=1` installer does
that for you. Only your tailnet can reach the URL, and WireGuard already encrypts
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

## Releasing

Releases are cut by CI on a version tag:

```bash
mise run bump patch --commit     # bump lassoSemver in version.go and commit
git tag "v$(grep -oP 'lassoSemver = "\K[^"]+' version.go)"
git push origin main --tags
```

`.github/workflows/release.yml` then builds the frontend, cross-compiles the
binaries (linux/darwin × amd64/arm64) with the tag stamped in, and publishes a
GitHub Release with the binaries, `checksums.txt`, and `install.sh`. The tag must
match `lassoSemver` (the workflow enforces it).
