package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
)

// devproxy.go — a stateless Host-header demux that gives every local dev-server
// port a PUBLIC, Cloudflare-fronted origin, so the sidebar Browser tab can
// embed it even when lasso itself is opened over a public hostname
// (e.g. lasso.example.com).
//
// Why this exists (and why `tailscale serve` / /api/preview isn't enough):
// tailnet hosts resolve to CGNAT 100.x addresses, which Chrome classifies as a
// *private* network. A document loaded from a *public* origin (lasso behind
// Cloudflare) may not embed a *private* subresource — Chrome's Private Network
// Access blocks the iframe (it loads fine as a top-level tab, just not framed).
// A Cloudflare Tunnel hostname resolves to Cloudflare's public anycast IPs, so
// a public lasso embedding `5173.<dev-domain>` is public->public — no PNA block
// and a trusted cert.
//
// Topology (set up once — see docs/dev-previews-cloudflare.md):
//
//	*.<dev-domain>  ──DNS wildcard──▶  your cloudflared tunnel
//	  cloudflared ingress: "*.<dev-domain>" -> http://127.0.0.1:<listen-port>
//	  this devproxy reads Host "<port>.<dev-domain>"
//	    and reverse-proxies to http://127.0.0.1:<port>
//
// It is stateless: any allowed port is reachable the instant a dev server binds
// it — nothing to create per preview (unlike the tailscale-serve flow).
//
// Security: the wildcard hostname MUST sit behind Cloudflare Access (same policy
// as lasso). Anyone who passes Access can reach any allowed loopback port on
// this host — the same trust model as /api/file and the tailscale-serve preview,
// but edge-authenticated rather than tailnet-only. We only ever proxy to
// loopback and only within the configured port range.

// portRange is an inclusive [min,max] allowlist of dev-server ports the proxy
// will forward to. Ports outside the range are refused.
type portRange struct {
	min, max int
}

func (pr portRange) contains(p int) bool { return p >= pr.min && p <= pr.max }

// parsePortRange parses "min-max" (e.g. "1024-65535").
func parsePortRange(s string) (portRange, error) {
	lo, hi, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return portRange{}, fmt.Errorf("port range %q must be MIN-MAX", s)
	}
	min, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return portRange{}, fmt.Errorf("bad range min %q: %w", lo, err)
	}
	max, err := strconv.Atoi(strings.TrimSpace(hi))
	if err != nil {
		return portRange{}, fmt.Errorf("bad range max %q: %w", hi, err)
	}
	if min < 1 || max > 65535 || min > max {
		return portRange{}, fmt.Errorf("port range %d-%d out of bounds (1-65535, min<=max)", min, max)
	}
	return portRange{min: min, max: max}, nil
}

// portFromHost extracts the dev-server port from a request Host of the form
// "<port>.<domain>" (any :port suffix on the Host header is ignored). It errors
// if the host doesn't match the domain, the label isn't a single numeric
// component, or the port falls outside the allowed range.
func portFromHost(host, domain string, pr portRange) (int, error) {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	suffix := "." + strings.ToLower(strings.TrimSuffix(domain, "."))
	if !strings.HasSuffix(h, suffix) {
		return 0, fmt.Errorf("host %q is not under *%s", host, suffix)
	}
	label := strings.TrimSuffix(h, suffix)
	if label == "" || strings.Contains(label, ".") {
		return 0, fmt.Errorf("expected <port>%s, got %q", suffix, host)
	}
	port, err := strconv.Atoi(label)
	if err != nil {
		return 0, fmt.Errorf("non-numeric port %q in host %q", label, host)
	}
	if !pr.contains(port) {
		return 0, fmt.Errorf("port %d outside allowed range %d-%d", port, pr.min, pr.max)
	}
	return port, nil
}

type devProxyPortKey struct{}

// devProxyHandler returns an http.Handler that maps "<port>.<domain>" to
// http://<target>:<port>. ReverseProxy transparently handles WebSocket upgrades
// (Vite/HMR), so dev servers work end to end.
func devProxyHandler(domain, target string, pr portRange) http.Handler {
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			port := req.Context().Value(devProxyPortKey{}).(int)
			orig := req.Host
			req.URL.Scheme = "http"
			req.URL.Host = net.JoinHostPort(target, strconv.Itoa(port))
			// Send a loopback Host upstream so dev servers that reject unknown
			// hosts (e.g. Vite's allowedHosts check → 403) accept the request.
			// Frameworks that build absolute URLs can still recover the public
			// host from X-Forwarded-Host / -Proto.
			req.Host = req.URL.Host
			req.Header.Set("X-Forwarded-Host", orig)
			req.Header.Set("X-Forwarded-Proto", "https")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "dev server unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port, err := portFromHost(r.Host, domain, pr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		ctx := context.WithValue(r.Context(), devProxyPortKey{}, port)
		rp.ServeHTTP(w, r.WithContext(ctx))
	})
}

// runDevProxy implements `lasso devproxy` — the Host-header demux daemon.
func runDevProxy(args []string) {
	fs := flag.NewFlagSet("devproxy", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9000", "address to listen on (the cloudflared ingress target)")
	domain := fs.String("domain", "", "base domain whose <port> subdomain selects the local port, e.g. dev.example.com (required)")
	target := fs.String("target", "127.0.0.1", "host to forward to (loopback only by design)")
	ports := fs.String("ports", "1024-65535", "inclusive MIN-MAX range of local ports this proxy may forward to")
	_ = fs.Parse(args)

	if strings.TrimSpace(*domain) == "" {
		fmt.Fprintln(os.Stderr, "devproxy: --domain is required (e.g. --domain dev.example.com)")
		os.Exit(2)
	}
	pr, err := parsePortRange(*ports)
	if err != nil {
		fmt.Fprintf(os.Stderr, "devproxy: %v\n", err)
		os.Exit(2)
	}
	dom := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(*domain), "."))

	srv := &http.Server{Addr: *listen, Handler: devProxyHandler(dom, *target, pr)}
	log.Printf("devproxy: %s  ->  *.%s -> http://%s:<%d-%d>", *listen, dom, *target, pr.min, pr.max)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("devproxy: %v", err)
	}
}
