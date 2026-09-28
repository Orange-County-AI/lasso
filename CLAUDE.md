# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Lasso is a Go backend (`src/main.go` and friends) that serves a React/TypeScript SPA. All Go code lives under `src/` (the Go module root — `src/go.mod`). The frontend in `src/web/` is **built and embedded into the Go binary** — `go build` embeds `src/web/dist/`, so it must exist locally (run `mise run build`) but is **not** committed (it's gitignored). CI builds the frontend and produces release binaries.

## Commands

Backend (run from repo root, via [mise](https://mise.jdx.dev); Go sources are in `src/`):
- `mise run build` — builds the frontend then `go build` in `src/` (binary → `./lasso`)
- `mise run dev` — Vite dev server with HMR, proxying to the Go backend (requires tailscale up; auto-bumps the dev port from 8190 if busy)
- `mise run test` — `go test .` in `src/`
- `mise run typecheck` / `mise run lint` / `mise run check` — the frontend checks
- `mise run icons` — re-render the favicons and home-screen icons from the raster mascot in `docs/icon/`
- `mise run diagram` — render `docs/architecture/*.reladraw` to SVG, in the `sandbox` incus container
- `mise run dev:containers` — list the per-worktree dev containers, the worktree each belongs to, and whether it still exists
- `mise run dev:prune` — delete dev containers whose worktree is gone (dry run; `-y` to act). Never touches one whose worktree exists

**Every frontend command runs inside this worktree's own incus dev container, not on titan.** `bun install`, Vite, tsc and biome are all third-party code, and on the host that code would run as the logged-in user next to the SSH key, the 1Password session and the tunnel credentials. The mise tasks above handle this for you — see `scripts/container.sh`. Do **not** reach past them and run `bun` directly in `src/web/`; that puts the dependency tree back on the host and is exactly what the container exists to prevent. The Go half (`build`, `test`) still runs on the host: Go has no install hooks, and the binary has to be here anyway to drive herdr.

**One container per worktree**, named `dev-lasso-<worktree>-<hash of the src/web path>` and mounting only that worktree's `src/web` (plus `docs/icon` for `icons`). A single shared `dev-lasso` used to be re-pointed at whichever worktree ran a task last, which unmounted the tree under another worktree's running Vite (Rolldown "Failed to get current dir" → SIGABRT). `node_modules` lives in each worktree's own `src/web`; only bun's download cache is shared, via the `lasso-bun-cache` incus volume, so a new worktree's first task takes ~30-40s rather than a cold download. One `mise run dev` per worktree (a second is refused); several worktrees' dev servers run side by side, each on the next free tailnet port from 5173. `LASSO_DEV_CONTAINER` pins a name and brings the old sharing back with it. A container is deleted only once its worktree is gone: on titan the weekly `lasso-prune` timer (titan-iac `tools/lasso-prune.sh`) does it right after archiving stale worktrees, and `mise run dev:prune -y` does it on demand. The old shared `dev-lasso`/`dev-lasso-N` show as `legacy` and are never pruned; `incus delete -f` them by hand once nothing runs in them.

Underlying bun scripts, for reference (invoke via the mise tasks, not directly):
- `bun run dev` / `bun run build` (`tsc -b && vite build`)
- `bun run typecheck` — `tsc -b`
- `bun run lint` — `biome lint .`
- `bun run format` — `biome format --write .`
- `bun run check` — `biome check --write .` (format + lint fixes + import/class sorting)

## Frontend workflow

- Run `mise run typecheck` (`tsc -b` — the project references mean a bare `tsc --noEmit` checks *nothing*, since the root tsconfig has `files: []`) and `mise run lint` before considering frontend work done.
- `src/web/dist/` is the embedded bundle — gitignored and not committed. Run `mise run build` to regenerate it locally; CI builds it for releases.

## Formatting & linting

Tooling is **Biome** (`src/web/biome.json`) — it replaced Prettier + ESLint. Style: 2-space indent, no semicolons, double quotes, ES5 trailing commas, 80-col width. Tailwind class sorting is handled by Biome's `useSortedClasses` (aware of `cn`/`cva`). a11y rules are demoted to warnings (not previously enforced); don't treat them as blocking. Go code: standard `gofmt`.

## Security gotchas

- Never bind to `0.0.0.0`. Use loopback or the tailscale IP. For non-loopback access set `UI_AUTH=user:pass`.
- The file endpoints (`/api/file`, `/api/files`, and the write/rename/delete/upload trio) read and write arbitrary absolute paths as the running user — on the calling tab's host, or on any host lasso may drive when the request names one (`?host=` / a `host` field; see "File viewer host"). Safe only on a private tailnet.
- Running lasso nested inside herdr requires `allow_nested = true` in `~/.config/herdr/config.toml`.
- `/api/push/subscribe` records a URL lasso will POST to unattended, so it is gated by `UI_AUTH` like the rest of the app (deliberately NOT in `withAuthExcept`) and refuses anything that is not `https` with a valid P-256 key. A stored endpoint is a bearer capability to push to that device: it is never returned to a client and never logged — the Settings list and the logs both identify a device by a digest of it (`pushDeviceName`).
- `/cdp` (the shared browser, see "Browser tab and the shared browser") is full control of a real Chromium — every page, every login in its profile — and follows `/mcp`'s trust model: exempt from `withAuthExcept`, gated by `withCDPAuth` (open by default; UI_AUTH basic when set; `/mcp`'s bearer/basic rule plus a `local`-reach scope check when `MCP_OAUTH` is set). Because the proxy strips `Origin` for Chromium, `cdpOriginAllowed` must refuse a foreign Origin **before** anything else — it is the only thing keeping an arbitrary web page from driving a loopback lasso's browser.
- `/browser-mcp` (chrome-devtools-mcp bridged over HTTP, see "The browser MCP endpoint") is a front door to `/cdp` and is gated at least as strictly: exempt from `withAuthExcept`, `withBrowserMCPAuth` = the Origin guard, then `/mcp`'s bearer/basic rule plus the `local`-reach check under `MCP_OAUTH`, **UI_AUTH basic under UI_AUTH alone** (where `/mcp` would be open — `/cdp` is not), open with neither. Its children reach `/cdp` on `internalCDPToken`, which `withInternalCDP` honors ahead of every gate (the Access header gate included) and on `/cdp` paths only.
- The `/mcp` MCP server is **unauthenticated by default** (exempt from `UI_AUTH` via `withAuthExcept`) — it lets any client that can reach lasso spawn and drive agents. Same trust model as `/api/file`: safe only on loopback / a private tailnet, or behind an edge auth gate (e.g. Cloudflare Access). It introduces no new binding. Set `MCP_OAUTH` (below) to gate it in the origin instead.
- **Unless `MCP_OAUTH` is set**, the origin deliberately implements **no** OAuth (no `.well-known`, no `401`/`WWW-Authenticate`). So OAuth-based MCP clients (Claude Desktop / claude.ai connectors) connecting to `/mcp` over the public hostname require **Managed OAuth enabled on the Cloudflare Access app** — Access then acts as the OAuth 2.1 authorization server (Dynamic Client Registration + auth-code/PKCE), runs the login against the existing Access policy, and issues tokens; the origin still sees an authenticated Access session and needs no auth code. Without it the client's registration fails ("Couldn't register with lasso's sign-in service"). This is an edge setting on the Access application, not a `cloudflared`/tunnel change. `oauth.go` keeps every OAuth route 404 while `MCP_OAUTH` is unset precisely so this Access path stays intact — don't make that metadata unconditional.

## MCP OAuth (`src/oauth.go`)

`MCP_OAUTH=client_id:client_secret` (environment only, like `UI_AUTH` — never argv) turns lasso into a small OAuth 2.1 authorization server for its own `/mcp` resource. Unset = today's open `/mcp`, unchanged.

- **Grants**: `client_credentials` (machine-to-machine — scripts, CLIs, Claude Code; restricted to the `MCP_OAUTH` client and to per-host clients minted with `lasso mcp-client add`, never to an open-DCR registration) and `authorization_code` + PKCE/S256 with `refresh_token` rotation. claude.ai / Claude Desktop custom connectors **cannot** use `client_credentials` — Anthropic's connector infra requires `authorization_code`+`refresh_token` and per-connection user consent — so their "Advanced settings → OAuth Client ID / Client Secret" fields are a *pre-registered client for the auth-code flow*. That's why both grants exist.
- **Routes**: `/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server`, `/oauth/register` (open DCR), `/oauth/token` — all exempt from `UI_AUTH`, since they're the credential-less half of the handshake. `/oauth/authorize` is **deliberately gated** by `UI_AUTH`/Access: that human gate is what makes open DCR safe (anyone may register; nobody gets a token without passing the door and clicking Approve).
- `/mcp` accepts **either** a bearer token **or** the `UI_AUTH` basic credentials, so the CLI and existing callers keep working without an OAuth dance.
- Redirect URIs: DCR clients get exact matching. The pre-registered client accepts any https/loopback callback (a connector's callback isn't known when the secret is minted) and the consent screen displays the exact target; set `MCP_OAUTH_REDIRECT_URIS=<comma-separated>` to lock it to an allowlist.
- Codes and tokens live in `oauth_*` tables in `lasso.db`, stored as SHA-256 hashes, so refresh tokens survive restarts and self-updates.


## Design notes (`docs/design/`)

Each subsystem's full rationale lives in `docs/design/<topic>.md`. **Read the matching file before changing that subsystem**: most of the rules there exist because the obvious version was tried and broke. The one-liners below are only the invariants that are easiest to violate.

- **Per-tab hosts** → `per-tab-hosts.md` (`reqhost.go`, `hostfeed.go`, `switch.go`, `lib/host.ts`). The host is a property of the browser TAB (`X-Lasso-Host` / `?host=`, sessionStorage), never a process global; `POST /api/host` attaches, it switches nothing. One feed per watched host, stopped when idle (the default host is exempt). The `pane.list` cache is keyed per host.
- **Theme atmosphere, appearance, palettes, legibility** → `theme-atmosphere.md` (`index.css`, `lib/theme.ts`, `lib/wallpaper.ts`, `lib/mode.ts`, `db.go`, `legibility.go`, `agentsync.go`). Backdrop and appearance are SERVER state in `ui_state`, merged per theme and per field; no `localStorage` for them, and no import from the old keys. `applyAtmosphere` is the single repaint chokepoint. Translucency is `background-color` alpha, never `opacity`. Only `setMode`/`setPalettePref`/the OS flip push a palette to the fleet, never `refreshTheme`. Never send `appearance_mode: ""`.
- **Sidebar-layout ownership and the `ui_state` write pipeline** → `sidebar-layout.md` (`uilock.go`, `lib/sidebar.ts`, `lib/ui-state.ts`). `sidebar_collapsed`/`sidebar_pct` are arbitrated by an in-memory claim: only a client stamping `user_intent`, or the current owner within its lease, may move them. One `ui_state` save on the wire at a time.
- **Agent visibility scope and groups** → `agent-scope.md`, runbook `docs/mcp-agent-scope.md` (`hostscope.go`, `callerscope.go`, `groups.go`). Caller identity comes from the token, never an argument; `host` defaults to the caller's own host. Containment requires `MCP_OAUTH`.
- **Agent record reconciliation** → `agent-records.md` (`agentreap.go`). Records are tombstoned (`closed_at`), not deleted, and only from a successful, non-empty, per-host enumeration after `agentReapMisses` consecutive misses. Foreign herdr panes are never touched.
- **File viewer host, herdr machines, `open_file`** → `file-viewer-host.md` (`panehost.go`, `agentcwd.go`, `clientmachine.go`, `openfile.go`). The sidebar follows the FOCUSED pane's cwd and the host it lives on (ssh-attach and machine hops included). Every file endpoint takes a host; half a cutover means an editor saving to the wrong machine.
- **Chat view** → `chat-view.md` (`chatview.go`, `codexchat.go`, `transcriptlog.go`, `ChatView.tsx`, `AgentSidebar.tsx`, `AgentsTab.tsx`, `lib/agents.ts`). An overlay over a still-mounted terminal, read from transcript files, never the PTY. A pane id is never carried across a hop. Logs are paged line-by-line (`readLogPage`) because inlined images make single records huge.
- **The input dial** → `input-dial.md` (`lib/mobile-input-dial.ts`). Lives inside the ttyd document (keeps the iOS keyboard up). Mounts on coarse pointer OR a PARENT viewport below `md`, both watched, mounted once.
- **Browser tab, shared browser, profiles, `/cdp`, `/browser-mcp`, terminal links** → `shared-browser.md` (`browser.go`, `browserprofiles.go`, `cdpproxy.go`, `browsermcp.go`, `mcp_browser.go`, `mcp_browser_profiles.go`, `BrowserTab.tsx`, `LiveBrowser.tsx`, `lib/cdp.ts`). No Go websocket library; lasso proxies CDP and never parses it. `cdpOriginAllowed` runs before the Origin strip, always. One chrome-devtools-mcp child per MCP session, dialing lasso's own `/cdp` (or `/cdp/p/<id>` for a profile). A profile is its own Chromium process (proxy and cookies can't be per-context and persistent); the default profile keeps its old dir, setting and bare URLs. No `npx` fallback, never auto-`--no-sandbox`.
- **Notifications / Web Push** → `notifications.md` (`notify.go`, `webpush.go`, `notifywatch.go`, `sw.js`, `lib/push.ts`). Stdlib-only Web Push; the VAPID key must never change. Only 404/410 deletes a device. `sw.js` caches nothing and has no fetch handler. `notify` must report honestly whether anything took it.
- **`lasso mcp` CLI and `lasso connect`** → `mcp-cli.md` (`mcpcli.go`, `connect.go`). CLI flags are built from the schema the SERVER advertises, never a local table; omitted flags are absent, not zero. Test `connect` with `HOME`/`XDG_CONFIG_HOME` pointed at a temp dir, never a real home.

## Lasso orchestrates agents; it does not talk to them

The MCP surface creates, lists, inspects (record + live status), and closes agents, plus identity (`whoami`), `notify`, `open_file`, `shared_browser`, and the browser profile/tab tools (which manage which pages exist and which one the human sees — not a way to talk to an agent). It deliberately has **no** tool that prompts, reads, or waits on an agent: `send_agent`, `message_agent` (and its store-and-forward queue), `read_agent` and `wait_agent` were removed, and `get_agent` no longer returns terminal output. herdr already does that job (`herdr agent prompt` / `read` / `wait`), as does Claude Code's own agent messaging, and a second way to do it only taught agents to reach for lasso when herdr was the right tool. Don't add one back. Existing databases keep their now-unused `agent_messages` table on purpose — nothing creates it any more, and nothing drops it.

