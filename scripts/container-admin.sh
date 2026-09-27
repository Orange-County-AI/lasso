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
# them by hand once nothing runs in them (`incus delete -f dev-lasso`).
set -euo pipefail

# Every container with a user.lasso.worktree key, plus the legacy names.
# Tab-separated: name, status, worktree ("-" for legacy), web mount source.
rows() {
  incus list -f json | jq -r '.[]
    | select(.config["user.lasso.worktree"] != null
             or (.name | test("^dev-lasso(-[0-9]+)?$")))
    | [.name, .status, (.config["user.lasso.worktree"] // "-"),
       (.devices.web.source // .expanded_devices.web.source // "-")]
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
    yes=""
    [ "${1:-}" = "-y" ] || [ "${1:-}" = "--yes" ] && yes=1
    n=0
    while IFS=$'\t' read -r name _ wt _; do
      # Legacy containers have no owner to check, and a path that exists means
      # a live worktree: both are skipped, which is the whole safety property.
      [ "$wt" != "-" ] && [ ! -d "$wt" ] || continue
      n=$(( n + 1 ))
      if [ -n "$yes" ]; then
        echo "deleting $name  (worktree gone: $wt)"
        incus delete -f "$name"
      else
        echo "would delete $name  (worktree gone: $wt)"
      fi
    done < <(rows)
    if [ "$n" = 0 ]; then
      echo "nothing to prune"
    elif [ -z "$yes" ]; then
      echo "dry run; re-run with -y to delete ($n container(s))"
    fi
    ;;
  *) echo "usage: $0 list | prune [-y]" >&2; exit 2 ;;
esac
