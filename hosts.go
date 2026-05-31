package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HostInfo describes one ssh-config host as a candidate herdr target. A host is
// usable (selectable in the footer switcher) when Reachable && Running &&
// Compatible; otherwise the UI greys it out and shows Err.
type HostInfo struct {
	Alias      string `json:"alias"`
	Reachable  bool   `json:"reachable"`  // ssh connected and ran the probe
	Running    bool   `json:"running"`    // herdr server is up on the host
	Version    string `json:"version"`    // remote herdr version
	Protocol   int    `json:"protocol"`   // remote herdr protocol
	Socket     string `json:"socket"`     // absolute remote herdr socket path
	Compatible bool   `json:"compatible"` // Protocol == local protocol
	Err        string `json:"err,omitempty"`
}

// hostsPayload is the body served at GET /api/hosts.
type hostsPayload struct {
	Active string `json:"active"` // currently driven host ("local" or an alias)
	Local  struct {
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
		Hostname string `json:"hostname"` // machine hostname, shown in place of "local"
	} `json:"local"`
	Hosts []HostInfo `json:"hosts"`
}

// localHostname is the short machine hostname (first label) used as the display
// label for the local host, falling back to "local" if it can't be resolved.
func localHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "local"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i] // strip any domain suffix for a compact label
	}
	return h
}

// ---------------------------------------------------------------------------
// local protocol (cached)
// ---------------------------------------------------------------------------

var localProto struct {
	once     sync.Once
	version  string
	protocol int
}

// localProtocol returns this machine's herdr protocol version (and version
// string), pinging the local socket once and caching the result. A host is
// "compatible" when its protocol equals this.
func localProtocol() (string, int) {
	localProto.once.Do(func() {
		if v, p, err := herdrPing(*herdrSock); err == nil {
			localProto.version, localProto.protocol = v, p
		}
	})
	return localProto.version, localProto.protocol
}

// ---------------------------------------------------------------------------
// ssh config parsing
// ---------------------------------------------------------------------------

// sshConfigHosts returns the concrete host aliases declared in ~/.ssh/config,
// skipping wildcard/negated patterns (*, ?, !) which aren't real targets.
// Include directives are not followed (v1).
func sshConfigHosts() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	defer f.Close()

	var hosts []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Keyword may be separated from values by spaces, tabs, or '='.
		fields := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' || r == '=' })
		if len(fields) < 2 || !strings.EqualFold(fields[0], "Host") {
			continue
		}
		for _, tok := range fields[1:] {
			if strings.ContainsAny(tok, "*?!") {
				continue // wildcard / negation, not a concrete host
			}
			if !seen[tok] {
				seen[tok] = true
				hosts = append(hosts, tok)
			}
		}
	}
	return hosts
}

// ---------------------------------------------------------------------------
// probing
// ---------------------------------------------------------------------------

