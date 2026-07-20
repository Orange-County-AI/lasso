---
name: lasso
description: Discover and act on your own identity when you are an agent running inside a lasso-managed terminal. Use when you need your own agent id, want to close yourself when your work is done (close_agent), fetch your own record (whoami / get_agent), or reference the workspace/repo/branch you were spawned into. Your pane id is exported as $HERDR_PANE_ID, so you never need to enumerate list_repos / list_agents to find yourself.
---

# lasso self-identity

If you were spawned by [lasso](https://github.com/52labs/lasso), you are running
inside a herdr pane that lasso created, and your own identity is already in your
environment. You do **not** need to call `list_repos` / `list_agents` and guess
which entry is you — that wastes tokens. Read your pane id from the env instead.

## Your environment variable

| Variable         | Meaning                                                              |
| ---------------- | ------------------------------------------------------------------- |
| `HERDR_PANE_ID`  | **Your herdr pane id** (e.g. `p_82`) — what every self-targeting tool resolves you by. |

Check whether you're a lasso agent at all by testing `HERDR_PANE_ID`:

```bash
echo "$HERDR_PANE_ID"   # empty/unset => you are NOT in a lasso-managed herdr pane
```

## Closing yourself when you're done

The easiest way to shut yourself down is the CLI — it reads `$HERDR_PANE_ID` for
you, so there's nothing to pass:

```bash
lasso closeme
```

That tells the running lasso server to kill your agent process and close your
pane (the same soft-close the UI and the `close_agent` MCP tool perform). This
works even when you were spawned by a lasso on a *different* machine: the local
server finds the owning record on its peer and closes your pane locally. If the
server runs on a non-default port, set `LASSO_LISTEN=host:port` — it must stay
an address of the machine you run on.

## Acting on yourself via the lasso MCP tools

The lasso MCP server runs inside lasso's own process, **not your shell**, so it
cannot read your environment — you must pass `$HERDR_PANE_ID` yourself.

- **`whoami`** — pass `$HERDR_PANE_ID` as `pane_id` to get your own agent record,
  including your agent `id`.
- **`close_agent`** — call with the `id` `whoami` returned (its `agent_id`) to
  shut yourself down (the long-hand of `lasso closeme`).

If `$HERDR_PANE_ID` is empty, you are not running under lasso and none of this
applies to you.
