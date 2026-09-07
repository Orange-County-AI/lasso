---
name: lasso
description: Use for lasso itself — inspecting and managing lasso agents, hosts, repos, and branches through its MCP server before falling back to lasso.db, the filesystem, or generic shell tooling. Also covers acting on your own identity inside a lasso-managed terminal (whoami / close_agent via $LUVUS_PANE_ID) and getting your human's attention with a push notification (notify / `lasso notify`).
---

# lasso

> **Start here for lasso itself.** Use the **lasso MCP server** to inspect
> and manage lasso agents, hosts, repos, and branches.
>
> Reach for these purpose-built tools **before** falling back to the
> `lasso.db` sqlite file, the filesystem, or generic shell tooling — those
> are last resorts, not the front door.
>
> | Tool | What it does |
> | ---- | ------------ |
> | `list_agents`   | List lasso agents |
> | `get_agent`     | Fetch one agent's record/metadata |
> | `read_agent`    | Read an agent's terminal output / transcript |
> | `wait_agent`    | Block until an agent reaches a state (e.g. idle/done) |
> | `create_agent`  | Spawn a new first-class lasso agent |
> | `close_agent`   | Shut an agent down |
> | `list_hosts`    | List hosts lasso knows about |
> | `list_repos`    | List repos available to spawn agents into |
> | `list_branches` | List branches for a repo |
> | `whoami`        | Resolve your own agent record |
> | `notify`        | Push a notification to the human running lasso |
>
> The rest of this skill covers the common self-identity case; everything above
> applies to acting on **other** agents too.

# lasso self-identity

