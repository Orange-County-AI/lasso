# lasso

A web viewer for [herdr](https://herdr.dev) workspaces — a single Go binary that
serves a two-pane UI over your tailnet: herdr's terminal alongside a live git
diff, a file browser, and a grid of every pane herdr is running, across every
host you're connected to. It also exposes an [MCP](https://modelcontextprotocol.io)
server at `/mcp` so an agent can spawn and drive other agents through lasso.

## What's in it

Two resizable, collapsible columns:

- **Left** — the **herdr** terminal (a `ttyd` session in an iframe), a **Grid**
  of every herdr pane (click to focus, right-click to rename/close,
  ⌘/ctrl/shift-click to multi-select), and **Settings** (the lasso version and
  whether an update is available, the herdr protocol/version with a one-click
  `herdr update`, and the New-Agent defaults).
- **Right** — the git **Diff** of the focused pane's repo (working tree, or the
  branch vs. its base when clean), a **Files** browser that follows the active
  pane's directory and opens files in a markdown/code/image viewer, a **Browser**
  web-preview iframe for a dev server, and a plain **Terminal** shell outside
  herdr.

The UI follows herdr's active pane live and repaints to match herdr's theme.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/knowsuchagency/lasso/main/install.sh | sh
```

This drops a prebuilt binary at `~/.local/bin/lasso` (override with
`LASSO_INSTALL_DIR`). lasso drives **herdr**, so install that too if you haven't:

```bash
curl -fsSL https://herdr.dev/install.sh | sh
```

Then:

```bash
lasso start          # run it in the background
open http://127.0.0.1:8090
```

Run `lasso doctor` if anything looks off.

## Using the CLI

The binary is both the server and its own control surface:

| command | what it does |
| --- | --- |
| `lasso start` (alias `up`) | start the server in the background (PID + log under `~/.lasso/`) |
| `lasso stop` (alias `down`) | stop the background server |
| `lasso restart` | stop (if running) then start |
| `lasso status` | report whether it's running, and its URL |
| `lasso update` | update to the latest release (see [Updating](#updating)) |
| `lasso doctor` | check herdr, the socket, the port, and the version |
| `lasso version` | print the version |
| `lasso serve` | run in the **foreground** (what a bare `lasso` does) |

`start`/`restart`/`serve` accept the server flags (`-listen`, `-theme`,
`-insecure-no-auth`, …); `lasso serve -h` lists them.

## Run from source

```bash
mise run build      # build the frontend (web/dist) then the binary
./lasso             # serves on 127.0.0.1:8090, spawns ttyd running herdr
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
port that bumps if busy, so it never clashes with a production instance.

## Architecture

One Go binary that serves the embedded SPA, reverse-proxies the `ttyd` terminals
(WebSocket), talks to the herdr server over its unix socket to track the focused
pane and workspace layout, and pushes live state to the browser over SSE. It can
drive herdr on the local box or on SSH-reachable hosts (the footer's host
switcher / the Grid), so one lasso fronts a whole fleet. Each instance spawns its
own ttyds on per-PID unix sockets, so several can run at once without colliding.
The data and terminal routes live under `/api/*`, `/terminal/`, and `/shell/`,
plus an unauthenticated MCP server at `/mcp`; see the route table in `main.go`.

## Theming

Both panes adopt the theme from `~/.config/herdr/config.toml` (`[theme].name`)
and repaint live when you change it — no restart. Leave `-theme auto` to follow
herdr, or force one with `-theme <name>` (`lasso serve -h` lists the names).

## Updating

`lasso update` brings the binary up to date. It auto-detects the install:

- A **release binary** (the curl install) downloads the latest GitHub release for
  your platform, verifies its checksum, atomically replaces itself, and restarts
  the background server if one is running.
- A **pitchfork-supervised source checkout** (the maintainer's prod) keeps the
  historical behavior: `git pull --ff-only` then `pitchfork restart`, which
  rebuilds from source.

The Settings tab surfaces "update available → vX.Y.Z" when a newer release exists.

## Exposing it

The left pane is a **writable shell** (and `/mcp` is unauthenticated), so never
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

### Over your tailnet

Bind to your tailscale interface; only your tailnet can reach it, and WireGuard
already encrypts and authenticates it:

```bash
lasso start -listen "$(tailscale ip -4):8090" -insecure-no-auth
```

For a login on top, set `UI_AUTH=user:pass` in the environment (never argv) and
drop `-insecure-no-auth`. The server **refuses** a non-loopback bind unless one of
those is set, so it can't accidentally expose a bare shell. Then reach it from any
tailnet device at `http://<host>:8090/` (MagicDNS, e.g. `http://citadel:8090/`).

> **Downloads need a secure context.** The Files tab downloads via a synthetic
> `<a download>`, which browsers only honor on **localhost** or over **HTTPS**.
> Over plain-HTTP tailnet access (`http://citadel:8090`) a download silently
> won't fire — use the Cloudflare tunnel (HTTPS) if you need to pull files off
> the box. (Viewing files still works; only the download action is gated.)

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

## Dogfooding

To run lasso from *inside* a herdr session (e.g. building lasso with itself), its
embedded terminal would otherwise refuse to nest. Set `allow_nested = true` under
`[experimental]` in `~/.config/herdr/config.toml` to allow it.
