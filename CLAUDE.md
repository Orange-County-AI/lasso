# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Lasso is a Go backend (`main.go` and friends) that serves a React/TypeScript SPA. The frontend in `web/` is **built and embedded into the Go binary** — `web/dist/` is committed and `go build` fails without it.

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

## Frontend workflow

- Run `bun run typecheck` and `bun run lint` before considering frontend work done.
- `web/dist/` is the embedded bundle. **Rebuild and commit it before pushing** (run `mise run build`), not on every commit — so the binary stays in sync with source.

## Formatting & linting

Tooling is **Biome** (`web/biome.json`) — it replaced Prettier + ESLint. Style: 2-space indent, no semicolons, double quotes, ES5 trailing commas, 80-col width. Tailwind class sorting is handled by Biome's `useSortedClasses` (aware of `cn`/`cva`). a11y rules are demoted to warnings (not previously enforced); don't treat them as blocking. Go code: standard `gofmt`.

## Security gotchas

- Never bind to `0.0.0.0`. Use loopback or the tailscale IP. For non-loopback access set `UI_AUTH=user:pass`.
- `/api/file` reads arbitrary absolute paths as the running user — safe only on a private tailnet.
- Running lasso nested inside herdr requires `allow_nested = true` in `~/.config/herdr/config.toml`.
