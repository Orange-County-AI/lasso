---
name: lasso
description: Use for lasso itself — inspecting and managing lasso agents, hosts, repos, and branches through its MCP server before falling back to lasso.db, the filesystem, or generic shell tooling. Also covers acting on your own identity inside a lasso-managed terminal (whoami / close_agent via $HERDR_PANE_ID), getting your human's attention with a push notification (notify / `lasso notify`), and driving the shared browser the human watches live in lasso's Browser tab (shared_browser / `lasso mcp shared-browser`, then chrome-devtools-mcp or Playwright over CDP).
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
> | `shared_browser`| Start the shared Chromium the human watches live and get its CDP endpoint |
>
> **No MCP client? Use `lasso mcp`.** Every tool above is also a shell command —
> `lasso mcp` lists them, `lasso mcp <tool> -h` shows one tool's flags, and
> `lasso mcp <tool> [flags]` calls it. Same server, same descriptions. See
> [From a shell: `lasso mcp`](#from-a-shell-lasso-mcp).
>
> The rest of this skill covers the common self-identity case; everything above
> applies to acting on **other** agents too.

# lasso self-identity

If you were spawned by [lasso](https://github.com/Orange-County-AI/lasso), you are running
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
  including your agent `id` and the `host` your pane lives on. Pane ids are only
  unique **per host**, so if the same pane id exists on several hosts, whoami
  refuses to guess (`found:false`) and names the candidate hosts — call it again
  with `host` set to the machine you actually run on (compare `hostname` against
  the labels from `list_hosts`).
- **`close_agent`** — call with the `id` **and `host`** `whoami` returned to
  shut yourself down (the long-hand of `lasso closeme`). Never guess the host:
  passing the wrong one (or another agent's id) kills an unrelated agent.

If `$HERDR_PANE_ID` is empty, you are not running under lasso and none of this
applies to you.

## Broadcasting what you're working on

Your pane has a status card in the herdr sidebar and lasso's ⌘K pane switcher. You can
put a live one-line summary on it so the human can see what you're doing
without opening your terminal:

```bash
herdr pane report-metadata "$HERDR_PANE_ID" --source agent:self \
  --token summary="migrating auth tests to vitest" --ttl-ms 1800000
```

Update it when you change phases (exploring → implementing → testing); the
TTL clears it automatically if you go quiet. Keep it under ~60 characters.

Do **not** use `herdr pane report-agent` — that claims lifecycle authority
over your pane, overriding herdr's own idle/working/blocked detection, and a
stale claim sticks if you exit uncleanly. `report-metadata` is display-only
and fails safe.

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
  resolved from `$HERDR_PANE_ID` (the CLI reads it for you). `-title` overrides
  it; a piped message works too: `make test 2>&1 | tail -3 | lasso notify`.

The **`notify`** MCP tool is the same call — pass `message` and your
`$HERDR_PANE_ID` as `pane_id`. Use the CLI unless you're already in an MCP
round trip; use the tool when you want the structured `sent` / `transports`
reply.

## The shared browser

lasso runs one real Chromium on **its own machine** and shows it live in the
sidebar's Browser tab. You can drive the same browser over the Chrome DevTools
Protocol, and the human sees every page you open, as you open it — and can
click in it too.

**Use it when a human should see the page**: checking a UI you just built,
reproducing a bug they reported, walking a flow they want to watch, anything
where "look at this" beats a screenshot. For headless scraping or a test suite
nobody is watching, your own Playwright/Chromium is fine.

### Getting the endpoint

```bash
lasso mcp shared-browser        # starts it if needed; prints ws_endpoint and pages
```

or the **`shared_browser`** MCP tool (pass `start: false` to only report its
state). The reply carries `ws_endpoint` (e.g. `ws://127.0.0.1:8090/cdp`), the
pages open right now, and a `note` when something needs saying — no Chromium
installed, or that `/cdp` needs your credential. The endpoint is stable across
browser restarts, so it is safe to put in a config. A refusal saying the
browser is outside your credential's reach is a scope boundary: don't work
around it.

### Connecting

- **chrome-devtools-mcp:** `npx chrome-devtools-mcp@latest --wsEndpoint
  <ws_endpoint>`; add `--wsHeaders '{"Authorization":"Bearer <token>"}'` when
  the note says `/cdp` needs credentials (the bearer token you use for
  `/mcp`; behind `UI_AUTH` alone, `Basic` credentials instead).
- **Playwright:** `chromium.connectOverCDP("<ws_endpoint>")` — then use
  `browser.contexts()[0]` (the shared, logged-in profile) and `newPage()` on
  it. Close your pages when done; never send CDP's `Browser.close`, which
  shuts the browser down for everyone. Playwright's own `browser.close()` is
  safe here: on a `connectOverCDP` connection it only disconnects.
- Plain CDP works too: `…/cdp/json/list` over HTTP lists the targets.

### Etiquette — a human may be watching

- **The human sees one page: the newest.** The Browser tab has no tab strip;
  it shows whichever page was opened most recently. Open a page with
  `new_page` / `newPage()` and the human's view follows you onto it, which is
  usually what you want ("watch me click through this"). The page they were
  on keeps running, just out of sight.
- **Don't navigate away a page you didn't open** unless the human asked you
  to work in it; it may be what they are reading. Open your own instead.
- **Close the pages you opened when you're done.** The view then falls back
  to another open page. Don't close pages you didn't open.
- **The profile is shared and may be logged in.** Cookies and logins persist
  in it. Treat any account it is signed into as the human's: reading is fine,
  but posting, sending, buying or changing settings needs their go-ahead.
- Don't restart or stop the browser, or change its proxy, to fix your own
  problem — that closes everyone's pages. Ask the human.

### `localhost` means lasso's machine

The browser runs where lasso runs, not necessarily where you do. If your dev
server is on the same machine as lasso, `http://localhost:5173` works. If you
are on another host, `localhost` inside the shared browser is **not** your
box: use an address lasso's machine can reach (your tailnet name or IP), and
make sure your server listens on it.

## Using the rest of the herdr CLI

herdr ships its own agent skill (`npx skills add herdrdev/herdr --skill
herdr`) covering pane orchestration — splitting panes, starting sibling
agents with `herdr agent start` / `prompt` / `wait`, and running commands with
`herdr pane run` / `wait-output`. Those all work from inside a lasso pane too
(you are in a herdr session; `HERDR_ENV=1` is set). Two lasso-specific rules
on top of it:

- Never `herdr pane close` yourself or any pane lasso created. `lasso
  closeme` (or the `close_agent` MCP tool) is the only sanctioned way to shut
  yourself down — it also cleans up lasso's agent record and staged prompt
  files, which a raw pane close leaves behind.
- Panes and agents you spawn directly via `herdr` are invisible to lasso's
  agent list (no record, no repo/branch, no close tracking). Prefer lasso's
  `create_agent` MCP tool when the new agent should show up as a first-class
  lasso agent; use raw herdr panes only for short-lived helpers.

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

## From a shell: `lasso mcp`

If you have a terminal but lasso's MCP server isn't configured as a client,
`lasso mcp` is the same surface as a command. It speaks MCP to the local
server, so there is no second implementation to drift and no extra
dependency — no `uvx`, no python, and it works on every host lasso is
installed on.

```bash
lasso mcp                                   # every tool, one line each
lasso mcp read-agent -h                     # one tool's flags
lasso mcp list-agents -host gigachad        # call it
lasso mcp list-agents | jq -r '.agents[].title'
```

- **Flags come from the server's live schema**, so they follow it
  automatically. `-h` on any tool is authoritative; don't guess a flag name.
- **Either spelling of a name works** — `read-agent` or `read_agent`,
  `-agent-id` or `-agent_id`.
- **An omitted flag is left out of the call entirely**, so the tool's own
  default applies. That matters: an empty `host` means "search every host I
  may reach", which is not the same as naming one.
- **Output is JSON** — indented on a terminal, compact when piped, so `| jq`
  needs no flag. `-json` gives the whole MCP envelope instead of just the
  structured result.
- **Exit codes:** `0` fine, `1` the tool itself refused (its message goes to
  stderr), `2` you typed something wrong.
- **A blocking tool's own timeout wins.** `lasso mcp wait-agent -agent-id X
  -timeout-ms 300000` waits the full five minutes; the CLI's transport
  deadline stretches to cover it. `-timeout <dur>` (before the tool name)
  sets the floor for everything else.

**`lasso skill` prints this skill** — the binary carries its own copy, so an
agent on a host with no checkout can read it (`lasso skill`) or install it
(`lasso skill > ~/.claude/skills/lasso/SKILL.md`).

`lasso notify` and `lasso closeme` stay the short spelling of calls this can
also make the long way — notify because its exit code is a contract and it
reads a piped message, closeme because it resolves `$HERDR_PANE_ID` for you.

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
