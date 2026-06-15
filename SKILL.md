---
name: lasso
description: Discover and act on your own identity when you are an agent running inside a lasso-managed terminal. Use when you need your own agent id, want to close yourself when your work is done (close_agent), fetch your own record (whoami / get_agent), or reference the workspace/repo/branch you were spawned into. Your identity is exported as LASSO_* environment variables, so you never need to enumerate list_repos / list_agents to find yourself.
---

# lasso self-identity

If you were spawned by [lasso](https://github.com/52labs/lasso), you are running
inside a tmux session that lasso created, and your own identity is already in
your environment. You do **not** need to call `list_repos` / `list_agents` and
guess which entry is you — that wastes tokens. Read it from the env instead.

## Your environment variables

| Variable             | Meaning                                                        |
| -------------------- | -------------------------------------------------------------- |
| `LASSO_TAB_ID`       | **Your agent id** — the value every self-targeting tool wants. |
| `LASSO_WORKSPACE_ID` | The workspace you belong to.                                   |
| `LASSO_REPO`         | Git agents only: the repo path (also just your cwd).           |
| `LASSO_BRANCH`       | Git agents only: the branch/worktree you're on.                |

Check whether you're a lasso agent at all by testing `LASSO_TAB_ID`:

```bash
echo "$LASSO_TAB_ID"   # empty/unset => you are NOT in a lasso-managed terminal
```

## Closing yourself when you're done

The easiest way to shut yourself down is the CLI — it reads `$LASSO_TAB_ID` for
you, so there's nothing to pass:

```bash
lasso closeme
```

That tells the running lasso server to kill your agent process and close your
tab (the same soft-close the UI and the `close_agent` MCP tool perform). If the
server runs on a non-default port, set `LASSO_LISTEN=host:port`.

## Acting on yourself via the lasso MCP tools

The lasso MCP server runs inside lasso's own process, **not your shell**, so it
cannot read your environment — you must pass `$LASSO_TAB_ID` yourself.

- **`whoami`** — pass `$LASSO_TAB_ID` as `tab_id` to get your own agent record.
- **`get_agent`** — equivalently, call with `$LASSO_TAB_ID` as the id.
- **`close_agent`** — call with `$LASSO_TAB_ID` to shut yourself down (the
  long-hand of `lasso closeme`).

If `$LASSO_TAB_ID` is empty, you are not running under lasso and none of this
applies to you.
