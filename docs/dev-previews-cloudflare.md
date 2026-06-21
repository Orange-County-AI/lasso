# Dev previews behind Cloudflare (`lasso devproxy`)

If you run lasso behind Cloudflare (a public hostname like
`lasso.example.com` via Cloudflare Tunnel + Access), the sidebar **Browser
tab** can't embed a local dev server through the usual `tailscale serve`
preview — the browser blocks it. This guide sets up a Cloudflare-native path so
it can.

This is **opt-in**: with `-preview-public-domain` unset (the default), lasso
behaves exactly as before (loopback / `tailscale serve` previews). Nothing here
is specific to any one deployment — the domain, ports, and listen address are
all flags.

## Why the usual preview goes blank

A tailnet host (`*.ts.net`) resolves to a CGNAT `100.x` address, which Chrome
classifies as a **private** network. A document loaded from a **public** origin
(lasso behind Cloudflare) may not embed a **private** subresource — Chrome's
**Private Network Access** blocks the iframe. The page still opens in a
standalone tab (top-level navigations are exempt), just not framed. So
`tailscale serve` / `/api/preview` URLs (private) go blank in the sidebar when
lasso is reached over its public hostname.

A **Cloudflare Tunnel** hostname resolves to Cloudflare's public anycast IPs, so
a public lasso embedding a public `<port>.<dev-domain>` is **public → public** —
no PNA block, a trusted cert, frameable.

## Architecture

```
browser
  └─ https://lasso.<your-domain>                  (public, Cloudflare Access)
        └─ iframes ─▶ https://<port>.<dev-domain>/        (public, Cloudflare Access)
                         │  DNS:    *.<dev-domain> CNAME → <your tunnel> (proxied)
                         │  Access: app "*.<dev-domain>" (same policy as lasso)
                         ▼
                      your cloudflared tunnel
                         ingress: "*.<dev-domain>" → http://127.0.0.1:<demux-port>
                         ▼
                      lasso devproxy  (the demux, a pitchfork/systemd daemon)
                         Host "<port>.<dev-domain>" → http://127.0.0.1:<port>
                         (rewrites Host to loopback so dev servers that check
                          Host — e.g. Vite's allowedHosts → 403 — accept it)
```

`lasso devproxy` is **stateless**: any allowed port is reachable the instant a
dev server binds it — nothing to provision per preview.

## Setup

Pick a `<dev-domain>` whose **first-level** wildcard you can put behind the
tunnel. A first-level wildcard (`*.<dev-domain>`) is covered by free Universal
SSL; a 2-level one (`*.dev.<your-domain>`) needs Advanced Certificate Manager.
A **dedicated** zone is recommended so the catch-all wildcard doesn't shadow
other subdomains, and so a wildcard Access app can't accidentally gate your
existing public APIs.

1. **devproxy daemon** — run it on a free loopback port (`<demux-port>`):

   ```
   lasso devproxy --listen 127.0.0.1:<demux-port> --domain <dev-domain> --ports 1024-65535
   ```

   Supervise it (pitchfork/systemd). No HTTP ready-check: the demux 404s any
   host that isn't `<port>.<dev-domain>`, so a 2xx probe never passes —
   process-up means ready.

2. **DNS** — `*.<dev-domain>` CNAME → `<tunnel-id>.cfargotunnel.com`, **proxied**.
   (`cloudflared tunnel route dns` doesn't create wildcards — add it via the
   dashboard or API.)

3. **Tunnel ingress** — in your `config.yml`, **after** all explicit hostnames
   and **before** the `404` catch-all (first match wins). Use `127.0.0.1`, not
   `localhost`, so cloudflared doesn't try IPv6 `::1` (the demux binds IPv4):

   ```yaml
     - hostname: "*.<dev-domain>"
       service: http://127.0.0.1:<demux-port>
     - service: http_status:404
   ```

   Then reload cloudflared (`systemctl restart cloudflared`). Validate first
   with `cloudflared --config <path> tunnel ingress validate`.

4. **Access** — a self-hosted app on `*.<dev-domain>` with the same policy as
   your lasso app. **Required:** the wildcard is internet-reachable, so without
   Access the demux exposes every allowed loopback port publicly (same trust
   model as `/api/file`, but edge-authenticated instead of tailnet-only).

5. **lasso** — run the server with `-preview-public-domain=<dev-domain>` (or
   `PREVIEW_PUBLIC_DOMAIN=<dev-domain>`). Now a port typed in the Browser tab
   (`5173`, `:5173`, or `localhost:5173`) auto-resolves to
   `https://5173.<dev-domain>/` when lasso is reached over a public origin;
   tailnet/loopback origins keep the `tailscale serve` flow.

## Using it

- **First load** of a given dev hostname redirects to Cloudflare Access. Because
  the Access *login* page itself can't be framed, the iframe may be blank until
  you've authenticated that hostname once — click **open in new tab**, sign in,
  then reload the iframe (the `CF_Authorization` cookie then rides the framed
  request).
- Before lasso runs with the flag, you can still use it by typing the **full**
  `https://<port>.<dev-domain>/` in the Browser tab.

## Security

The wildcard hostname is internet-reachable, so keep it behind the Access app.
Anyone who passes Access can reach any allowed loopback port on the host — scope
`--ports` accordingly. The demux only ever forwards to loopback.

## Teardown

- DNS: delete/restore the `*.<dev-domain>` record.
- cloudflared: remove the ingress rule (keep a `config.yml` backup) and reload.
- Access: delete the `*.<dev-domain>` app.
- daemon: stop and remove `lasso devproxy`.
