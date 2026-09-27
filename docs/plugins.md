# Writing a lasso plugin

A plugin adds to lasso in any of four ways:

- **Sidebar tabs**: a web page of its own in lasso's right-hand sidebar, next to Files, Browser and Settings.
- **MCP tools**: tools that show up on lasso's `/mcp` server as `<plugin>__<tool>`, so every agent connected to lasso can call them. `lasso mcp` lists them too.
- **Themes**: Omarchy-format palettes that become ordinary lasso themes (see [Themes](#themes)).
- **Fonts**: font files that become choices in Settings' Typography section (see [Fonts](#fonts)).

A plugin's code runs in a **microsandbox microVM** by default, not on your machine. Nothing a plugin ships runs until you enable it, and enabling it approves exactly the permissions lasso shows you.

A complete working example is in [`examples/plugins/hello`](../examples/plugins/hello): one tab and a stdlib-only Python MCP server. [`examples/plugins/harbor`](../examples/plugins/harbor) is an appearance-only plugin: one theme and one font.

## Layout

```
<LASSO_DIR or ~/.lasso>/plugins/
  hello/
    plugin.json        the manifest (required)
    ui/index.html      a tab's page (any files you like)
    server.py          the MCP server (anything that speaks MCP over stdio)
```

A plugin gets there in one of three ways (see [Installing and sharing](#installing-and-sharing)):

- **From GitHub**: `lasso plugin install owner/repo[/subdir]`, or Settings → Plugins → Install from GitHub.
- **Linked**: `lasso plugin link <path>` uses a checkout where it is, without copying it. This is the way to develop one.
- **By hand**: put its directory there. The directory name **is** the plugin's name.

```sh
cp -r examples/plugins/hello ~/.lasso/plugins/
lasso plugin list
lasso plugin enable hello
```

## The manifest

```json
{
  "name": "hello",
  "version": "0.1.0",
  "description": "one line",
  "min_lasso_version": "3.6.0",
  "platforms": ["linux", "darwin"],
  "tabs": [
    { "id": "main", "label": "Hello", "icon": "sparkles", "entry": "ui/index.html" },
    { "id": "docs", "label": "Docs", "icon": "book", "url": "https://example.com" }
  ],
  "mcp": {
    "image": "python:3.12-slim",
    "command": ["python3", "-u", "server.py"],
    "network": ["api.example.com:443"],
    "env": { "LOG_LEVEL": "info" },
    "secrets": [ { "name": "EXAMPLE_TOKEN", "hosts": ["api.example.com"] } ]
  }
}
```

| field | |
|---|---|
| `name` | Required. `^[a-z][a-z0-9-]{0,31}$`, and it must equal the directory name. |
| `version`, `description` | Shown in Settings. |
| `min_lasso_version` | Optional `X.Y.Z`. On an older lasso the plugin is `invalid` with "needs lasso >= X (this is Y)". A development build (`-dev`) always passes. |
| `platforms` | Optional list of `linux` and `darwin` (`macos` is accepted for `darwin`). On any other OS the plugin is `invalid` with the reason. |
| `tabs[].id` | `^[a-z][a-z0-9-]{0,31}$`, unique within the plugin. The tab's global id is `plugin:<name>:<id>`. |
| `tabs[].label` | 1-64 characters. |
| `tabs[].icon` | A name from lasso's curated set: `activity bell book book-open bot box calendar chart clock cloud code cpu dashboard database file-text flask folder gauge git-branch globe hammer heart image layers link list mail map message music notebook package puzzle rocket search server shield sparkles star terminal` (see `PLUGIN_ICONS` in `src/web/src/lib/plugins.ts`). An unknown name falls back to a puzzle piece and is never an error. |
| `tabs[].entry` | A file inside the plugin directory, served by lasso. Relative, with no `..` and no hidden segments. |
| `tabs[].url` | An `http(s)` URL, framed as-is. A tab has exactly one of `entry` or `url`. |
| `mcp.image` | Required with `mcp`. The OCI image the sandbox boots. |
| `mcp.command` | Required with `mcp`. The argv of a stdio MCP server. It runs with the plugin directory as its working directory, mounted read-only at `/plugin`. |
| `mcp.network` | The only hosts the server may reach, each `host[:port]` (the port defaults to 443). `*.example.com` covers subdomains. Empty or absent means **no network at all**. |
| `mcp.env` | Plain, non-secret environment variables. |
| `mcp.secrets` | Secrets the server needs, each with the hosts it may be sent to. |
| `themes` | Up to 32 themes. See [Themes](#themes). |
| `fonts` | Up to 16 fonts. See [Fonts](#fonts). |

Neither `min_lasso_version` nor `platforms` is a permission, so neither is in the fingerprint.

An invalid manifest never loads anything. The plugin is listed as `invalid` with the reason, in Settings and in `lasso plugin list`.

## Approval and trust

A manifest is written by whoever wrote the directory, so nothing in it can grant itself anything. You grant it, in Settings → Plugins or with `lasso plugin`.

- **A new plugin is disabled.** Enabling it (Settings shows the exact permissions in a dialog first) approves the permissions shown: its tabs' entries and URLs, the image, the command, the network allowlist, the env variable names, each secret with the hosts it may go to, its theme ids, and its fonts' ids, families and categories. lasso stores a fingerprint of that set in its own database, never in the plugin directory.
- **If a later edit changes any of those**, the plugin reads as `needs_approval`. Its tabs and its MCP server stop loading until you approve the new set. Changes to the version, the description, a tab's label or icon, a theme's label or palette, a font's license, or the font files themselves do not need re-approval. lasso rescans the directory every 10 seconds, and on every Settings visit, so an edit is noticed without a reload.
- **Trusted** runs the MCP server directly on your machine, as your user, outside the sandbox. It is a flag in lasso's database that only you can set (`lasso plugin trust <name>`, or the Settings toggle). A manifest field cannot set it. Even a trusted server gets a minimal environment (PATH, HOME, locale, XDG directories) plus its own `env` and secrets. It never gets lasso's `UI_AUTH`, `MCP_OAUTH` or `LASSO_MCP_TOKEN`.

```sh
lasso plugin list [-json]
lasso plugin enable|disable|trust|untrust|restart <name>
lasso plugin reload
```

These talk to the running lasso (found through `LASSO_URL` or `LASSO_LISTEN`, and sending `UI_AUTH` if it is set). Enabling a plugin starts a process, and only the server can do that.

## Installing and sharing

```sh
lasso plugin install <source> [--ref R] [-y] [--no-enable]
lasso plugin update <name> [-y]
lasso plugin uninstall <name> [--purge-data]
lasso plugin link <path> [--enable]
lasso plugin unlink <name>
lasso plugin log <name> [-n 200] [-f]
lasso plugin data-dir <name>
```

**Install** takes `owner/repo`, `owner/repo/sub/dir`, or `https://github.com/owner/repo[/tree/<ref>/sub/dir]`. Only GitHub is supported. `--ref` pins a branch, a tag or a commit. lasso shallow-clones the repository into a hidden staging directory (`plugins/.staging/`), with git hooks off and submodules not fetched. It deletes `.git`, and refuses a checkout over 50 MB or 5000 files, or one whose plugin directory holds a symlink pointing outside it. Then it validates the manifest exactly as the scanner does, and shows you every permission, the source and the exact commit. Nothing is installed until you confirm. "Install and enable" approves exactly the fingerprint the preview showed; if the staged manifest no longer matches it, the confirm is refused and you preview again. A preview you do not confirm expires after 10 minutes. The plugin lands in `plugins/<name>/` (the name comes from its manifest), and lasso records its source, ref and commit. Without a terminal, `install` refuses unless you pass `-y`.

**Update** re-fetches the recorded source and ref, shows the same preview plus the current commit, and says whether the permissions change. Confirming swaps the new directory in; if that fails, the old one is put back. Unchanged permissions keep their approval. Changed ones read `needs_approval` until you approve them.

**Uninstall** removes a GitHub install's directory and forgets its approval and trust. It keeps the plugin's data directory unless you pass `--purge-data`. A hand-placed plugin is never deleted by lasso: delete the directory yourself.

**Link** registers a local checkout (an absolute path holding `plugin.json`) by the name in its manifest. It is served and mounted from that path, and edits there take effect on the next rescan. **Unlink** forgets it and leaves the files alone. A link whose path disappears is listed as `invalid` with the reason. A name can only belong to one plugin: install and link refuse a name that is already installed, linked or hand-placed.

`lasso plugin list` shows each plugin's source: `github owner/repo@abc1234`, `linked <path>`, or `local`.

### Sharing a plugin

Put `plugin.json` at the root of a public GitHub repository (or in a subdirectory, and install it as `owner/repo/sub/dir`), and tag the repository with the GitHub topic **`lasso-plugin`** so people can find it. A topic is self-applied and nobody reviews it. The listing is not what keeps you safe: the approval dialog and the microVM are.

## Data directory

The plugin directory is read-only to the plugin. Its one writable place is `<LASSO_DIR or ~/.lasso>/plugin-data/<name>/` (mode 0700, created when its MCP server first starts). In the sandbox it is mounted read-write at `/data`, and `LASSO_PLUGIN_DATA=/data`. A trusted server gets `LASSO_PLUGIN_DATA=<the host path>`. Keep state there, not in the plugin directory: an update replaces the plugin directory, and uninstall keeps the data directory unless asked. `lasso plugin data-dir <name>` prints the path.

## Tabs

Each tab lands in the sidebar's strip before Settings. You choose which tabs show and in what order, built-in and plugin alike, in Settings → General → Sidebar. That layout is lasso's `ui_state` (`sidebar_tabs`), so every browser on the same lasso follows it. Settings itself cannot be hidden, and a hidden Files or Browser tab still opens when something needs it (an agent opening a file, a terminal link) until you pick another tab. A disabled plugin's entries are kept in the layout, so re-enabling it puts its tab back where it was.

A tab with an `entry` is served from `/plugins/<name>/<path>` and framed in the sidebar. Every response carries

```
Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads
```

That makes your page an **opaque origin**, even if someone opens its URL directly. Your scripts run, but they cannot read lasso's cookies or call lasso's API. That restriction is deliberate: lasso's file endpoints read and write any path on the machine, so a page that could call them would own the machine. Files are served `no-cache`, so an edit shows on the next load. Hidden files (`.env`) and symlinks that point outside the plugin directory are never served.

A tab with a `url` is framed as-is, under the same iframe `sandbox`, and the site must allow framing. It gets **no bridge**: it is someone else's site, and it has no business calling your plugin's tools.

A plugin tab mounts the first time it is selected and stays mounted after, like the built-in tabs, so switching away and back does not reload your page.

### The bridge

A page reaches lasso by posting messages to its parent. The protocol is `lasso-plugin/1`:

```js
// request
window.parent.postMessage({ lasso: 1, id: 7, method: "context.get", params: {} }, "*")
// response
{ lasso: 1, id: 7, result: { ... } }      // or { lasso: 1, id: 7, error: "message" }
// pushes, sent once after load and again whenever the value changes
{ lasso: 1, event: "context", data: { ... } }
{ lasso: 1, event: "theme", data: { ... } }
```

The parent answers only the frame the message came from, and routes each request by that frame. Plugin A can never make a call as plugin B. Pushes are posted with target origin `"*"` (an opaque origin cannot be named), so they carry nothing secret.

| method | params | result |
|---|---|---|
| `context.get` | | `{ host, cwd, cwd_host, pane_id, agent, plugin, tab }`, the focused pane |
| `theme.get` | | `{ dark, colors: { background, foreground, ... } }` |
| `file.open` | `{ path, line?, host? }` | Opens the file in the Files viewer, the same way an agent's `open_file` does. Unsaved edits are protected. `host` defaults to `cwd_host`. |
| `tool.call` | `{ tool, arguments }` | Calls one of **this plugin's own** MCP tools and returns its `CallToolResult`. `tool` may be un-prefixed (`greet`) or prefixed (`hello__greet`). Another plugin's tool is refused. |
| `toast` | `{ message }` | A short toast, prefixed with the plugin's name. |

Any other method answers `error: "unknown method"`.

## Themes

```json
"themes": [
  { "id": "harbor-night", "label": "Harbor Night", "dir": "themes/harbor-night" }
]
```

| field | |
|---|---|
| `id` | `^[a-z][a-z0-9-]{0,47}$`, unique within the plugin. It is the theme's key everywhere: in the theme picker, in herdr's `config.toml`, and on every host the theme is synced to. |
| `label` | Optional, at most 64 characters. Defaults to the id title-cased. |
| `dir` | A directory inside the plugin (same path rules as a tab's `entry`) holding an [Omarchy theme](https://github.com/basecamp/omarchy): `colors.toml` (or a legacy `alacritty.toml`), an optional `light.mode` marker, and an optional `backgrounds/` of `.jpg`/`.jpeg`/`.png`/`.webp`/`.avif` wallpapers. The palette must parse, or the manifest is invalid. |

An enabled plugin's theme is a first-class lasso theme. It goes through the same registry as the official Omarchy themes and the ones installed from a URL, so it paints the chrome and the terminals, can be herdr's theme or an appearance palette, is written into every agent CLI's theme file, syncs across the fleet, and its wallpapers show in the backdrop gallery (served under `/omarchy/bg/<id>/<file>`, read through the plugin directory so a symlink out of it is never followed).

**Existing themes win.** The order is lasso's built-ins, then the official Omarchy themes, then themes installed from a URL, then plugins. A plugin theme whose id is already taken, or is another spelling of a taken one, is **skipped**, not an error: the rest of the plugin loads, and the listing carries a warning (`key_taken` on the theme, a line in `warnings`), shown in Settings and `lasso plugin list`. When two plugins claim one id, the plugin whose name sorts first wins and the other gets the warning.

Palettes are re-read on every rescan (every 10 seconds), so an edit to `colors.toml` shows up without a reload. If the edited theme is the one herdr is wearing, lasso rewrites herdr's config and re-syncs the fleet as if you had picked it again. Disabling the plugin withdraws the theme; a selection naming it is kept, and lasso keeps painting the palette it last wrote to herdr's config, the same as for a theme only a newer lasso knows.

## Fonts

```json
"fonts": [
  {
    "id": "space-mono",
    "family": "Space Mono",
    "category": "mono",
    "faces": [
      { "file": "fonts/space-mono-latin-400-normal.woff2", "weight": 400, "style": "normal" },
      { "file": "fonts/space-mono-latin-700-normal.woff2", "weight": 700, "style": "normal" }
    ],
    "license": "OFL-1.1"
  }
]
```

| field | |
|---|---|
| `id` | `^[a-z][a-z0-9-]{0,31}$`, unique within the plugin. The font's global id is `plugin:<name>:<id>`. |
| `family` | `^[A-Za-z0-9 _-]{1,64}$`. The CSS family name lasso declares the faces under. |
| `category` | `sans`, `serif`, `display` or `mono`. |
| `faces` | 1-8 faces. `file` is a file inside the plugin ending in `.woff2`, `.woff`, `.ttf` or `.otf`, at most 5 MB; `weight` is 100-900 in steps of 100; `style` is `normal` or `italic`. |
| `license` | Optional, at most 64 characters, shown in Settings. Ship the license text in the plugin too. |

Face files are served from `/plugins/<name>/<file>` with their font Content-Type, and only while the plugin is enabled. Each font becomes an option in Settings' **Typography** section, one choice per slot:

| slot | where it applies | offers |
|---|---|---|
| Interface (`sans`) | body and UI text | every font |
| Display (`display`) | display headings | every font |
| Labels (`label`) | the small all-caps labels | every font |
| Code (`mono`) | code, the file viewer and editor, diffs | `mono` fonts only |
| Terminal (`terminal`) | every terminal | `mono` fonts only |

The choice is lasso's `ui_state.typography` (`{sans, display, label, mono, terminal}`, each a global id or absent), so every browser on the same lasso follows it. Each slot is saved on its own, so two devices changing two slots do not overwrite each other. Your family always goes first, with the slot's usual stack behind it, so a missing glyph or a slow load falls back to the normal look. The terminal keeps its Nerd Font as the fallback, so the icons TUIs draw still render. If a plugin is disabled, slots naming its fonts fall back to lasso's default and come back when it is re-enabled.

### Why no CSS

A plugin cannot ship a stylesheet, and that is deliberate. CSS that could restyle lasso could also hide the warnings in the plugin approval dialog or paint a fake prompt over the terminal. So a plugin supplies values, and lasso writes every line of CSS itself from values it has checked. The family regex is what keeps a family name from breaking out of the `@font-face` rule it is written into. Colours come from a palette lasso parses into its own theme model. There is no field that reaches the page as raw CSS.

## MCP tools

`mcp.command` must be an MCP server over **stdio**: newline-delimited JSON-RPC on stdin/stdout, with logs on stderr. lasso keeps one long-lived instance per plugin. It lists the tools and mirrors each onto `/mcp` as `<plugin>__<tool>`, with the description prefixed `[plugin <name>] `. Arguments and results pass through untouched. A mirrored name must fit `^[a-zA-Z0-9_-]{1,64}$`, and a tool whose input schema is not an object is skipped, with a line in lasso's log.

- Plugin tools run on lasso's machine. A caller whose MCP credential does not reach lasso's own machine (a per-host `self`-scoped client for another host) is refused.
- If the server dies, lasso removes its tools and restarts it with backoff (1s, doubling to 60s), then re-adds them. lasso pings a running server every 10 seconds, because a server that dies inside a microVM does not always close the connection visibly.
- A server that cannot start for a reason a retry will not fix goes `unavailable` until you act. The two causes are no `msb` on the machine and a secret that did not resolve. Fix the cause, then use Restart (or `lasso plugin restart <name>`).

## The sandbox

An untrusted plugin's server runs as

```
msb run --name lasso-plugin-<name> --no-tty \
  --net-default-egress deny --net-default-ingress allow \
  [--net-rule allow@dns --net-rule allow@<host>:tcp:<port> ...] \
  -p 127.0.0.1:<port>:7700 \
  --mount-file <lasso binary>:/opt/lasso:ro --mount-dir <plugin dir>:/plugin:ro \
  --mount-dir <data dir>:/data -w /plugin \
  [-e K=V ...] -e LASSO_PLUGIN_DATA=/data [--secret NAME@host,host ...] \
  <image> -- /opt/lasso plugin-stdio-serve -listen :7700 -- <command...>
```

- **Isolation.** The server has its own kernel and filesystem. The plugin directory is read-only, its data directory is the only writable mount, and nothing else from your machine is mounted. Egress is denied except to the approved hosts, and DNS is allowed only when there is some host to resolve.
- **Transport.** `msb run` does not forward piped stdin, so stdio cannot cross the VM boundary. lasso mounts its own static binary into the guest and runs `lasso plugin-stdio-serve`, a small adapter that bridges one TCP connection on a published loopback port to a fresh copy of your server's stdin/stdout.
- **Secrets never enter the guest.** lasso resolves each approved secret from its own environment, or failing that from `secret NAME` (10s timeout). It passes the value only to the `msb` process. Inside the guest, `NAME` holds a placeholder that msb replaces with the real value only in traffic to that secret's approved hosts.
- **Logs.** `msb run` holds the guest's output until the sandbox exits, so your server's stderr does not stream into lasso's log. When a server fails to start or dies, lasso fetches the last lines with `msb logs` and includes them in the status and the log. `lasso plugin log <name>` (or Settings' Logs button) shows the recent output at any time; `-f` follows it (`msb logs -f lasso-plugin-<name>` underneath). A trusted server's stderr is kept in memory, the last 500 lines.
- **Cost.** A cold boot takes about 2-3 seconds. The first boot of an image also pulls it, and lasso allows two minutes for that.

msb is found through `LASSO_MSB` (a path or a name on PATH, or `off` to disable sandboxed plugins), then `msb` on PATH, then `~/.microsandbox/bin/msb`. Without it, tabs still work, and every untrusted MCP server reads `unavailable: microsandbox (msb) not found`.

## HTTP API

These are for the Settings pane and the CLI. They are behind `UI_AUTH` like the rest of the UI.

| | |
|---|---|
| `GET /api/plugins` | `{ dir, msb: {available, path, reason}, plugins: [...] }`. Each plugin carries `themes: [{id, label, key_taken?}]`, `fonts: [{id, global_id, family, category, license?, faces?: [{url, weight, style}]}]` (`faces` only while enabled), `warnings: [string]`, and `permissions.themes` / `permissions.fonts`. |
| `POST /api/plugins/reload` | Rescan the directory and return the listing. |
| `POST /api/plugins/<name>/enable` | Approve the current permissions. The optional body `{fingerprint}` is refused with 409 if the manifest has changed since that listing; the Settings dialog always sends the fingerprint of what it showed. |
| `POST /api/plugins/<name>/disable` | |
| `POST /api/plugins/<name>/trust` | `{trusted: bool}` |
| `POST /api/plugins/<name>/restart` | |
| `POST /api/plugins/<name>/call` | `{tool, arguments}`. The tool must be this plugin's own. Returns the `CallToolResult`. |
| `POST /api/plugins/install/preview` | `{source, ref?}` → `{token, name, version, description, source, ref, commit, fingerprint, permissions, themes, fonts, warnings}`. Clones into staging; installs nothing. |
| `POST /api/plugins/install/confirm` | `{token, fingerprint, enable}` → the listing. 409 if `fingerprint` is not the staged manifest's; 404 for an unknown or expired token. |
| `POST /api/plugins/install/cancel` | `{token}` → `{ok: true}`. Discards an install **or update** preview. |
| `POST /api/plugins/link` | `{path, enable?}` → the listing. `enable` approves the manifest as read at that moment. |
| `POST /api/plugins/<name>/unlink` | Linked plugins only. |
| `POST /api/plugins/<name>/uninstall` | `{purge_data?}`. GitHub installs only. |
| `POST /api/plugins/<name>/update/preview` | The install preview plus `current_commit` and `changes_permissions`. GitHub installs only. |
| `POST /api/plugins/<name>/update/confirm` | `{token, fingerprint}` → the listing. 409/404 as for install. |
| `GET /api/plugins/<name>/log?lines=200` | `{name, sandboxed, lines: [string], note?}`. `note` says why there is nothing (no msb, no sandbox yet). |

Each plugin in the listing also carries `source: {kind: "github"|"linked"|"local", source?, ref?, commit?, installed_at?, updated_at?, path?, url?}` (`url` is the GitHub tree at the installed commit) and `data_dir`. Refusals other than those named (a bad source, a failed clone, a name that is taken, the wrong kind of plugin for the operation) are 400 with the reason as the body.

Every change bumps `plugins_rev` in the `/api/events` frames, so every open tab refetches the listing. It also bumps when the plugin themes change (a palette edited in place included), which is what makes browsers refetch the theme catalog (`GET /api/omarchy-themes`, where a plugin theme has `source: "plugin"` and `plugin: "<name>"`).
