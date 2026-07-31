# MCP agent scope — who an agent can see, and how to provision it

Operator runbook for the per-host MCP credentials. The *why* and the code map
live in `CLAUDE.md` under "Agent visibility scope"; this is how to run it.

## The model in one paragraph

Two bounds decide which agents an MCP caller can see and message, and both
apply. **What lasso can address** (`src/hostscope.go`) is the local box plus the
concrete aliases in the ssh config lasso reads — membership comes from the
config, never from the agents db, so a host whose alias was removed stops being
addressable instead of resolving to a target no call can reach. **What this
caller may address** (`src/callerscope.go`) is per-credential: one OAuth client
per host, the host riding `auth.TokenInfo` into every tool call, so a caller's
host is derived from its token and cannot be asserted by the caller. Scope is
`self` (that host only, the default) or `fleet` (everything lasso can address);
**groups** (`src/groups.go`) add reach to named sets of *other* hosts on top of
`self`, which is the middle ground between those two ends.

MCP itself offers nothing here — as of revision 2025-11-25 `clientInfo` is a
self-asserted name/version/title and no header identifies the caller — which is
why identity comes from a credential rather than an argument.

## Prerequisite: `MCP_OAUTH` must be set

**Without it none of this bites.** `withMCPAuth` is a no-op while `MCP_OAUTH` is
unset (`src/oauth.go`), so `/mcp` is open, every caller is unidentified, and
unidentified means fleet-scoped. Provisioned per-host credentials are inert;
startup logs a warning when that combination exists.

On titan (systemd `--user`, secret injected from fnox so it is never plaintext
at rest):

```bash
printf 'lasso-mcp:%s' "$(openssl rand -base64 32)" | fnox set MCP_OAUTH -g

mkdir -p ~/.config/systemd/user/lasso.service.d
cat > ~/.config/systemd/user/lasso.service.d/20-mcp-oauth.conf <<'EOF'
[Service]
# MCP_OAUTH is environment-only (never argv). The bare ExecStart= clears the
# inherited line — Type=simple accepts exactly one.
ExecStart=
ExecStart=/home/stephan/.local/share/mise/shims/fnox exec -- /home/stephan/.local/share/mise/shims/lasso -listen 127.0.0.1:8090
EOF
systemctl --user daemon-reload && systemctl --user restart lasso.service
```

`parseAuth` splits on the first colon, so a base64 secret is fine. Note that
`fnox exec` injects *every* global secret into lasso's environment; if you want
the daemon's env narrow, use `EnvironmentFile=` with a `0600` file holding only
`MCP_OAUTH` instead.

Verify on loopback, which sidesteps Cloudflare Access entirely:

```bash
journalctl --user -u lasso -n 20 | grep 'mcp:'      # "OAuth on — client_id=lasso-mcp, ..."
curl -s -u "$MCP_OAUTH" -d grant_type=client_credentials \
  http://127.0.0.1:8090/oauth/token | jq -r .access_token
```

Rollback is one file: remove the drop-in, `daemon-reload`, `restart`.

### Trade-off: this displaces Access Managed OAuth for connectors

While `MCP_OAUTH` is unset the origin implements no OAuth at all, which is what
lets **Cloudflare Access Managed OAuth** act as the authorization server for
claude.ai / Claude Desktop connectors (they authenticate to Access, and the
origin just sees an authenticated session). Once `MCP_OAUTH` is set the origin
demands a bearer *it* issued, and a connector's bearer is Access's — so the
connector breaks.

**Decision (2026-07-30): the claude.ai lasso connector is turned off** in favour
of per-host credentials. Do not re-enable it without also teaching
`withMCPAuth` to accept a verified Access identity as a fleet-scoped caller
(not implemented).

## Provisioning a host

```bash
lasso mcp-client add --host norm              # scope self: that host only
lasso mcp-client add --host local --fleet     # the lasso host: whole fleet
lasso mcp-client list                         # incl. how many tokens each holds
lasso mcp-client token <client_id> [--ttl 90d] # bearer token; default no expiry
lasso mcp-client rm <client_id>               # also drops its outstanding tokens
```

`--host` takes `local` or an ssh-config alias exactly as `list_hosts` shows it;
a host lasso cannot address is refused up front. The secret prints once and is
stored hashed. Only these clients (and the `MCP_OAUTH` client) may use
`client_credentials` — an open-DCR registration still may not, since that path
takes no human approval.

## Groups: reach between hosts

`self` and `fleet` are the two ends. A **group** is the middle: a named set of
hosts whose members may see and message each other, plus **directed grants**
between groups for the cases where reach should run one way only.

The model, in the order it bites:

- **Mutual inside a group.** Every host in a group's member closure may address
  every other host in that closure. Symmetric, because "these boxes work
  together" is symmetric.
- **Directed between groups.** `grant A B` lets A's hosts reach B's hosts and
  *not* the reverse, and it is **not transitive**: A→B plus B→C gives A nothing
  in C. One hop, always.
- **Members are hosts** — ssh aliases, or the literal `local` — never client
  credentials. Re-key a host (`mcp-client rm` + `add`) and its membership is
  untouched, which is the whole reason membership is not stored on the client.
- **Additive on top of `self`.** No new scope value and no change to
  `oauth_clients`: a self-scoped credential keeps its own host and gains
  whatever its host's groups add. `fleet` and unidentified callers are
  unchanged — they already reach everything.
