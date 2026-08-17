# herdr-mirror on titan — verified facts (2026-08-17)

Installed and running today. Everything below was measured on this machine, not inferred.

## What it is

nikok6/herdr-mirror v0.3.0 (Rust), a herdr plugin. It mirrors remote herdr servers' workspaces and agents into the LOCAL sidebar. One `daemon` process (control plane: reconciles remote workspaces into local mirrors, pushes agent status) plus one `pane` process per mirror pane (data plane: streams that remote terminal over its own ssh connection).

- Plugin id: `mirror`. Actions: start, pause, status, once, restore, teardown, remote-new-workspace, remote-new-tab, remote-split-right, remote-split-down (+ `remote-invoke <plugin>.<action>` and `bind`/`unbind`/`remote-actions` from the CLI). Event hook: `workspace.focused` -> `herdr-mirror ensure` (autostart).
- CLI: `~/.local/bin/herdr-mirror` (symlink into `~/.config/herdr/plugins/github/mirror-00154761637c/target/release/herdr-mirror`).
- Config: `~/.config/herdr-mirror/hosts.toml`.
- State: `~/.local/state/herdr-mirror/` — `daemon.pid`, `daemon.log`, `<host>-map.json` (local<->remote object map), `<host>.ctl`, `<host>-api.sock`, `streamer-pids`.

## How a mirror appears to herdr (and therefore to lasso)

- A mirrored remote workspace is a REAL LOCAL workspace whose label is `"<hostkey>: <remote label>"`. The prefix is the host's `prefix` setting, defaulting to the hosts.toml key.
- Its pane's process is `herdr-mirror pane <ssh-target> <remoteWsId>:<remotePaneId> [--always-control] --ctl-path <state>/<host>.ctl --cols N --rows N`.
- `agent_status` on a mirror row is DAEMON-PUSHED from the remote's real agent state, not derived from the stream. Confirmed live: `ocai: clem` idle, `ticket500: stub` done, `52labs: fitty` idle, `norm: norm` idle, `visiquate: pepe` idle, plus blocked agents on gigachad/blackbird.
- Mirror rows have NO git chip: herdr derives sidebar git branch/ahead-behind from the local workspace cwd, and a mirror has no local cwd. The plugin's own README lists this as a limitation with no API to feed remote repo state.
- Custom sidebar metadata tokens the remote publishes ARE forwarded onto mirror rows (`herdr pane report-metadata ... --token "rcwd=$PWD"`), so `$rcwd`-style tokens work on mirrors.
- The plugin cannot render custom sidebar UI (plugin API limitation): it only sets the `<host>: ` label prefix and keeps rows ordered into per-host groups.

## The 9 mirrored hosts (all herdr 0.8.0 / protocol 19)

Full control (headless boxes): `ocai` (@clem), `norm` (@norm), `52labs` (fitty), `ticket500` (@stub + channel-host), `visiquate` (pepe).
Watch-only: `wistock` (someone else's box), `minime`, `gigachad`, `blackbird` (Macs with humans at them).

`norm-darren` was deliberately removed today, both from hosts.toml and as an ssh alias in ~/.ssh/config.

Current scale: 27 mirror workspaces / 32 mirror panes, 11 of them reporting real agent state. Steady-state cost ~35% of one core across 44 processes, 534 MiB RSS.

## Things that will bite you

- `minime` is mirrored through a second ssh alias `minime-mirror`, identical to `minime` minus a `LocalForward 9224`. So a mirror's ssh target is NOT necessarily the alias a human would recognize; map ssh target -> host key via hosts.toml, not by assuming they match.
- Workspace boxes (`ocai`, `norm`, `52labs`, `ticket500`, `wistock`) are reached through a cloudflared ProxyCommand and their herdr socket is `/dev/shm/herdr/herdr.sock`; the Macs and visiquate use `~/.config/herdr/herdr.sock`. Socket paths are discovered per host from `herdr status --json`.
- `close_remote_on_local_close = false` is set globally on titan: closing a mirror locally only stops mirroring and leaves the remote pane and its agent running. `herdr-mirror restore` brings closed mirrors back. Do not build UI that implies a local close destroys the remote.
- After a `systemctl --user restart herdr.service` the daemon self-heals ("mirror pane(s) mapped but not streaming (server restart?) — re-exec'ing streamers") — mirrors return without duplication and remotes are untouched. Verified with a real restart today.
- The daemon caches its host list at startup: editing hosts.toml requires `herdr-mirror pause && herdr-mirror start` to take effect. `herdr-mirror status` re-reads the config itself, so status can list hosts the running daemon does not know.

## Useful commands

```
herdr-mirror status              # daemon + per-host mirror counts + recent log
herdr workspace list             # labels (mirrors are "<host>: <label>"), agent_status, pane_count
herdr pane list --workspace <id>
herdr pane read <pane_id> --source visible --lines 8
cat ~/.local/state/herdr-mirror/<host>-map.json
```
