---
name: wrap
description: "Wrap up a finished feature in a lasso worktree: merge the current branch to local main, cut a release (bump version + push tag), run `lasso update` once the release publishes, then close this agent's own herdr pane. Use when the user says \"wrap\", \"/wrap\", \"wrap it up\", \"ship it\", or asks to finalize/land/release a completed feature from inside a lasso agent."
---

# wrap — land a finished feature, release, update, and close out

Run this from **inside a lasso agent** working in a git **worktree** whose branch
holds a *completed* feature. It takes that branch all the way to a published
release and then closes the agent. The final step kills this agent's terminal, so
everything else must succeed first.

End-to-end: **merge → bump → push → tag → wait for release → `lasso update` → close own herdr pane.**

This repo has **no auto-push git hook** — merging to local `main` does *not* reach
GitHub on its own, and the GitHub Release is what `lasso update` pulls from. So we
push `main` and the tag explicitly.

> **Layout note:** all Go code lives under `src/` (the Go module root), the
> frontend under `src/web/`, and the version source of truth is `src/version.go`.

`$ARGUMENTS` may name the bump type (`major` / `minor` / `patch`). Default `patch`.

## Preconditions — verify before touching anything

1. `feature=$(git -C . branch --show-current)`. Abort if it's `main` (nothing to wrap) or empty (detached HEAD).
2. If the working tree has uncommitted changes (`git status --porcelain` non-empty) that plainly belong to the feature being wrapped, **commit them and continue — don't stop to ask**. Stage everything and commit with a descriptive message summarizing the feature. Only pause to ask the user if the changes look unrelated or surprising (e.g. edits outside the feature's scope, or debris you didn't create).
3. Locate the main worktree (where `main` is checked out — normally `/home/stephan/projects/lasso`):
   ```bash
   MAIN=$(git worktree list --porcelain | awk '/^worktree /{w=$2} /^branch refs\/heads\/main$/{print w}')
   ```
   Abort if empty.

## 1. Mirror CI locally (don't cut a red release)

The release workflow only publishes binaries if the build is green, and a red
`main` poisons every later agent. Run the same checks CI does, on the feature
branch, **before** merging. From the worktree root:

```bash
( cd src/web && bun install --frozen-lockfile && bun run typecheck && bun run lint ) \
  && ( cd src && go vet ./... && go test . )
```

If anything fails, **stop and report** — do not merge. Fix or hand back to the user.

## 2. Merge the feature into main

Work on the main worktree via `git -C "$MAIN"` so this agent's worktree is never
checked out elsewhere:

```bash
git -C "$MAIN" fetch origin
git -C "$MAIN" merge --ff-only origin/main          # sync main with remote first
git -C "$MAIN" merge --no-ff "$feature" -m "Merge $feature"
```

A `--no-ff` merge commit matches this repo's history (`Merge <branch>: …`). If the
merge conflicts, abort it (`git -C "$MAIN" merge --abort`) and report — don't guess.

## 3. Bump the version (this is "publishing a release" step 1)

`src/version.go` holds the single source of truth (`lassoSemver`). The release
workflow refuses to publish unless the pushed tag equals it. Bump + commit on main
(the `mise run bump` task edits `src/version.go` and commits when given `--commit`):

```bash
( cd "$MAIN" && mise run bump "${ARGUMENTS:-patch}" --commit )
VER=$(grep -oP 'lassoSemver = "\K[0-9]+\.[0-9]+\.[0-9]+' "$MAIN/src/version.go")
```

## 4. Push main, then the tag (triggers the GitHub release)

```bash
git -C "$MAIN" push origin main
git -C "$MAIN" tag "v$VER"
git -C "$MAIN" push origin "v$VER"     # this push is what fires .github/workflows/release.yml
```

## 5. Wait for the release to actually publish

`lasso update` pulls from the GitHub Release, which takes a few minutes to build
and upload. Running update too early silently re-installs the *old* version (the
mise `ls-remote` cache compounds this). So **wait for the release + its assets**:

