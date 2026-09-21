---
name: wrap
description: "Wrap up a finished feature in a lasso worktree: merge the current branch to local main, cut a release (bump version + push tag), run `lasso update` once the release publishes, then close this agent's own herdr pane. Use when the user says \"wrap\", \"/wrap\", \"wrap it up\", \"ship it\", or asks to finalize/land/release a completed feature from inside a lasso agent."
---

# wrap — land a finished feature, release, update, and close out

Run this from **inside a lasso agent** working in a git **worktree** whose branch
holds a *completed* feature. It takes that branch all the way to a published
release and then closes the agent. The final step kills this agent's terminal, so
everything else must succeed first.

End-to-end: **sync → verify → merge → bump → tag → wait for release → `lasso update` → close own herdr pane.**

**Assume the merge publishes itself.** Titan carries a GLOBAL
`post-commit`/`post-merge` hook (`core.hooksPath = ~/.config/git/hooks`) that
auto-pushes `main` to origin, so a commit or merge on `main` **is** the push — it
happens before you can look at the result, and there is no local staging window in
which to discover a red tree. Check once, and let the answer shape the run:

```bash
H=$(git config --get core.hooksPath)   # titan: ~/.config/git/hooks
ls "${H:-.git/hooks}"/post-merge 2>/dev/null && echo "merging main auto-pushes it"
```

(It is a GLOBAL config, so this answers the same from any worktree — no need to
wait for `$MAIN` below.)

Either way the ordering below is the same, because it is built not to need that
window: **everything is verified on the feature branch with main already merged
INTO it**, where nothing is published and a conflict costs nothing. The explicit
`git push origin main` in step 5 is then a no-op on titan and the real push on a
box without the hook. The tag is never auto-pushed by anything, and the GitHub
Release is what `lasso update` pulls from, so that push is always ours to make.

> **Layout note:** all Go code lives under `src/` (the Go module root), the
> frontend under `src/web/`, and the version source of truth is `src/version.go`.

`$ARGUMENTS` may name the bump type (`major` / `minor` / `patch`). Default `patch`.

## Preconditions — verify before touching anything

1. `feature=$(git -C . branch --show-current)`. Abort if it's `main` (nothing to wrap) or empty (detached HEAD).
2. If the working tree has uncommitted changes (`git status --porcelain` non-empty) that plainly belong to the feature being wrapped, **commit them and continue — don't stop to ask**. Stage everything and commit with a descriptive message summarizing the feature. Only pause to ask the user if the changes look unrelated or surprising (e.g. edits outside the feature's scope, or debris you didn't create).
3. Locate the main worktree (where `main` is checked out — `/home/stephan/projects/lasso` on titan, `/home/dev/projects/lasso` on a workspace box; derive it, never assume, and ignore the `prunable` worktrees whose paths belong to another machine):
   ```bash
   MAIN=$(git worktree list --porcelain | awk '/^worktree /{w=$2} /^branch refs\/heads\/main$/{print w}')
   ```
   Abort if empty.

## 1. Bring main INTO the feature branch first

This is the step that replaces the staging window the auto-push hook takes away.
Another agent lands on `main` constantly, and a merge git resolves cleanly can
still be **semantically** broken — this skill's own last run merged a test that
called a helper a branch landing in parallel had deleted, and the hook published
that the same second. Merging the other direction first puts the identical tree on
the feature branch, where a break is a local problem:

```bash
git fetch origin
git merge origin/main            # in THIS worktree, on the feature branch
```

Conflicts are resolved here, on the feature branch. If you can't resolve one with
confidence, `git merge --abort` and report.

## 2. Mirror CI locally (don't cut a red release)

The release workflow only publishes binaries if the build is green, and a red
`main` poisons every later agent. Run the same checks CI does **on the branch you
just merged main into** — that tree is what `main` is about to become, so this is
the check that actually protects it. From the worktree root:

```bash
mise run typecheck && mise run lint && ( cd src && go vet ./... && go test . )
```

