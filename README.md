# lasso

A web viewer for [herdr](https://herdr.dev) workspaces — a single Go binary that
serves a two-pane UI over your tailnet: herdr's terminal alongside a live git
diff, a file browser, and a grid of every pane herdr is running.

## What's in it

Two resizable, collapsible columns:

- **Left** — the **herdr** terminal (a `ttyd` session in an iframe), a **Grid**
  of every herdr pane (click to focus, right-click to rename/close,
  ⌘/ctrl/shift-click to multi-select), and **Settings** (installed vs. latest
  herdr version, with a one-click `herdr update`).
- **Right** — the git **Diff** of the focused pane's repo (working tree, or the
  branch vs. its base when clean), a **Files** browser that follows the active
  pane's directory and opens files in a markdown/code/image viewer, a **Browser**
  web-preview iframe for a dev server, and a plain **Terminal** shell outside
  herdr.

The UI follows herdr's active pane live and repaints to match herdr's theme.

## Run

```bash
mise run build      # build the frontend + binary
./lasso             # serves on 127.0.0.1:8090, spawns ttyd running herdr
```

Open <http://localhost:8090>. Run `./lasso -h` for the full flag list — the ones
you'll usually touch are `-listen`, `-theme`, and `-insecure-no-auth` (below).

## Develop

```bash
mise run dev        # Vite dev server (frontend HMR) + Go backend, on your tailnet
mise run test       # Go tests
```

The frontend is a React + Vite + Tailwind (shadcn/ui) app under `web/`, built to
`web/dist` and embedded into the binary via `go:embed` — so the shipped binary is
self-contained. `go build` therefore needs `web/dist` to exist; `mise run build`
produces it, and it's committed so a bare `go build` works.

`mise run dev` serves the UI through Vite with hot reload and proxies the API and
terminal routes to the Go backend: frontend edits reload instantly, Go changes
need a task restart. It binds your tailscale interface and uses a dedicated dev
port that bumps if busy, so it never clashes with a production instance.

## Architecture

One Go binary that serves the embedded SPA, reverse-proxies the `ttyd` terminals
(WebSocket), talks to the herdr server over its unix socket to track the focused
pane and workspace layout, and pushes live state to the browser over SSE. Each
instance spawns its own ttyds on per-PID unix sockets, so several can run at once
without colliding. The data and terminal routes live under `/api/*`, `/terminal/`,
and `/shell/`; see the route table in `main.go` for specifics.

## Theming

Both panes adopt the theme from `~/.config/herdr/config.toml` (`[theme].name`)
and repaint live when you change it — no restart. Leave `-theme auto` to follow
herdr, or force one with `-theme <name>` (`./lasso -h` lists the names).

## Exposing it (tailnet only)

The left pane is a **writable shell**, so never bind to `0.0.0.0` — on a VPS
that's the public internet. Bind to your tailscale interface instead; only your
tailnet can reach it, and WireGuard already encrypts and authenticates it:

```bash
./lasso -listen "$(tailscale ip -4):8090" -insecure-no-auth
```

For a login on top, set `UI_AUTH=user:pass` in the environment (never argv) and
drop `-insecure-no-auth`. The server **refuses** a non-loopback bind unless one of
those is set, so it can't accidentally expose a bare shell. Then reach it from any
tailnet device at `http://<host>:8090/` (MagicDNS, e.g. `http://citadel:8090/`).

Note `/api/file` reads any absolute path as the running user — fine on a private
tailnet, but confine it before widening access.

## Dogfooding

To run lasso from *inside* a herdr session (e.g. building lasso with itself), its
embedded terminal would otherwise refuse to nest. Set `allow_nested = true` under
`[experimental]` in `~/.config/herdr/config.toml` to allow it.
