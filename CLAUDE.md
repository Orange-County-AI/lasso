# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Lasso is a Go backend (`src/main.go` and friends) that serves a React/TypeScript SPA. All Go code lives under `src/` (the Go module root — `src/go.mod`). The frontend in `src/web/` is **built and embedded into the Go binary** — `go build` embeds `src/web/dist/`, so it must exist locally (run `mise run build`) but is **not** committed (it's gitignored). CI builds the frontend and produces release binaries.

## Commands

Backend (run from repo root, via [mise](https://mise.jdx.dev); Go sources are in `src/`):
- `mise run build` — builds the frontend (`bun run build` in `src/web/`) then `go build` in `src/` (binary → `./lasso`)
- `mise run dev` — Vite dev server with HMR, proxying to the Go backend (requires tailscale up; auto-bumps the dev port from 8190 if busy)
- `mise run test` — `go test .` in `src/`

Frontend (`src/web/`, package manager is **bun**):
- `bun run dev` / `bun run build` (`tsc -b && vite build`)
- `bun run typecheck` — `tsc --noEmit`
- `bun run lint` — `biome lint .`
- `bun run format` — `biome format --write .`
- `bun run check` — `biome check --write .` (format + lint fixes + import/class sorting)

## Frontend workflow

- Run `bun run typecheck` and `bun run lint` before considering frontend work done.
- `src/web/dist/` is the embedded bundle — gitignored and not committed. Run `mise run build` to regenerate it locally; CI builds it for releases.

## Formatting & linting

Tooling is **Biome** (`src/web/biome.json`) — it replaced Prettier + ESLint. Style: 2-space indent, no semicolons, double quotes, ES5 trailing commas, 80-col width. Tailwind class sorting is handled by Biome's `useSortedClasses` (aware of `cn`/`cva`). a11y rules are demoted to warnings (not previously enforced); don't treat them as blocking. Go code: standard `gofmt`.

## Security gotchas

- Never bind to `0.0.0.0`. Use loopback or the tailscale IP. For non-loopback access set `UI_AUTH=user:pass`.
- `/api/file` reads arbitrary absolute paths as the running user — safe only on a private tailnet.
- Running lasso nested inside herdr requires `allow_nested = true` in `~/.config/herdr/config.toml`.
- The `/mcp` MCP server is **unauthenticated by default** (exempt from `UI_AUTH` via `withAuthExcept`) — it lets any client that can reach lasso spawn and drive agents. Same trust model as `/api/file`: safe only on loopback / a private tailnet, or behind an edge auth gate (e.g. Cloudflare Access). It introduces no new binding. Set `MCP_OAUTH` (below) to gate it in the origin instead.
- **Unless `MCP_OAUTH` is set**, the origin deliberately implements **no** OAuth (no `.well-known`, no `401`/`WWW-Authenticate`). So OAuth-based MCP clients (Claude Desktop / claude.ai connectors) connecting to `/mcp` over the public hostname require **Managed OAuth enabled on the Cloudflare Access app** — Access then acts as the OAuth 2.1 authorization server (Dynamic Client Registration + auth-code/PKCE), runs the login against the existing Access policy, and issues tokens; the origin still sees an authenticated Access session and needs no auth code. Without it the client's registration fails ("Couldn't register with lasso's sign-in service"). This is an edge setting on the Access application, not a `cloudflared`/tunnel change. `oauth.go` keeps every OAuth route 404 while `MCP_OAUTH` is unset precisely so this Access path stays intact — don't make that metadata unconditional.

## MCP OAuth (`src/oauth.go`)

`MCP_OAUTH=client_id:client_secret` (environment only, like `UI_AUTH` — never argv) turns lasso into a small OAuth 2.1 authorization server for its own `/mcp` resource. Unset = today's open `/mcp`, unchanged.

- **Grants**: `client_credentials` (machine-to-machine — scripts, CLIs, Claude Code; restricted to the `MCP_OAUTH` client) and `authorization_code` + PKCE/S256 with `refresh_token` rotation. claude.ai / Claude Desktop custom connectors **cannot** use `client_credentials` — Anthropic's connector infra requires `authorization_code`+`refresh_token` and per-connection user consent — so their "Advanced settings → OAuth Client ID / Client Secret" fields are a *pre-registered client for the auth-code flow*. That's why both grants exist.
- **Routes**: `/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server`, `/oauth/register` (open DCR), `/oauth/token` — all exempt from `UI_AUTH`, since they're the credential-less half of the handshake. `/oauth/authorize` is **deliberately gated** by `UI_AUTH`/Access: that human gate is what makes open DCR safe (anyone may register; nobody gets a token without passing the door and clicking Approve).
- `/mcp` accepts **either** a bearer token **or** the `UI_AUTH` basic credentials, so the CLI and existing callers keep working without an OAuth dance.
- Redirect URIs: DCR clients get exact matching. The pre-registered client accepts any https/loopback callback (a connector's callback isn't known when the secret is minted) and the consent screen displays the exact target; set `MCP_OAUTH_REDIRECT_URIS=<comma-separated>` to lock it to an allowlist.
- Codes and tokens live in `oauth_*` tables in `lasso.db`, stored as SHA-256 hashes, so refresh tokens survive restarts and self-updates.