**Never run `bun install` on the host.** Those mise tasks run the frontend half
inside the `dev-lasso` incus container (`scripts/container.sh`), which is the whole
point: `bun install` and Vite are the only third-party code in this repo, and on
the host they execute beside the SSH key, the 1Password session and — on a
workspace box — this org's entire exported credential set. Both boxes' install
guards refuse a bare `bun install` on the first attempt for exactly that reason.
The Go half stays on the host — Go has no install hooks, and the binary has to be
here anyway to drive herdr.

**A fresh `dev-lasso` on titan does NOT come up working today** (verified
2026-09-21), so budget for it rather than being surprised. Two independent breaks,
both in the container, neither in this repo's code:

- **No `raw.idmap`, so the bind mount is unwritable.** `container_needs_idmap`
  reads titan's `/etc/subuid` line `root:1000:1` as "the identity map already
  applies" and skips the flag, so `src/web` mounts `nobody:nogroup` and
  `bun install` dies with `EACCES ... could not create the "node_modules"
  directory`. Fix on the existing container and restart it:
  ```bash
  incus config set dev-lasso raw.idmap="both 1000 1000" && incus restart dev-lasso
  ```
- **No DHCP lease on `incusbr0`**, so the container cannot reach npm at all
  (`eth0` is up with no IPv4; `ConnectionRefused downloading tarball ...`). With no
  network there is no way to install, so seed `node_modules` from a checkout that
  already has one — a host-side copy, which executes nothing:
  ```bash
  cmp -s src/web/bun.lock "$MAIN/src/web/bun.lock" \
    && cp -a "$MAIN/src/web/node_modules" src/web/node_modules
  ```
  Only when the lockfiles match. If they differ the deps genuinely changed and the
  container needs its network back — stop and report rather than typechecking
  against the wrong tree.

If anything fails, **stop and report** — do not merge. Fix or hand back to the user.

## 3. Merge the feature into main

Work on the main worktree via `git -C "$MAIN"` so this agent's worktree is never
checked out elsewhere:

```bash
git -C "$MAIN" fetch origin
git -C "$MAIN" merge --ff-only origin/main          # sync main with remote first
git -C "$MAIN" merge --no-ff "$feature" -m "Merge $feature"
```

A `--no-ff` merge commit matches this repo's history (`Merge <branch>: …`).

**This merge is the publish on titan** — the `post-merge` hook pushes `main` the
instant it lands, and its output says so (`Merged into main branch, pushing to
origin... Successfully pushed to origin/main`). Two consequences:

- **A conflict here means step 1 was skipped or main moved since.** Abort
  (`git -C "$MAIN" merge --abort`), go back to step 1, and re-verify — don't
  resolve a conflict directly on `main`, where the resolution publishes itself.
- **A break found after this point is fixed FORWARD on `main`**, with its own
  commit. There is nothing to unwind: the bad tree is already on origin, and a
  revert is one more published commit than a fix.

If `origin/main` moved between step 1 and here (another agent landed), the
`--ff-only` sync brings in code step 2 never checked. Cheap insurance, on `main`,
before spending a version number on it:

```bash
( cd "$MAIN/src" && go vet ./... && go test . ) && ( cd "$MAIN" && mise run typecheck )
```

## 4. Bump the version (this is "publishing a release" step 1)

`src/version.go` holds the single source of truth (`lassoSemver`). The release
workflow refuses to publish unless the pushed tag equals it. Bump + commit on main
(the `mise run bump` task edits `src/version.go` and commits when given `--commit`):

```bash
( cd "$MAIN" && mise run bump "${ARGUMENTS:-patch}" --commit )
VER=$(grep -oP 'lassoSemver = "\K[0-9]+\.[0-9]+\.[0-9]+' "$MAIN/src/version.go")
```

## 5. Push main, then the tag (triggers the GitHub release)

On titan the hook has already pushed `main` (the bump commit too), so the branch
push below is an idempotent `Everything up-to-date`. Run it anyway: it is the real
push on a box without the hook, and it proves the remote is reachable before a tag
is cut that no release could be built from. **The tag is never auto-pushed** —
that one is always ours.

