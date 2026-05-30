# herdr + file viewer (ttyd-in-iframe prototype)

A single Go binary that serves a two-column web UI:

- **Left** — three tabs:
  - **herdr** (default) — `herdr` running inside a `ttyd` terminal, embedded in an
    `<iframe>`. The iframe stays mounted when you switch tabs, so the terminal
    never reconnects.
  - **Grid** — a grid of every herdr pane, grouped by workspace (click to focus it
    in the terminal, right-click to rename/close, ⌘/ctrl/shift-click to
    multi-select and bulk-close).
  - **Settings** — shows the installed herdr version and the latest published
    release. When an update is available, the **latest** pill is clickable: it
    opens the **Terminal** tab (see Right) with `herdr update` pre-typed (not
    submitted) so you can run it there — a real TTY is required for herdr to
    hand running sessions over to the new version live.
- **Right** — four tabs:
  - **Diff** (default) — the git diff of the focused pane's repo, in the spirit
    of Fulcrum's diff view. Shows working-tree changes (empty when the tree is
    clean), or flip **vs primary branch** to diff the whole branch against the
    primary branch instead. The tab carries an amber badge with the count of
    uncommitted changes whenever the tree is dirty.
  - **Files** — a file browser that follows herdr's **focused pane** `cwd` live;
    click a file to open it full-screen with rich markdown preview and syntax
    highlighting (see [File viewer](#file-viewer)).
  - **Browser** — an embedded `<iframe>` web preview with a URL bar, for viewing
    a dev server running in a pane.
  - **Terminal** — a plain shell (`$SHELL`, falling back to `bash`/`sh`) in its
    own `ttyd`, running *outside* any herdr session — the place to run
    interactive commands like `herdr update`. Absent with `-spawn-ttyd=false`.

  The right column is collapsible via the `»` button in the tab strip (the terminal
  then fills the width); a floating `«` button brings it back. The state persists
  in `localStorage`. The divider between the panes is also drag-resizable.

The Go server reverse-proxies the terminal (WebSocket upgrade handled natively
by `httputil.ReverseProxy`) and talks to the herdr server over its
newline-delimited JSON unix socket — subscribing to `pane.focused` /
`tab.focused` / `workspace.focused` events and polling `pane.list` as a fallback
for plain `cd`s — then pushes active-pane state to the browser over SSE.

## Run

```bash
go build -o herdr-viewer .
./herdr-viewer            # spawns ttyd+herdr on loopback, serves UI on 127.0.0.1:8090
```

Open <http://localhost:8090>.

### Development (`mise run dev`)

```bash
mise run dev    # builds, then serves on the tailscale IP (~:8090) with frontend hot reload
```

`mise run dev` binds to the tailscale interface (`tailscale ip -4`, no auth — see
[Expose over Tailscale](#expose-over-tailscale-plain-http-tailnet-only)) and runs
with `-dev`: `index.html` and `static/` are served **from disk** (not the embedded
copy), and a livereload client is injected — so editing `index.html` refreshes the
browser automatically, **no rebuild**. A full reload also reconnects the terminal
iframe (ttyd respawns the herdr client, which re-attaches to the persistent
server). Go changes still need a restart (`Ctrl-C`, rerun the task). Also `mise run
build` and `mise run test`.

Each instance spawns its **own** ttyds on private unix sockets, not shared TCP
ports — so a prod instance and any number of dev instances run side by side
without ever colliding on a port or proxying onto each other's terminal. There
are two: the herdr terminal (`$TMPDIR/herdr-viewer-ttyd-<pid>.sock`, proxied at
`/terminal/`) and the out-of-herdr shell behind the Terminal tab
(`$TMPDIR/herdr-viewer-shell-<pid>.sock`, proxied at `/shell/`). Both sockets are
removed on exit. (The `-ttyd-port` flag only applies with `-spawn-ttyd=false`,
where you point the proxy at an externally-run ttyd for the herdr terminal; the
shell terminal is viewer-spawned only and is absent in that mode.)

In `-dev` mode the requested **web** port **falls forward to the next free one**
if it's taken (`:8090` → `:8091` → …), so a second instance just lands on the next
slot — watch the `UI: http://…` log line for the URL it actually bound. Outside
`-dev` a busy web port is a hard error (a prod instance never moves silently).

## Flags

| flag           | default                          | meaning                                      |
|----------------|----------------------------------|----------------------------------------------|
| `-listen`      | `127.0.0.1:8090`                 | web server address                           |
| `-ttyd-port`   | `7682`                           | loopback port for an external ttyd (only used with `-spawn-ttyd=false`; a spawned ttyd uses a private unix socket) |
| `-term-cmd`    | `herdr`                          | command ttyd runs                            |
| `-shell-cmd`   | _(empty)_                        | command for the Terminal tab's shell (empty = `$SHELL`, then `bash`, then `sh`) |
| `-herdr-sock`  | `~/.config/herdr/herdr.sock`     | herdr API socket                             |
| `-spawn-ttyd`  | `true`                           | spawn/supervise ttyd as a child              |
| `-poll`        | `2s`                             | fallback poll interval for cwd changes       |
| `-proc-cwd`    | `true`                           | resolve agent panes' real cwd via `/proc`    |
| `-theme`       | `auto`                           | `auto` follows herdr's config, or force a theme name |
| `-insecure-no-auth` | `false`                     | allow a bare (no-auth) non-loopback bind     |
| `-dev`         | `false`                          | serve `index.html`/`static` from disk + livereload; fall forward to the next free web port if busy (run from repo root) |

## Theming (follows herdr)

Both panes adopt whichever theme you've selected in `~/.config/herdr/config.toml`
(`[theme].name`), so the demo matches herdr instead of hardcoding one palette.
herdr exposes no theme method on its socket, so the server resolves the theme
the same way herdr does: it reads the configured **name**, applies any
`[theme.custom]` per-token overrides, then the legacy `[ui].accent`. All of
herdr 0.6.4's built-ins are supported, dark and light:
`catppuccin`, `tokyo-night`, `dracula`, `nord`, `gruvbox`, `one-dark`,
`solarized`, `kanagawa`, `rose-pine`, `vesper`, `terminal`, `catppuccin-latte`,
`tokyo-night-day`, `gruvbox-light`, `one-light`, `solarized-light`,
`kanagawa-lotus`, `rose-pine-dawn` — plus herdr's alternate spellings (`latte`,
`dawn`, `lotus`, `tokyonight-day`, …). Unknown → catppuccin (herdr's default).

- The **sidebar** CSS variables come from herdr's own 16-token UI palette
  (transcribed from herdr's source), so the chrome matches herdr's chrome.
- The **terminal** (xterm.js, via ttyd `-t theme=`) gets its background /
  foreground / cursor from those same UI tokens (so the iframe blends with
  herdr) plus the scheme's **canonical 16-color ANSI palette** (so colored output
  inside panes — git, `ls`, agents — looks right).

Note herdr paints most of its own TUI chrome with explicit RGB, independent of
the xterm palette; the xterm theme mainly governs the default background and the
ANSI colors that programs running *inside* a pane emit. Force a specific theme
with `-theme <name>` regardless of config.

**Live updates.** The theme is re-resolved on the same poll that tracks the
active pane (`-poll`, default 2 s), so editing `[theme].name` in
`config.toml` repaints the UI **without a restart** — no need to even
`herdr server reload-config`. When the resolved palette changes the server
bumps a `theme_rev` on the SSE stream; the browser then fetches `/api/theme`
and (a) rewrites the injected `:root` variables — which cascades to the
sidebar, file viewer, diff and markdown — and (b) sets `term.options.theme`
on the embedded xterm.js instance (ttyd exposes it as `window.term`, and the
iframe is same-origin), so the **live terminal repaints in place** with no
reconnect and no lost scrollback.

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

## Pane grid (the Panes tab)

The right column's **Panes** tab lists every herdr pane as a card, grouped by
workspace, showing the tab label, cwd, and (for agent panes) the agent + status.
The focused pane is highlighted, and that highlight stays in sync with herdr
however focus changes — clicking a card, or navigating directly in the terminal
(the highlight follows the same SSE focus stream the Files view uses).

**Click** a card to focus that pane in the terminal. herdr's socket has **no
`pane.focus`** method, so the server focuses a pane by calling `workspace.focus`
then `tab.focus` (panes are one-per-tab in the common case; for a split tab this
focuses the tab the pane belongs to). The grid is fetched on tab open and via
its **⟳** refresh button.

The grid also **re-syncs live** to match herdr's layout. The server computes a
*layout signature* — the workspace order (each workspace's `number` + id +
label) plus pane→workspace/tab membership — and bumps a `panes_rev` counter on
the SSE stream whenever it changes. So when you **reorder workspaces** in herdr
(their `number`s flip, which the grid sorts by), or rename a workspace, or add/
remove a pane, the open grid reloads itself into the new order — no manual
refresh. The signature deliberately omits focus and cwd, so a plain focus move
just slides the highlight instead of rebuilding the grid. herdr's socket exposes
no workspace-reorder *method* (reordering happens in its TUI), so this is driven
by the `workspace.updated` event, with the 2 s poll as a backstop.

**Right-click** a card for a context menu:

- **Rename…** prompts for a new name and calls `tab.rename` — the cards are
  labeled by their *tab*, and `pane.rename` sets a pane name herdr never
  surfaces, so renaming the tab is what actually relabels the card (the change
  shows up in herdr's own tab bar too).
- **Close pane** closes that pane (`pane.close`); closing the last pane in a tab
  closes the tab.

**Multi-select** with ⌘/ctrl/shift-click (a plain click still just focuses).
Selected cards get an accent ring + ✓ badge, and a selection bar appears with
**Close selected** (and **Clear**). Bulk close calls `pane.close` per id,
**serialized with retries and a little pacing** so it's resilient to herdr's
reconfiguration races: closing a pane shifts focus / recomputes layout / may
close the tab, and firing the next close into that churn used to fail
transiently (the old "just retry it" flakiness). Now each pane is retried with
exponential backoff, and a pane that's **already gone** — e.g. cascade-closed
when its tab's last sibling was closed — counts as a successful close rather
than an error (herdr's `pane_not_found` is treated as idempotent success). Only
a pane that still can't be closed after retries is reported per-id. The
selection is also pruned to still-present panes on every refresh. Close is
**confirmed** first since it terminates the terminal — and any agent running in it.

## File viewer

Clicking a file in the **Files** tab opens it in a viewer that **fills the right
column** — the terminal stays visible on the left (← / ✕ / Esc returns to the
list), modeled on Fulcrum's replace-view editor:

- **Markdown** (`.md`, `.markdown`, …) renders as rich formatted HTML via
  [marked](https://marked.js.org), sanitized with
  [DOMPurify](https://github.com/cypress-io/dompurify) and themed to match herdr.
  A **Raw** toggle flips between the rendered view and the highlighted source.
- **Code / text** is syntax-highlighted with
  [highlight.js](https://highlightjs.org): the language is picked from the file
  extension (falling back to auto-detect), and the tokens are colored from the
  active herdr theme's palette — not a fixed highlight.js stylesheet — so the code
  matches everything else. Line wrapping is **on by default**; a **Wrap** toggle
  flips it (the choice persists in `localStorage`).
- **Images** render inline (on a checkerboard so transparency is visible).
- **↗** opens the raw file in a new browser tab.

These three libraries are **vendored** under `static/` and embedded into the
binary (`go:embed`), loaded lazily on the first file open — there's **no runtime
CDN dependency**, which suits a tailnet-only tool. Files over 2 MiB aren't
fetched (the server's preview cap); files over ~400 KiB are shown without
highlighting (a badge notes this) so a huge file doesn't hang the tab. Markdown
is sanitized because the viewer can open files from any repo, including untrusted
ones.

## Diff view (the Diff tab)

The **Diff** tab shows the git diff of the repository containing the focused
pane's `cwd` — it always follows the active pane. The server resolves the repo
root (`git rev-parse --show-toplevel`) from that directory and builds the diff
the way Fulcrum's diff view does:

- **Working-tree changes** (default) — staged (`git diff --cached`) + unstaged
  (`git diff`) only. When the tree is clean there's nothing to show (no files,
  no diff) — the branch comparison is opt-in, not a fallback.
- **vs primary branch** — toggle it on to diff the whole branch
  (`merge-base(base, HEAD)..HEAD`) against the primary branch (`origin/HEAD`,
  else `main`/`master`) — useful for reviewing everything a branch adds over
  `main`. A `vs <base>` pill marks this mode and **untracked** is disabled (there
  are no working-tree files to add).

Either way, the tab shows an amber badge with the **count of uncommitted
changes** (`git status --short`) whenever the working tree is dirty, so you can
tell there's local work even while looking at the branch diff.

The diff is parsed client-side into per-file blocks: each file is a collapsible
header with `+adds`/`−dels` counts (**collapsed by default** — click a header or
**expand all** to open them), and the lines are colored (added/removed/context/
hunk) in the active theme. Toolbar toggles: **vs primary branch**, **untracked**
(synthesizes an all-added diff for untracked files, which `git diff` omits),
**ignore whitespace** (`-w`, on by default), **wrap** (on by default), plus
**expand all** / **collapse all** and a **⟳** refresh. Large diffs are capped at
2 MiB (a `diff truncated` pill shows when that happens). The view refreshes on
tab open, on a cwd change, and on demand.

## Browser pane (the Browser tab)

The **Browser** tab is a plain `<iframe>` web preview with a URL bar
(navigate, **⟳** reload, **↗** open-in-new-tab) — the same approach Fulcrum
takes. The default URL is `http://<this-host>:3000`, where `<this-host>` is
whatever you reached the UI on (the tailscale IP or MagicDNS name, via
`location.hostname`) and `3000` is the usual dev-server port; the last URL you
visit is remembered in `localStorage`.

Two caveats, both inherent to iframing:

- **Reachability** — the iframe loads from *your* browser, over the tailnet. A
  dev server bound to the VPS's `localhost` won't be reachable; bind it to the
  tailscale IP (or `0.0.0.0`) so the tailnet can see it. This is why the default
  points at the tailscale host, not `localhost`.
- **Framing** — sites that send `X-Frame-Options: DENY` / a restrictive
  `frame-ancestors` CSP won't render in the iframe (use **↗** to open them in a
  real tab). Dev servers generally don't set these, so previews work.

## Expose over Tailscale (plain HTTP, tailnet-only)

The left pane is a **writable shell**, so never bind to `0.0.0.0` (on a VPS that
is the *public* internet). Bind to the **tailscale interface IP** instead — only
your tailnet can reach it. Auth is required for any non-loopback bind (guard).

```bash
# tailnet-only, no auth (WireGuard already encrypts + authenticates the tailnet):
./herdr-viewer -listen "$(tailscale ip -4):8090" -insecure-no-auth

# or, to require a login as well, set creds via env (never argv) and drop the flag:
UI_AUTH="herdr:$(cat .authpass)" ./herdr-viewer -listen "$(tailscale ip -4):8090"
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
- `/static/*` — vendored viewer libs (marked, highlight.js, DOMPurify), embedded
- `/terminal/*` — reverse proxy to ttyd
- `GET /api/active` — current focused pane `{cwd, workspace, tab, agent, ...}`
- `GET /api/events` — SSE stream of active-pane changes
- `GET /api/files?path=` — directory listing
- `GET /api/file?path=` — file preview (2 MiB cap)
- `GET /api/panes` — every pane with workspace/tab labels, agent, and focus state
- `POST /api/focus` — focus a pane `{workspace_id, tab_id}` (→ `workspace.focus` + `tab.focus`)
- `POST /api/rename` — rename a tab `{tab_id, label}` (→ `tab.rename`)
- `POST /api/close` — close panes `{pane_ids: [...]}` (→ `pane.close` each); returns `{closed, errors}`
- `GET /api/diff?path=&mode=&baseBranch=&ignoreWhitespace=&includeUntracked=` — git diff of the repo containing `path` (`mode=branch` forces the branch-vs-base comparison; `baseBranch` overrides the base it compares against); returns `{repo, branch, diff, files, isBranchDiff, baseBranch, truncated, dirty}`
