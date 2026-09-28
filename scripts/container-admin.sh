#!/usr/bin/env bash
# List and prune the per-worktree dev containers (see scripts/container.sh).
#
#   usage: container-admin.sh list
#          container-admin.sh prune [-y]
#
# A container belongs to the worktree recorded in its user.lasso.worktree key
# at creation. Worktrees come and go faster than anyone deletes containers, so
# `prune` removes the ones whose worktree directory no longer exists. It is a
# dry run unless given -y, and it never touches a container whose worktree is
# still on disk, whatever state it is in — another agent may be mid-task in it.
#
# The old shared containers (dev-lasso, dev-lasso-2, ...) carry no such key:
# their mount followed whichever worktree ran last, so it says nothing about
# who is using them. They are listed as `legacy` and never pruned here; delete
# them by hand once nothing runs in them (`isb rm dev-lasso`).
#
# Both halves go through isb, which reads the lasso.worktree label (stored as
# user.lasso.worktree) that scripts/isb/dev.yaml sets.
set -euo pipefail

command -v isb >/dev/null 2>&1 || {
  echo "error: isb is not on PATH. Install it with:" >&2
  echo "  mise use -g \"cargo:https://github.com/execution-associates/isb@branch:main\"" >&2
  exit 1
}

# Every container with a lasso.worktree label, plus the legacy names.
# Tab-separated: name, status, worktree ("-" for legacy), web mount source.
rows() {
  isb -q ls --json | jq -r '.[]
    | select(.labels["lasso.worktree"] != null
             or (.name | test("^dev-lasso(-[0-9]+)?$")))
    | [.name, .status, (.labels["lasso.worktree"] // "-"),
       (.devices.web.source // "-")]
    | @tsv'
}

cmd="${1:-list}"; shift || true
case "$cmd" in
  list)
    { printf 'CONTAINER\tSTATUS\tWORKTREE\tON DISK\n'
      rows | while IFS=$'\t' read -r name status wt web; do
        if [ "$wt" = "-" ]; then
          printf '%s\t%s\t%s\t%s\n' "$name" "$status" "legacy, last mounted ${web%/src/web}" "n/a"
        else
          [ -d "$wt" ] && on=yes || on=GONE
          printf '%s\t%s\t%s\t%s\n' "$name" "$status" "$wt" "$on"
        fi
      done
    } | column -t -s $'\t'
    ;;
  prune)
    # isb only ever considers a container carrying the label, and only deletes
    # one whose labelled path is gone: legacy containers have no owner to
    # check, and a path that exists means a live worktree. Both are skipped,
    # which is the whole safety property. Dry run unless -y.
    yes=()
    [ "${1:-}" = "-y" ] || [ "${1:-}" = "--yes" ] && yes=(-y)
    isb prune --label lasso.worktree --missing-path "${yes[@]}"
    ;;
  *) echo "usage: $0 list | prune [-y]" >&2; exit 2 ;;
esac
