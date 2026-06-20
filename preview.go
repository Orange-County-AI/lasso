package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// preview.go — give any local dev-server port a trusted HTTPS origin.
//
// Why: lasso is served only over HTTPS (Cloudflare lasso.knowsuchagency.ai and
// `tailscale serve` on https://citadel.tail9dd8e.ts.net:8443). The sidebar
// Browser tab drops a URL into an iframe; entering http://host:5173 inside an
// HTTPS page is blocked by the browser as mixed content. We hand each local
// port its own trusted HTTPS URL via `tailscale serve`.
//
// We use `tailscale serve` (NOT portless / `--set-path`): each forwarded port
// gets its OWN origin root, so a dev server's absolute asset paths (e.g.
// /@vite/client) resolve correctly. A path-based reverse proxy would rewrite
// the root and break those, and portless needs a local CA / .localhost name a
// remote laptop won't trust.
//
// Security: this is tailnet-only by construction. We NEVER pass --funnel (which
// would expose the port to the public internet) and never bind 0.0.0.0 — the
// HTTPS listener lives on the node's tailscale interface and is reachable only
// by other nodes on the private tailnet. Same trust model as /api/file.

const (
	previewPortMin = 8500
	previewPortMax = 8599
)

func tailscaleBin() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	for _, p := range []string{
		"/usr/bin/tailscale",
		"/usr/local/bin/tailscale",
		"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
	} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return "tailscale"
}

type tailnetIdentity struct {
	dnsName string   // trailing dot stripped, e.g. "citadel.tail9dd8e.ts.net"
	ips     []string // TailscaleIPs, IPv4 first as reported
}

var (
	tailnetSelfMu    sync.Mutex
	tailnetSelfCache *tailnetIdentity
)

