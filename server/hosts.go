package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// HostInfo describes one ssh-config host as a candidate lasso target. A host is
// usable (selectable in the switcher, and a valid agent/grid target) when
// Reachable && HasTmux. Otherwise the UI greys it out and shows Err.
//
// Unlike the herdr era there's no daemon to match protocols with: lasso drives
// the remote tmux server directly over SSH, so the only remote requirement is
// that ssh connects and `tmux` is installed.
type HostInfo struct {
	Alias       string `json:"alias"`
	Reachable   bool   `json:"reachable"` // ssh connected and ran the probe
	HasTmux     bool   `json:"has_tmux"`  // `tmux` is on the remote PATH
	TmuxVersion string `json:"tmux_version,omitempty"`
	Home        string `json:"home,omitempty"` // remote $HOME (probed)
	Err         string `json:"err,omitempty"`
}

// usable reports whether lasso can drive this host.
func (h HostInfo) usable() bool { return h.Reachable && h.HasTmux }

// hostsPayload is the body served at GET /api/hosts.
type hostsPayload struct {
	Active   string     `json:"active"`   // currently driven host ("local" or an alias)
	Hostname string     `json:"hostname"` // local machine hostname, shown for the local host
	Hosts    []HostInfo `json:"hosts"`    // discovered ssh-config hosts
}

// localHostname is the short machine hostname (first label) used as the display
// label for the local host, falling back to "local".
func localHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "local"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// ---------------------------------------------------------------------------
// ssh config parsing
// ---------------------------------------------------------------------------

// sshConfigHosts returns the concrete host aliases declared in ~/.ssh/config,
// skipping wildcard/negated patterns (*, ?, !) which aren't real targets.
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

// probeHost asks a host whether it's reachable and has tmux, in ONE ssh round
// trip that also captures the remote $HOME (saving a connect-time round trip).
// BatchMode makes hosts that would prompt (password / unknown key) fail fast.
func probeHost(ctx context.Context, alias string) HostInfo {
	hi := HostInfo{Alias: alias}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// One command emits three lines we parse: HOME, TMUX (version, if installed).
	// `command -v tmux` keeps it quiet when tmux is absent. ClearAllForwardings
	// drops any tunnel the user's config attaches to this host — the probe only
	// runs a command. The PATH export matches remotePathExport (remote.go): a
	// non-interactive ssh shell misses Homebrew/~/.local/bin, where tmux often
	// lives (macOS hosts), and the probe must see the same PATH later commands do.
	const probe = remotePathExport + `printf 'HOME=%s\n' "$HOME"; if command -v tmux >/dev/null 2>&1; then printf 'TMUX=%s\n' "$(tmux -V 2>/dev/null)"; fi`
	cmd := exec.CommandContext(cctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=4",
		"-o", "StrictHostKeyChecking=accept-new",
		alias, probe)
	out, err := cmd.Output()
	if err != nil {
		ee, isExit := err.(*exec.ExitError)
		stderr := ""
		if isExit {
			stderr = firstLine(strings.TrimSpace(string(ee.Stderr)))
		}
		// ssh failed to connect (exit 255) or couldn't run at all → unreachable.
		// Any other exit means the remote ran the command (reachable).
		if !isExit || ee.ExitCode() == 255 {
			hi.Err = "unreachable"
			if stderr != "" {
				hi.Err = stderr
			}
			return hi
		}
	}
	hi.Reachable = true
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "HOME="):
			hi.Home = strings.TrimPrefix(line, "HOME=")
		case strings.HasPrefix(line, "TMUX="):
			hi.HasTmux = true
			hi.TmuxVersion = strings.TrimSpace(strings.TrimPrefix(line, "TMUX="))
		}
	}
	if !hi.HasTmux {
		hi.Err = "tmux not installed"
	}
	return hi
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

// discoverHosts probes every ssh-config host concurrently and caches the result.
// A fresh cache (within hostCacheTTL) is returned as-is unless force is set.
func discoverHosts(ctx context.Context, force bool) []HostInfo {
	hostCache.mu.Lock()
	if !force && !hostCache.at.IsZero() && time.Since(hostCache.at) < hostCacheTTL {
		h := hostCache.hosts
		hostCache.mu.Unlock()
		return h
	}
	hostCache.mu.Unlock()

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
			results[i] = probeHost(ctx, alias)
		}(i, alias)
	}
	wg.Wait()

	// Stable order: usable hosts first, then by alias.
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].usable() != results[j].usable() {
			return results[i].usable()
		}
		return results[i].Alias < results[j].Alias
	})

	hostCache.mu.Lock()
	hostCache.at = time.Now()
	hostCache.hosts = results
	hostCache.mu.Unlock()
	return results
}

// discoveredHosts returns the cached host list (probing once if cold). Used by
// the grid aggregator and list_hosts without forcing a re-probe.
func discoveredHosts() []HostInfo {
	return discoverHosts(context.Background(), false)
}

// hostTarget is one host to aggregate over: its routing name ("" = local) and a
// display label.
type hostTarget struct {
	host  string
	label string
}

// usableHostTargets is the local host plus every reachable, tmux-capable ssh
// host — the set the cross-host grid and the all-hosts sidebar aggregate over.
// Local is always first.
func usableHostTargets() []hostTarget {
	targets := []hostTarget{{"", localHostname()}}
	for _, h := range discoveredHosts() {
		if h.usable() {
			targets = append(targets, hostTarget{h.Alias, h.Alias})
		}
	}
	return targets
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

// invalidateHostCache forces the next discoverHosts to re-probe.
func invalidateHostCache() {
	hostCache.mu.Lock()
	hostCache.at = time.Time{}
	hostCache.mu.Unlock()
}

// ---------------------------------------------------------------------------
// GET /api/hosts
// ---------------------------------------------------------------------------

func serveHosts(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1"
	writeJSON(w, hostsPayload{
		Active:   curBackend().Name(),
		Hostname: localHostname(),
		Hosts:    discoverHosts(r.Context(), force),
	})
}