```bash
# poll until the release exists AND a linux-amd64 binary asset is attached
for i in $(seq 1 60); do
  if gh release view "v$VER" --repo Orange-County-AI/lasso --json assets \
       -q '.assets[].name' 2>/dev/null | grep -q lasso-linux-amd64; then
    echo "release v$VER published"; break
  fi
  sleep 15
done
```

If it never appears, check the run: `gh run list --repo Orange-County-AI/lasso --workflow release.yml`.
Don't proceed to update against a missing/failed release.

## 6. lasso update — then restart the daemon via **its supervisor**

Clear the mise cache first so the new version is actually seen, then update:

```bash
mise cache clear
lasso update        # swaps the release binary in place
```

**`lasso update` atomically replaces the running binary (`replaceSelf` renames
the new bytes over `os.Executable()`) — it does NOT update mise metadata.** On
titan the daemon's exe resolves through the mise shim into a versioned install
dir (e.g. `installs/ubi-52labs-lasso/2.9.7/lasso`), so after an update the
directory name and the `~/.config/mise/config.toml` pin still claim the old
version while the bytes are the new release (verified 2026-08-17: dir named
2.9.7 served 2.9.11). Any later `mise install`/`upgrade`/`prune` on that tool
silently rolls prod back to the pinned version. Keep the pin honest — re-pin to
the version just released:

```bash
mise use -g "ubi:52labs/lasso@$VER"
```

**`lasso update` only auto-restarts a *pidfile-managed* daemon.** When lasso is
run under a supervisor (prod is a systemd `--user` unit), the built-in restart
no-ops and the running daemon keeps serving the **old** binary — `/api/version`
then stays stale and closing out would land on a half-applied update. So
restart explicitly via whatever owns the process:

```bash
if systemctl --user is-active --quiet lasso.service 2>/dev/null; then
  systemctl --user restart lasso.service          # prod: systemd --user unit
else
  lasso restart                                    # dev / unsupervised (pidfile)
fi
```

Then verify the *running* daemon picked it up (check the server, not the shell —
the shell PATH can read a staler binary). Default prod listen is `127.0.0.1:8090`;
override via `$LASSO_LISTEN`:

```bash
curl -s "http://${LASSO_LISTEN:-127.0.0.1:8090}/api/version"
```

Confirm it reports `v$VER`. If it still doesn't, do **not** close the pane —
stop and report. (Manual recovery: `mise upgrade lasso` then restart via the
supervisor above, and re-check `/api/version`.)

## 7. Close this agent — do this LAST

Close **this agent's own herdr pane**. Herdr can perform this self-close
directly; no `close_agent` MCP call or lasso round-trip is required. This
terminates the terminal this agent is running in, so nothing after it runs. Only
reach here once steps 1–6 succeeded.

```bash
herdr pane close "$HERDR_PANE_ID"   # confirm with `herdr pane current` if unset
```

Never close a pane that isn't this agent's.

## Summary to print before closing

Right before closing the pane, tell the user what happened: merged `<feature>` →
`main`, released `v<VER>`, ran `lasso update` (now serving `v<VER>`), and closing
the agent. After the pane closes the connection drops — that's success, not an error.

## Notes / gotchas

- Each step is checked: if a command fails, **stop and report** rather than barrelling
  to the close. A half-finished wrap that still closed the agent is the worst outcome.
- The agent's terminal is a herdr pane; herdr is a separate daemon from lasso, so
  the pane survives the `lasso update` daemon restart and updating mid-wrap is safe.
- **wrap never touches the herdr binary.** `lasso update` only swaps the lasso
  binary and restarts the lasso daemon — it does not install, pin, or replace
  herdr, and lasso resolves the `herdr` client via `PATH`. A custom/forked herdr
  (e.g. a local build in `~/.local/bin`) is left exactly as-is.
  - Caveat: what *can* shift the lasso↔herdr relationship is the release's code
    itself. If the feature being wrapped changes `lassoHerdrProtocol` (in
    `src/main.go`), the new lasso may become incompatible with a pinned/forked
    herdr. For a purely frontend or non-protocol change it's unchanged. When in
    doubt: `git -C "$MAIN" diff HEAD~1 -- src/main.go | grep lassoHerdrProtocol`.
- Bump type override: `/wrap minor` → minor bump. No arg → patch.
