# herdr + file viewer (ttyd-in-iframe prototype)

A single Go binary that serves a two-column web UI:

- **Left** — `herdr` running inside a `ttyd` terminal, embedded in an `<iframe>`.
- **Right** — a file viewer that follows herdr's **focused pane** `cwd`, live.

The Go server reverse-proxies the terminal (WebSocket upgrade handled natively
by `httputil.ReverseProxy`) and talks to the herdr server over its
newline-delimited JSON unix socket — subscribing to `pane.focused` /
`tab.focused` / `workspace.focused` events and polling `pane.list` as a fallback
for plain `cd`s — then pushes active-pane state to the browser over SSE.

## Run

```bash
go build -o ttyd-iframe-demo .
./ttyd-iframe-demo            # spawns ttyd+herdr on loopback, serves UI on 127.0.0.1:8090
```

Open <http://localhost:8090>.

## Flags

| flag           | default                          | meaning                                      |
|----------------|----------------------------------|----------------------------------------------|
| `-listen`      | `127.0.0.1:8090`                 | web server address                           |
| `-ttyd-port`   | `7682`                           | loopback port for ttyd                       |
| `-term-cmd`    | `herdr`                          | command ttyd runs                            |
| `-herdr-sock`  | `~/.config/herdr/herdr.sock`     | herdr API socket                             |
| `-spawn-ttyd`  | `true`                           | spawn/supervise ttyd as a child              |
| `-poll`        | `2s`                             | fallback poll interval for cwd changes       |
| `-proc-cwd`    | `true`                           | resolve agent panes' real cwd via `/proc`    |
| `-insecure-no-auth` | `false`                     | allow a bare (no-auth) non-loopback bind     |

## Active-pane cwd tracking (and the agent-pane caveat)

herdr's `pane.cwd` is the **shell's** cwd (tracked via OSC 7). For plain shell
panes that's live and correct. But once an **agent** (e.g. `claude`) becomes the
pane's foreground process, the shell stops emitting OSC 7, so herdr's `cwd`
freezes at the shell's launch dir (often `/home/<user>`) — the file viewer would
then show the wrong directory.

herdr exposes no pane→PID key, so for agent panes the server recovers the real
cwd from `/proc`: it matches the herdr **tab label** against the agent process's
command line (whose first non-flag arg is the task title) and reads that
process's `/proc/<pid>/cwd`. The result is surfaced with a provenance badge:

- **`cwd ✓ process`** — real cwd, resolved from the agent process.
- **`⚠ shell cwd`** — agent pane the resolver couldn't pin (ambiguous/no match);
  showing herdr's stale shell dir. Use the path box.
- *(blank)* — a shell pane; herdr's cwd is trusted.

The UI always offers a manual **path box** and a **⟳ active** button, so a stale
guess is never a dead end. Disable the `/proc` step with `-proc-cwd=false`.

## Expose over Tailscale (plain HTTP, tailnet-only)

The left pane is a **writable shell**, so never bind to `0.0.0.0` (on a VPS that
is the *public* internet). Bind to the **tailscale interface IP** instead — only
your tailnet can reach it. Auth is required for any non-loopback bind (guard).

```bash
# tailnet-only, no auth (WireGuard already encrypts + authenticates the tailnet):
./ttyd-iframe-demo -listen "$(tailscale ip -4):8090" -insecure-no-auth

# or, to require a login as well, set creds via env (never argv) and drop the flag:
UI_AUTH="herdr:$(cat .authpass)" ./ttyd-iframe-demo -listen "$(tailscale ip -4):8090"
```

Then from any tailnet device: `http://<host>:8090/` (MagicDNS) — e.g.
`http://citadel:8090/`. No TLS needed; WireGuard already encrypts the tailnet.

## Security

- Binds to **loopback** by default; the terminal (ttyd) always stays on loopback.
- `UI_AUTH=user:pass` enables HTTP basic auth across the page, terminal, SSE and
  file APIs. The server **refuses to start** on a non-loopback `-listen` unless
  either `UI_AUTH` is set or `-insecure-no-auth` is passed — so it can never
  *accidentally* expose a bare writable shell on a public interface.
- `-insecure-no-auth` is intended only for a private interface (e.g. `tailscale0`),
  where the network itself provides authentication. Never use it on a public IP.
- `/api/file` reads any absolute path as the running user — fine on a private
  tailnet; confine it before widening access.

## Endpoints

- `GET /` — UI
- `/terminal/*` — reverse proxy to ttyd
- `GET /api/active` — current focused pane `{cwd, workspace, tab, agent, ...}`
- `GET /api/events` — SSE stream of active-pane changes
- `GET /api/files?path=` — directory listing
- `GET /api/file?path=` — file preview (2 MiB cap)
