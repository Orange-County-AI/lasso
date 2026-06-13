# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Lasso is a Go backend (`main.go` and friends) that serves a React/TypeScript SPA. The frontend in `web/` is **built and embedded into the Go binary** — `go build` embeds `web/dist/`, so it must exist locally (run `mise run build`) but is **not** committed (it's gitignored). CI builds the frontend and produces release binaries.

## Commands

Backend (root, via [mise](https://mise.jdx.dev)):
- `mise run build` — builds the frontend (`bun run build` in `web/`) then `go build -o lasso .`
- `mise run dev` — Vite dev server with HMR, proxying to the Go backend (requires tailscale up; auto-bumps the dev port from 8190 if busy)
- `mise run test` — `go test .`

Frontend (`web/`, package manager is **bun**):
- `bun run dev` / `bun run build` (`tsc -b && vite build`)
- `bun run typecheck` — `tsc --noEmit`
- `bun run lint` — `biome lint .`
- `bun run format` — `biome format --write .`
- `bun run check` — `biome check --write .` (format + lint fixes + import/class sorting)

## Running the server (mise + pitchfork)

- The shipped install (`install.sh`) is **mise-based**: it installs lasso and its runtime (lasso via `ubi:knowsuchagency/lasso`, plus `pitchfork`, `ttyd`, `tmux`) as global mise tools. `lasso update` defers to `mise upgrade` when the running binary lives under mise's installs dir, else self-replaces the release binary; either way it then `pitchfork restart`s the daemon.
- **pitchfork is the required supervisor.** `lasso start/up`, `stop/down`, `restart`, `status` delegate to pitchfork (there is no self-daemon fallback — the old Setsid/PID-file path was removed). `lasso start` idempotently writes a marker-delimited `[daemons.<name>]` block into `~/.config/pitchfork/config.toml` (see `pitchfork.go`: `ensureLassoDaemon`/`upsertDaemonBlock`), then `pitchfork start <name> -f`. The block pins `env.PATH` (snapshot of the invoking PATH + mise shims) so the daemon can find tmux/ttyd/tailscale under a scrubbed supervisor env. Daemon name overridable via `LASSO_PITCHFORK_DAEMON`.
- **`--tailscale`** (on `serve`/`start`/`restart`) keeps lasso on loopback and publishes it on the tailnet via **`tailscale serve --bg`** (registered on start, explicitly torn down on shutdown — see `tailscale.go`). It needs the one-time `sudo tailscale set --operator=$USER`. Flags passed to `start`/`restart` persist into the daemon's run line. (portless was evaluated and rejected — its proxy needs sudo/:443 + trust-store writes and can't expose a persistent loopback server headlessly.)
- Tailscale Serve terminates HTTPS on `:443` by default (clean `https://<node>.ts.net` URL). **On citadel `:443` is owned by `dokploy-traefik` (Docker)**, which shadows it — so use `-tailscale-https-port 8443` there (`https://citadel.tail9dd8e.ts.net:8443`). The serve port is configurable via that flag.
- `mise run dev` is unchanged and deliberately self-daemon-free (multiple concurrent dev instances on bumped ports); it does not touch pitchfork.

## Frontend workflow

- Run `bun run typecheck` and `bun run lint` before considering frontend work done.
- `web/dist/` is the embedded bundle — gitignored and not committed. Run `mise run build` to regenerate it locally; CI builds it for releases.

## Formatting & linting

Tooling is **Biome** (`web/biome.json`) — it replaced Prettier + ESLint. Style: 2-space indent, no semicolons, double quotes, ES5 trailing commas, 80-col width. Tailwind class sorting is handled by Biome's `useSortedClasses` (aware of `cn`/`cva`). a11y rules are demoted to warnings (not previously enforced); don't treat them as blocking. Go code: standard `gofmt`.

## Security gotchas

- Never bind to `0.0.0.0`. Stay on loopback; reach it off-box via `--tailscale` (Tailscale Serve, HTTPS) or a Cloudflare tunnel. A bare non-loopback `-listen` still requires `UI_AUTH=user:pass` or `-insecure-no-auth`.
- `/api/file` reads arbitrary absolute paths as the running user — safe only on a private tailnet.
- Terminals run on a **dedicated tmux server socket** (`~/.lasso/tmux.sock`, `-f /dev/null`), isolated from your default tmux and surviving lasso restarts. Every tmux call must carry `-S ~/.lasso/tmux.sock -f /dev/null` — a missing `-S` would hit your real tmux server.
- The `/mcp` MCP server is **unauthenticated** (exempt from `UI_AUTH` via `withAuthExcept`) — it lets any client that can reach lasso spawn and drive agents. Same trust model as `/api/file`: safe only on loopback / a private tailnet, or behind an edge auth gate (e.g. Cloudflare Access). It introduces no new binding.
- The origin deliberately implements **no** OAuth (no `.well-known`, no `401`/`WWW-Authenticate`). So OAuth-based MCP clients (Claude Desktop / claude.ai connectors) connecting to `/mcp` over the public hostname require **Managed OAuth enabled on the Cloudflare Access app** — Access then acts as the OAuth 2.1 authorization server (Dynamic Client Registration + auth-code/PKCE), runs the login against the existing Access policy, and issues tokens; the origin still sees an authenticated Access session and needs no auth code. Without it the client's registration fails ("Couldn't register with lasso's sign-in service"). This is an edge setting on the Access application, not a `cloudflared`/tunnel change.
