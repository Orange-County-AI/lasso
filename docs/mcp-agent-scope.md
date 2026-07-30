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
`self` (that host only, the default) or `fleet` (everything lasso can address).

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

1. **Scope is `self` or `fleet`, with nothing in between.** That covers the two
   ends of the titan fleet exactly: the workspace pods can ssh nowhere (`norm`
   has no `~/.ssh` at all), so `self` *is* "the hosts it can ssh into"; and for
   the lasso host, `fleet` *is* titan's ssh config by construction. It does not
   express "the hosts I can ssh into" for a middle-tier box like `gigachad`,
   which has its own 11 aliases — that box is currently either too narrow
   (`self`) or too broad (`fleet`, which would hand it every workspace pod).
   The fix is a per-credential host allowlist, intersected with
   `addressableHosts()` since lasso is the one doing the ssh'ing.
2. **Alias names are not identities**, which any allowlist work must respect.
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