- Reach is always intersected with what lasso can address, so a member whose ssh
  alias has since been removed goes inert rather than becoming a target no call
  could reach. Same for a dangling reference to a deleted group.

```bash
lasso mcp-group add <name>
lasso mcp-group rm <name>            # cascades: its members, its grants, and refs to it
lasso mcp-group list                 # groups, members (tree), grants
lasso mcp-group add-member <group> <host|@group>...
lasso mcp-group rm-member <group> <host|@group>
lasso mcp-group grant  <from-group> <to-group>   # directed
lasso mcp-group revoke <from-group> <to-group>
lasso mcp-group reach <host>         # effective reach, and why it is reachable
```

A bare name is a host; the `@` sigil marks a subgroup reference (CLI syntax
only — the db stores the kind, so a host and a group may share a name and stay
distinct). Host members are checked against the addressable set when added.

Worked example — the two norm boxes work as one stack:

```bash
lasso mcp-group add norm-stack
lasso mcp-group add-member norm-stack norm norm-darren
```

`norm` and `norm-darren` now list and message each other's agents with their
existing self-scoped credentials. titan (the lasso host, provisioned
`--fleet`) saw both before and still does. **Nobody in norm-stack sees titan**:
reach is mutual only among members, and titan is not one — a group is not a
back door to the fleet-scoped host. To hand one group one-way reach into
another:

```bash
lasso mcp-group add ci
lasso mcp-group grant ci norm-stack   # ci drives norm-stack; norm-stack cannot drive ci
```

**Nesting is the sharp edge.** Nest `@H` inside `G` and every host in H becomes
mutual with *all* of `closure(G)`, not just with G's direct members — nesting
merges reach, it does not layer it. If that is not what you want, don't nest:
make a grant instead. `lasso mcp-group reach <host>` prints the effective set
and the reason for each entry; run it after any structural edit.

Group edits apply on the caller's **next tool call**. Reach is resolved per
request at token-verification time, so there is no token to re-mint, no session
to restart, and no client-side change on the affected hosts — which also means
`rm-member` revokes immediately.

One wrinkle, inherited from credential hosts: `local` is the literal name of
the box lasso runs on, and if that box also has an ssh alias pointing at itself
the alias is a **distinct member**. Adding one does not add the other. Pick the
name the credential uses (`mcp-client list` shows it) and use that one.

## Installing on a host

A workspace box needs **two** credentials, and the split is the useful part: the
Cloudflare Access service token decides *whether* that box may talk to lasso at
all; the lasso credential decides *what it may see* once inside.

```bash
# on titan: provision the host, then mint it a token
lasso mcp-client add --host norm
lasso mcp-client token <client_id>          # no --ttl => never expires

# on the box (put the token in ITS secret store only)
claude mcp add --transport http --scope user lasso \
  https://lasso.orangecountyai.com/mcp \
  --header "Authorization: Bearer $LASSO_MCP_TOKEN" \
  --header "CF-Access-Client-Id: $LASSO_ACCESS_ID" \
  --header "CF-Access-Client-Secret: $LASSO_ACCESS_SECRET"
```

`token` defaults to **no expiry**, because it sits unattended in a host's config
where a rolling expiry is an outage on a timer rather than a security win. Pass
`--ttl 90d` / `2w` / `12h` for a dated one. To revoke: `lasso mcp-client rm
<client_id>` drops the client and every token issued to it, then `add` a
replacement — one client is one host, so the blast radius is exactly the host you
are re-keying. `lasso mcp-client list` shows how many tokens each host holds and
how many never expire.

The client_id/client_secret still work for `client_credentials` where a caller
can run a grant; the minted token is for clients that can only carry a header.

Sanity check from that box — `list_agents` with no `host` should return **its
own** host, and naming another host should be refused with the credential
explanation.

## Known gaps

1. **Group membership is hand-maintained, not derived.** Groups close the
   `self`/`fleet` gap itself — a middle-tier box like `gigachad`, which has its
   own 11 aliases and is either too narrow at `self` or too broad at `fleet`,
   now gets exactly the hosts you put in its group. But you have to enumerate
   them: nothing reads that box's ssh config and turns it into a group, so the
   two drift apart silently as its config changes. Auditing is manual (`lasso
   mcp-group reach <host>`), and deriving membership automatically runs
   straight into gap 2.
2. **Alias names are not identities**, which any attempt to derive group
   membership from a remote ssh config must respect.
   Measured on 2026-07-30: `52labs` resolves to `ws-52labs.orangecountyai.com`
   from titan but to `5.78.190.149` from gigachad — *different machines under
   one name*, so name-based matching would hand gigachad's credential a
   workspace pod. Conversely `minime` is `minime.tail9dd8e.ts.net` from titan
   and bare `minime` from gigachad — *one machine under two names*, which
   HostName matching alone would miss. Any derivation from a remote ssh config
   must resolve both sides with `ssh -G`, match on HostName, and have a human
   confirm the mapping.

## What this is not

Not a sandbox. A fleet-scoped agent is one message away from proxying for a
contained one, and a secret on a box can be exfiltrated from it. Where a trust
zone is a real boundary, enforce it at the network — block that box's path to
lasso — and treat credential scope as defence in depth. As of 2026-07-30 the
workspace pods cannot reach lasso at all (Access returns 401 and the origin is
bound to `127.0.0.1:8090`), which is a stronger guarantee than anything in the
origin.