// tailnetSelf returns this node's tailnet DNS name + IPs, caching the first
// successful lookup (the node identity doesn't change for the process's life).
func tailnetSelf() (*tailnetIdentity, error) {
	tailnetSelfMu.Lock()
	defer tailnetSelfMu.Unlock()
	if tailnetSelfCache != nil {
		return tailnetSelfCache, nil
	}
	out, err := exec.Command(tailscaleBin(), "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var st struct {
		Self struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	dns := strings.TrimSuffix(st.Self.DNSName, ".")
	if dns == "" {
		return nil, fmt.Errorf("tailscale status: no Self.DNSName")
	}
	id := &tailnetIdentity{dnsName: dns, ips: st.Self.TailscaleIPs}
	tailnetSelfCache = id
	return id, nil
}

// firstTailscaleIPv4 returns the node's first IPv4 tailscale address, if any.
func (id *tailnetIdentity) firstTailscaleIPv4() string {
	for _, ip := range id.ips {
		if net.ParseIP(ip).To4() != nil {
			return ip
		}
	}
	return ""
}

// serveState is the parsed shape of `tailscale serve status --json` that we
// care about: the set of HTTPS ports already in use, and a map of httpsPort ->
// proxy target string (e.g. "http://100.114.163.121:5173").
type serveState struct {
	httpsPorts map[int]bool
	proxies    map[int]string
}

func parseServeStatus(raw []byte) (*serveState, error) {
	// Empty output (no serves configured) parses as the zero value.
	st := &serveState{httpsPorts: map[int]bool{}, proxies: map[int]string{}}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return st, nil
	}
	var parsed struct {
		TCP map[string]struct {
			HTTPS bool `json:"HTTPS"`
		} `json:"TCP"`
		Web map[string]struct {
			Handlers map[string]struct {
				Proxy string `json:"Proxy"`
			} `json:"Handlers"`
		} `json:"Web"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse serve status: %w", err)
	}
	for portStr, tcp := range parsed.TCP {
		if !tcp.HTTPS {
			continue
		}
		if p, err := strconv.Atoi(portStr); err == nil {
			st.httpsPorts[p] = true
		}
	}
	// Web keys look like "host:port"; pull the port and the "/" handler proxy.
	for hostPort, web := range parsed.Web {
		idx := strings.LastIndex(hostPort, ":")
		if idx < 0 {
			continue
		}
		p, err := strconv.Atoi(hostPort[idx+1:])
		if err != nil {
			continue
		}
		if h, ok := web.Handlers["/"]; ok && h.Proxy != "" {
			st.proxies[p] = h.Proxy
		}
	}
	return st, nil
}

func serveStatus() (*serveState, error) {
	out, err := exec.Command(tailscaleBin(), "serve", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale serve status: %w", err)
	}
	return parseServeStatus(out)
}

// localTarget finds an address `tailscale serve` can proxy to for the given
// local port. Dev servers may bind loopback or the tailscale interface, so we
// probe loopback first, then the node's IPv4 tailscale IP.
func localTarget(port int) (string, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		c.Close()
		return "127.0.0.1", nil
	}
	if id, err := tailnetSelf(); err == nil {
		if ip := id.firstTailscaleIPv4(); ip != "" {
			taddr := net.JoinHostPort(ip, strconv.Itoa(port))
			if c, err := net.DialTimeout("tcp", taddr, 300*time.Millisecond); err == nil {
				c.Close()
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("nothing is listening on port %d", port)
}

// allocHTTPSPort returns the first port in the managed range not already used
// as an HTTPS serve port.
func allocHTTPSPort(st *serveState) (int, error) {
	for p := previewPortMin; p <= previewPortMax; p++ {
		if !st.httpsPorts[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free HTTPS port in [%d,%d]", previewPortMin, previewPortMax)
}

// localPortFromProxy parses the local port back out of a proxy target string
// like "http://100.114.163.121:5173" -> 5173.
func localPortFromProxy(proxy string) (int, bool) {
	idx := strings.LastIndex(proxy, ":")
	if idx < 0 {
		return 0, false
	}
	p, err := strconv.Atoi(proxy[idx+1:])
	if err != nil {
		return 0, false
	}
	return p, true
}

type previewEntry struct {
	Port      int    `json:"port"`
	HTTPSPort int    `json:"httpsPort"`
	URL       string `json:"url"`
}

func previewURL(dns string, httpsPort int) string {
	return fmt.Sprintf("https://%s:%d/", dns, httpsPort)
}

func servePreview(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		servePreviewCreate(w, r)
	case http.MethodGet:
		servePreviewList(w, r)
	case http.MethodDelete:
		servePreviewDelete(w, r)
	default:
		http.Error(w, "POST, GET or DELETE only", http.StatusMethodNotAllowed)
	}
}

func servePreviewCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Port int `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		http.Error(w, "port must be 1-65535", http.StatusBadRequest)
		return
	}
	id, err := tailnetSelf()
	if err != nil {
		http.Error(w, "tailscale unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	target, err := localTarget(req.Port)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, err := serveStatus()
	if err != nil {
		http.Error(w, "tailscale serve unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Idempotent: if some HTTPS port already proxies this local port, reuse it
	// rather than creating a duplicate serve.
	for hp, proxy := range st.proxies {
		if lp, ok := localPortFromProxy(proxy); ok && lp == req.Port {
			writeJSON(w, previewEntry{Port: req.Port, HTTPSPort: hp, URL: previewURL(id.dnsName, hp)})
			return
		}
	}
	hp, err := allocHTTPSPort(st)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proxy := fmt.Sprintf("http://%s:%d", target, req.Port)
	// tailnet-only: --bg --https=<hp>. NEVER --funnel.
	cmd := exec.Command(tailscaleBin(), "serve", "--bg", fmt.Sprintf("--https=%d", hp), proxy)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		http.Error(w, "tailscale serve unavailable: "+msg, http.StatusInternalServerError)
		return
	}
	writeJSON(w, previewEntry{Port: req.Port, HTTPSPort: hp, URL: previewURL(id.dnsName, hp)})
}

func servePreviewList(w http.ResponseWriter, r *http.Request) {
	id, err := tailnetSelf()
	if err != nil {
		http.Error(w, "tailscale unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	st, err := serveStatus()
	if err != nil {
		http.Error(w, "tailscale serve unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	entries := []previewEntry{}
	for hp, proxy := range st.proxies {
		if hp < previewPortMin || hp > previewPortMax {
			continue
		}
		lp, ok := localPortFromProxy(proxy)
		if !ok {
			continue
		}
		entries = append(entries, previewEntry{Port: lp, HTTPSPort: hp, URL: previewURL(id.dnsName, hp)})
	}
	writeJSON(w, entries)
}

func servePreviewDelete(w http.ResponseWriter, r *http.Request) {
	hp, err := strconv.Atoi(r.URL.Query().Get("httpsPort"))
	if err != nil {
		http.Error(w, "httpsPort required", http.StatusBadRequest)
		return
	}
	if hp < previewPortMin || hp > previewPortMax {
		http.Error(w, "httpsPort out of managed range", http.StatusBadRequest)
		return
	}
	cmd := exec.Command(tailscaleBin(), "serve", fmt.Sprintf("--https=%d", hp), "off")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		http.Error(w, "tailscale serve unavailable: "+msg, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