If you were spawned by [lasso](https://github.com/Orange-County-AI/lasso), you are running
inside a Luvus pane that lasso created, and your own identity is already in your
environment. You do **not** need to call `list_repos` / `list_agents` and guess
which entry is you — that wastes tokens. Read your pane id from the env instead.

## Your environment variables

| Variable         | Meaning                                                              |
| ---------------- | ------------------------------------------------------------------- |
| `LUVUS_PANE_ID`  | **Your Luvus pane id** (a decimal string, e.g. `7`) — what every self-targeting tool resolves you by. |
| `LUVUS_ENV`      | `1` inside any Luvus pane. Set for every pane, lasso-created or not. |

Check whether you're a lasso agent at all by testing `LUVUS_PANE_ID`:

```bash
echo "$LUVUS_PANE_ID"   # empty/unset => you are NOT in a lasso-managed Luvus pane
```

## Closing yourself when you're done

The easiest way to shut yourself down is the CLI — it reads `$LUVUS_PANE_ID` for
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
cannot read your environment — you must pass `$LUVUS_PANE_ID` yourself.

- **`whoami`** — pass `$LUVUS_PANE_ID` as `pane_id` to get your own agent record,
  including your agent `id` and the `host` your pane lives on. Pane ids are only
  unique **per host**, so if the same pane id exists on several hosts, whoami
  refuses to guess (`found:false`) and names the candidate hosts — call it again
  with `host` set to the machine you actually run on (compare `hostname` against
  the labels from `list_hosts`).
- **`close_agent`** — call with the `id` **and `host`** `whoami` returned to
  shut yourself down (the long-hand of `lasso closeme`). Never guess the host:
  passing the wrong one (or another agent's id) kills an unrelated agent.

If `$LUVUS_PANE_ID` is empty, you are not running under lasso and none of this
applies to you.

## Naming your pane

Luvus pane names are addressable handles, not free-form progress messages. Use a
short, stable name only when one is useful:

```bash
luvus pane name auth-tests --pane "$LUVUS_PANE_ID"
```

Names must match `[a-z][a-z0-9_-]{0,31}`. Keep the handle stable so other callers
can address it. `--clear` removes it.

Do **not** use `luvus agent report` — that publishes a *leased authoritative*
state, overriding Luvus's own idle/working/blocked detection for your pane, and a
lease remains until it expires or is released (`luvus agent release`). It is an
integration API for a harness that owns the lifecycle, not a progress message.

## Getting your human's attention

A status card only helps if someone is looking at a screen. When you actually
need the human — a decision only they can make, a question that blocks you, a
long job finishing while they're away — push them a notification:

```bash
lasso notify "vitest is green across all 3 packages — want me to open the PR?"
```

It reaches their phone with the screen off (lasso as an iOS home-screen app), so
treat it as a real interruption:

- **Only when you need them.** An agent that pings on every step trains its
  human to ignore every ping. Blocking on an approval needs no notification at
  all — lasso already watches for that and notifies on its own.
- **Say what you need, not that you need something.** It's read on a lock
  screen: *"the migration drops 2 columns — safe to run on prod?"*, not
  *"please check lasso"*.
- **Check the outcome.** The command exits **non-zero** and prints why when no
  device is registered — nothing was delivered, so don't tell your human you
  notified them. (Same signal in the MCP tool's `sent` field.)
- The notification is titled with **your** agent name and opens on your host,
  resolved from `$LUVUS_PANE_ID` (the CLI reads it for you). `-title` overrides
  it; a piped message works too: `make test 2>&1 | tail -3 | lasso notify`.

The **`notify`** MCP tool is the same call — pass `message` and your
`$LUVUS_PANE_ID` as `pane_id`. Use the CLI unless you're already in an MCP
round trip; use the tool when you want the structured `sent` / `transports`
reply.

## Using the rest of the Luvus CLI

Luvus ships its own agent skill (`luvus skill show` prints the bundled,
version-matched copy; `luvus skill enable` installs it into the agent hosts it
detects) covering pane orchestration — splitting panes, starting sibling agents
with `luvus agent start` / `agent prompt` / `wait agent-status`, and running
commands with `luvus pane run` / `wait output`. All of it works from inside a
lasso pane too (you are in a Luvus session; `LUVUS_ENV=1` is set, and
`LUVUS_PANE_ID` is your default split anchor). Two lasso-specific rules on top
of it:

- Never `luvus pane close` yourself or any pane lasso created. `lasso closeme`
  (or the `close_agent` MCP tool) is the only sanctioned way to shut yourself
  down — it also cleans up lasso's agent record and staged prompt files, which a
  raw pane close leaves behind.
- Panes and agents you spawn directly via `luvus` are invisible to lasso's agent
  list (no record, no repo/branch, no close tracking). Prefer lasso's
  `create_agent` MCP tool when the new agent should show up as a first-class
  lasso agent; use raw Luvus panes only for short-lived helpers.

## Inspecting and managing lasso agents

Use the lasso MCP tools to inspect agent state and manage agent lifecycles:

- **List:** `list_agents` (who's running), `list_hosts` / `list_repos` /
  `list_branches` (where they can run), `get_agent` (one agent's record).
- **Inspect:** `read_agent` to read another agent's terminal output/transcript.
- **Wait for state:** `wait_agent` to block until an agent reaches a state
  (e.g. idle/done).
- **Manage:** `create_agent` to spawn a first-class lasso agent, `close_agent`
  to shut one down.

These are the canonical, purpose-built path. Only drop to reading `lasso.db`
directly or shelling out when a tool genuinely can't express what you need.

## Scope: which agents you can actually see

Your view of the fleet may be **bounded by the credential you authenticate
with** — not by what exists. What to expect:

- `list_hosts` / `list_agents` return only the hosts **your credential may
  address**: your own host, plus any hosts the operator grouped yours with.
  An empty-ish listing is usually containment working as intended, not an
  outage.
- The `host` argument of every tool **defaults to your own host** — the one
  your credential was issued for — not to the machine lasso runs on.
- **Direction matters.** Another caller may be able to inspect your host
  while you cannot inspect theirs. Don't infer reciprocal access from your
  own credential's reach.
- A refusal like *"this credential may not address host …"* is a policy
  boundary, not a transient error. Do **not** retry, work around it via ssh,
  or read `lasso.db` to peek past it. If you genuinely need the reach, tell
  the human — widening is an operator action (`lasso mcp-group` /
  `lasso mcp-client`), never something you can do from your side.
- Group membership can change live: reach you lacked a moment ago may appear
  on your next call without any reconnect.