// probeHost asks a host whether it has a compatible herdr server running by
// running `herdr status server --json` over ssh. BatchMode makes hosts that
// would prompt (password / unknown key) fail fast rather than hang.
func probeHost(ctx context.Context, alias string, wantProto int) HostInfo {
	hi := HostInfo{Alias: alias}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// ClearAllForwardings drops any LocalForward/RemoteForward the user's config
	// attaches to this host (e.g. a tunnel that conflicts with a busy port) — the
	// probe only needs to run one command, no forwarding.
	//
	// Run herdr through a login shell ("$SHELL -lc"): `ssh host <cmd>` uses a
	// non-login, non-interactive shell whose PATH usually omits ~/.local/bin
	// (where herdr installs), so a bare `herdr` reports "command not found" even
	// though an interactive `ssh host` session finds it. A login shell sources
	// the profile that puts herdr on PATH. ${SHELL:-sh} falls back if SHELL is
	// unset.
	cmd := exec.CommandContext(cctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=4",
		"-o", "StrictHostKeyChecking=accept-new",
		alias, `${SHELL:-sh} -lc 'herdr status server --json'`)
	out, err := cmd.Output()
	if err != nil {
		ee, isExit := err.(*exec.ExitError)
		stderr := ""
		if isExit {
			stderr = firstLine(strings.TrimSpace(string(ee.Stderr)))
		}
		// ssh itself failed to connect (exit 255), or the process couldn't run at
		// all → unreachable. Any other exit code means the remote ran the command
		// (e.g. exit 127 "herdr: command not found") → reachable but no herdr.
		if !isExit || ee.ExitCode() == 255 {
			hi.Err = "unreachable"
			if stderr != "" {
				hi.Err = stderr
			}
			return hi
		}
		hi.Reachable = true
		if len(out) == 0 {
			hi.Err = "herdr not installed"
			if stderr != "" {
				hi.Err = stderr
			}
			return hi
		}
		// Non-zero exit but JSON on stdout (e.g. server stopped): fall through.
	}
	hi.Reachable = true
	var st struct {
		Running  bool   `json:"running"`
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
		Socket   string `json:"socket"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		hi.Err = "herdr not running"
		return hi
	}
	hi.Running = st.Running
	hi.Version = st.Version
	hi.Protocol = st.Protocol
	hi.Socket = st.Socket
	if !st.Running {
		hi.Err = "herdr not running"
		return hi
	}
	hi.Compatible = wantProto != 0 && st.Protocol == wantProto
	if !hi.Compatible {
		hi.Err = "protocol " + strconv.Itoa(st.Protocol) + " ≠ " + strconv.Itoa(wantProto)
	}
	return hi
}

// firstLine trims a multi-line ssh error to its first line for a compact tooltip.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// ---------------------------------------------------------------------------
// discovery cache
// ---------------------------------------------------------------------------

var hostCache struct {
	mu    sync.Mutex
	at    time.Time
	hosts []HostInfo
}

const hostCacheTTL = 30 * time.Second

// discoverHosts probes every ssh-config host concurrently and caches the
// result. A fresh cache (within hostCacheTTL) is returned as-is unless force is
// set (the footer's refresh button).
func discoverHosts(ctx context.Context, force bool) []HostInfo {
	hostCache.mu.Lock()
	if !force && !hostCache.at.IsZero() && time.Since(hostCache.at) < hostCacheTTL {
		h := hostCache.hosts
		hostCache.mu.Unlock()
		return h
	}
	hostCache.mu.Unlock()

	_, wantProto := localProtocol()
	aliases := sshConfigHosts()

	results := make([]HostInfo, len(aliases))
	sem := make(chan struct{}, 8) // bound concurrent ssh processes
	var wg sync.WaitGroup
	for i, alias := range aliases {
		wg.Add(1)
		go func(i int, alias string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probeHost(ctx, alias, wantProto)
		}(i, alias)
	}
	wg.Wait()

	// Stable order: usable hosts first, then by alias.
	sort.SliceStable(results, func(i, j int) bool {
		ui := results[i].Reachable && results[i].Running && results[i].Compatible
		uj := results[j].Reachable && results[j].Running && results[j].Compatible
		if ui != uj {
			return ui
		}
		return results[i].Alias < results[j].Alias
	})

	hostCache.mu.Lock()
	hostCache.at = time.Now()
	hostCache.hosts = results
	hostCache.mu.Unlock()
	return results
}

// findHost returns the cached HostInfo for alias, if present.
func findHost(alias string) (HostInfo, bool) {
	hostCache.mu.Lock()
	defer hostCache.mu.Unlock()
	for _, h := range hostCache.hosts {
		if h.Alias == alias {
			return h, true
		}
	}
	return HostInfo{}, false
}

// ---------------------------------------------------------------------------
// GET /api/hosts
// ---------------------------------------------------------------------------

func serveHosts(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"
	hosts := discoverHosts(r.Context(), force)
	ver, proto := localProtocol()

	var p hostsPayload
	p.Active = curBackend().Name()
	p.Local.Version = ver
	p.Local.Protocol = proto
	p.Local.Hostname = localHostname()
	p.Hosts = hosts
	writeJSON(w, p)
}