```bash
# Prove the remote is reachable BEFORE cutting a tag no release can be built from.
git -C "$MAIN" ls-remote origin -h refs/heads/main >/dev/null   # publickey failure → https fallback below
git -C "$MAIN" push origin main
git -C "$MAIN" tag "v$VER"
git -C "$MAIN" push origin "v$VER"     # this push is what fires .github/workflows/release.yml
```

**A box with no GitHub ssh key pushes over https through `gh`.** `origin` is ssh
(`git@github.com:Orange-County-AI/lasso.git`), and a workspace box typically holds
a `gh` token with `repo` scope but no deploy key — `ls-remote` then fails with
`Permission denied (publickey)`. Push through gh's credential helper rather than
rewriting the remote, and keep the token out of argv (`/proc/<pid>/cmdline` is
world-readable):

```bash
R=https://github.com/Orange-County-AI/lasso.git
git -C "$MAIN" -c credential.helper='!gh auth git-credential' push "$R" main
git -C "$MAIN" tag "v$VER"
git -C "$MAIN" -c credential.helper='!gh auth git-credential' push "$R" "v$VER"
```

If neither path works, **stop and report**. An unpushed tag publishes no release,
and `lasso update` against a missing release silently leaves the old version
running — which step 7's verification is the only thing that would catch.

## 6. Wait for the release to actually publish

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

## 7. lasso update — then restart the daemon via **whatever owns the process**

Clear the mise cache first so the new version is actually seen, then update:

```bash
mise cache clear
lasso update                  # swaps the release binary in place
```

**The binary may not be yours to write.** On a workspace box `/usr/local/bin/lasso`
is `root:root` out of the image, so `replaceSelf`'s `os.CreateTemp` in that
directory fails as the agent user with `replace binary: permission denied`. `sudo`
takes no password there and runs the identical code path, so on such a box the
update is `sudo lasso update`.

**`lasso update` atomically replaces the running binary (`replaceSelf` renames
the new bytes over `os.Executable()`) — it does NOT update mise metadata.** On
titan the daemon's exe resolves through the mise shim into a versioned install
dir (e.g. `installs/ubi-52labs-lasso/2.9.7/lasso`), so after an update the
directory name and the `~/.config/mise/config.toml` pin still claim the old
version while the bytes are the new release (verified 2026-08-17: dir named
2.9.7 served 2.9.11). Any later `mise install`/`upgrade`/`prune` on that tool
silently rolls prod back to the pinned version. Keep an **existing** pin honest —
and only an existing one:

```bash
case "$(readlink -f "$(command -v lasso)")" in
  */installs/*) mise use -g "ubi:52labs/lasso@$VER" ;;   # mise-managed (titan)
  *) : ;;                                                # image binary — no pin, see below
esac
```

**Never create a mise pin for lasso on a box that has none.** `/opt/mise/shims` is
first on PATH, so `mise use -g ubi:52labs/lasso@…` on a workspace box mints a shim
that permanently shadows the image's `/usr/local/bin/lasso`: the supervisor keeps
launching the image binary while every shell — and every later wrap — reads the
mise one.

**`lasso update` only auto-restarts a *pidfile-managed* daemon.** Under any
supervisor the built-in restart no-ops and the running daemon keeps serving the
**old** binary, so find the owner by asking the process, not the platform:

```bash
# The running server: its argv carries -listen under a supervisor or a unit. The
# pattern is anchored on the executable deliberately — a loose 'lasso .*-listen'
# also matches THIS shell (its own argv quotes the pattern) and `-n` then hands
# back the wrapper's pid, i.e. a SIGTERM aimed at yourself.
LPID=$(pgrep -nf '(^|/)lasso -listen')

if lasso status | grep -q running; then
  lasso restart                                          # pidfile daemon (dev) — update already did this
elif [ -n "${XDG_RUNTIME_DIR:-}" ] && systemctl --user is-active --quiet lasso.service; then
  systemctl --user restart lasso.service                 # systemd --user unit
elif systemctl is-active --quiet lasso.service 2>/dev/null; then
  sudo systemctl restart lasso.service                   # system unit
elif [ -n "$LPID" ] && ps -o args= -p "$(ps -o ppid= -p "$LPID" | tr -d ' ')" | grep -q supervise.sh; then
  sudo kill -TERM "$LPID"; sleep 6                       # supervise.sh child — its loop relaunches it
else
  echo "cannot identify what owns the running lasso — stop and report"; exit 1
fi
```

**On a workspace box `lasso restart` is the wrong answer, and it fails quietly.**
There is no `lasso.service` on those boxes at all — the only unit is
`workspace@<org>.service`, whose Main PID is `supervise.sh`, and lasso is a
backgrounded job of that script (`start_lasso`) — while `systemctl --user` cannot
even reach a bus (`$XDG_RUNTIME_DIR` and `$DBUS_SESSION_BUS_ADDRESS` unset), so a
`systemctl --user`-first branch falls straight through. `lasso restart` then finds
no pidfile, skips the stop, and **starts a second daemon** on `127.0.0.1:8090`
sharing `~/.lasso/lasso.db`, with its own ttyds, herdr clients, agent reaper and
notify watcher — while the supervised instance keeps serving the old binary. The
verification below would read that new rogue process, report `v$VER`, and close the
pane on a half-applied update. Hence the `supervise.sh` branch: SIGTERM the child
and let the supervisor relaunch it. That is also the only way it gets its
credential env (`/run/workspace-lasso/lasso.env`, never argv) and its real flags
back; the supervisor's loop picks it up within ~2s.

Then verify the *running* daemon picked it up (check the server, not the shell —
the shell PATH can read a staler binary). Take the address from the process,
because the default `127.0.0.1:8090` is not what a workspace box binds:

```bash
LPID=$(pgrep -nf '(^|/)lasso -listen')
LADDR=$(tr '\0' ' ' </proc/"$LPID"/cmdline | grep -oP '(?<=-listen )\S+')
LADDR=${LADDR:-${LASSO_LISTEN:-127.0.0.1:8090}}; LADDR=${LADDR/0.0.0.0/127.0.0.1}
curl -s "http://$LADDR/api/version"
```

A `403 forbidden: this lasso requires a Cloudflare Access identity` is not a
failed update: that box gates on the Access header, and nothing injects one into a
loopback request. Present an allowed identity and read the version:

```bash
EMAIL=$(sudo sed -n 's/.*LASSO_ACCESS_ALLOWED_EMAILS=//p' /etc/workspace/lasso-mode | tr -d "\"' " | cut -d, -f1)
curl -s -H "Cf-Access-Authenticated-User-Email: $EMAIL" "http://$LADDR/api/version"
```

Confirm it reports `v$VER`. If it still doesn't, do **not** close the pane — stop
and report. (Manual recovery: re-run the update with `sudo` if the binary is
root-owned, confirm `pgrep -nf 'lasso .*-listen'` is a *new* pid, and re-check
`/api/version`. A second lasso on `:8090` means the `lasso restart` branch ran by
mistake — `lasso stop` kills that one; never SIGKILL the supervised pid.)

## 8. Close this agent — do this LAST

Close **this agent's own herdr pane**. Herdr can perform this self-close
directly; no `close_agent` MCP call or lasso round-trip is required. This
terminates the terminal this agent is running in, so nothing after it runs. Only
reach here once steps 1–7 succeeded.

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
- **On a workspace box the binary swap sits outside the image pin, deliberately.**
  supervise.sh launches lasso with `-disable-self-update`, and its own comment says
  that flag "IS THE POINT OF THE IMAGE PIN": the Dockerfile asserts a sha256 for
  `/usr/local/bin/lasso`, so replacing those bytes makes "what is running here"
  unanswerable from the repo, and a runtime rebuild reverts the box to the pinned
  version. A wrap there is correct but temporary — persisting it is a pin bump in
  `titan-iac`, which no box can write from inside itself. Say so in the summary.
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
