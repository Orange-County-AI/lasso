package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"os/user"
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
	Hostname   string `json:"hostname"`   // effective ssh HostName (for grouping aliases on one box)
	User       string `json:"user"`       // effective ssh User (distinguishes users on one host)
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
		User     string `json:"user"`     // the user lasso runs as (labels the local row when a host groups >1 user)
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

// localUsername returns the name of the user lasso runs as, used to label the
// local row when a physical host groups more than one user (e.g. the local
// session alongside a loopback ssh alias for another account). Falls back to
// the $USER env var, then "local", if the OS lookup fails.
func localUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "local"
}

// resolveSSHTarget returns the effective HostName and User ssh would use for an
// alias, via `ssh -G` (which expands the full config — Host/Match blocks,
// defaults, includes — without connecting). The frontend groups aliases whose
// HostName is the same physical box (and folds loopback aliases under the local
// host), so two accounts on one machine cluster together. Best-effort: on any
// failure the fields stay empty and the frontend falls back to the alias.
func resolveSSHTarget(alias string) (hostname, username string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-G", alias).Output()
	if err != nil {
		return "", ""
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			hostname = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "user "); ok {
			username = strings.TrimSpace(v)
		}
	}
	return hostname, username
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

// remoteHerdrShell wraps a herdr command line so it runs in the remote user's
// login shell with the usual user-local install dirs (~/.local/bin and mise's
// shim dir) forced onto PATH first. `ssh host <cmd>` uses a non-login shell whose
// PATH omits those dirs, and even `$SHELL -lc` only finds herdr if the login
// profile happens to add them — a freshly provisioned host (herdr just dropped in
// ~/.local/bin, profile not yet wired) would otherwise still report "command not
// found" and keep showing "set up". Prefixing PATH ourselves — matching what the
// provision script does — makes detection and update robust regardless of how the
// host's profile is set up. $HOME/$PATH are left for the remote login shell to
// expand (the single-quoted body is opaque to the outer shell).
func remoteHerdrShell(herdrCmd string) string {
	return `${SHELL:-sh} -lc 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"; ` + herdrCmd + `'`
}

// probeHost asks a host whether it has a compatible herdr server running by
// running `herdr status server --json` over ssh. BatchMode makes hosts that
// would prompt (password / unknown key) fail fast rather than hang.
func probeHost(ctx context.Context, alias string, wantProto int) HostInfo {
	hi := HostInfo{Alias: alias}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// ClearAllForwardings drops any LocalForward/RemoteForward the user's config
	// attaches to this host (e.g. a tunnel that conflicts with a busy port) — the
	// probe only needs to run one command, no forwarding. remoteHerdrShell runs
	// herdr in a login shell with the user-local install dirs forced onto PATH.
	cmd := exec.CommandContext(cctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=4",
		"-o", "StrictHostKeyChecking=accept-new",
		alias, remoteHerdrShell("herdr status server --json"))
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
			hi := probeHost(ctx, alias, wantProto)
			// Resolve where the alias points (HostName/User) so the UI can group
			// aliases that share a physical box. Cheap and connectionless (`ssh -G`).
			hi.Hostname, hi.User = resolveSSHTarget(alias)
			results[i] = hi
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
	p.Local.User = localUsername()
	p.Hosts = hosts
	writeJSON(w, p)
}

// invalidateHostCache forces the next discoverHosts to re-probe (used after an
// action that changes a host's herdr — e.g. a remote update).
func invalidateHostCache() {
	hostCache.mu.Lock()
	hostCache.at = time.Time{}
	hostCache.mu.Unlock()
}

// ---------------------------------------------------------------------------
// POST /api/host-update — run `herdr update` on a remote host
// ---------------------------------------------------------------------------

// hostUpdateTimeout bounds the whole remote update (manifest fetch + binary
// download + install), generous because it pulls a release binary over the far
// host's network.
const hostUpdateTimeout = 3 * time.Minute

// serveHostUpdate runs `herdr update` on a remote ssh-config host to bring a
// host that's behind this lasso's herdr protocol back into compatibility.
//
// herdr's updater is interactive: when a protocol change forces running sessions
// to restart it asks "stop after installing? [y/N]" (stopping exits the old
// server's pane processes), and after a successful update it may ask to star the
// repo. Both prompts bail with an error unless stdin is a TTY, so we force a
// remote PTY with `ssh -tt` and feed the answers — "y" to stop the old server
// (the caller has accepted killing those processes), then "n" to decline the
// star prompt. Any fed line a prompt doesn't consume is discarded once herdr
// exits, so over-feeding is harmless.
func serveHostUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" || req.Host == "local" {
		http.Error(w, "remote host required", http.StatusBadRequest)
		return
	}
	// Only update a host we've already probed as reachable with herdr running, so
	// a stray alias can't make us shell out to an arbitrary box. The alias rides
	// ssh's argv (not a shell), so it can't inject a command.
	hi, ok := findHost(req.Host)
	if !ok || !hi.Reachable || !hi.Running {
		http.Error(w, "host not reachable / no herdr running", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hostUpdateTimeout)
	defer cancel()

	// -tt forces a remote PTY even though our stdin is a pipe, so herdr's updater
	// sees a terminal and runs its prompts (rather than erroring out). remoteHerdrShell
	// runs in a login shell with `herdr` forced onto PATH, matching probeHost. We
	// only run one command, so clear any forwardings the host's config attaches.
	cmd := exec.CommandContext(ctx, "ssh",
		"-tt",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		req.Host, remoteHerdrShell("herdr update"))
	cmd.Stdin = strings.NewReader("y\nn\n")
	out, err := cmd.CombinedOutput()

	resp := map[string]any{"ok": err == nil, "output": strings.TrimSpace(string(out))}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			resp["error"] = "timed out"
		} else {
			resp["error"] = err.Error()
		}
	} else {
		// The host's herdr just changed; drop the cache so the next /api/hosts
		// re-probes and reflects the new version/protocol/compatibility.
		invalidateHostCache()
	}
	writeJSON(w, resp)
}

// ---------------------------------------------------------------------------
// POST /api/host-provision — install herdr + supervise it with systemd --user
// ---------------------------------------------------------------------------

// hostProvisionTimeout bounds the whole bootstrap: it may download the herdr
// release binary over the far host's network.
const hostProvisionTimeout = 5 * time.Minute

// provisionScript bootstraps herdr-under-systemd on a remote Linux host, end to
// end and idempotently: ensure herdr (herdr.dev/install.sh), write a systemd
// --user unit for the server, enable lingering so it survives logout/reboot,
// start it, and install the agent-state integrations for every harness lasso
// can spawn (claude/codex/opencode) so herdr gets authoritative
// idle/working/blocked hooks instead of screen-scraping. It's shell-agnostic —
// rather than trust the login shell's PATH wiring, it puts the user-local bin
// dirs on PATH itself. Every step logs a line so the captured output reads as a
// provisioning log.
const provisionScript = `set -u
log() { printf '==> %s\n' "$*"; }

export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"
hash -r 2>/dev/null || true

# 0. systemd -----------------------------------------------------------------
# Supervision is systemd --user; a non-interactive ssh session may lack the
# runtime dir env the user manager is addressed by.
command -v systemctl >/dev/null 2>&1 || { echo "ERROR: systemctl not found — provisioning requires a Linux host with systemd" >&2; exit 3; }
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
systemctl --user is-enabled default.target >/dev/null 2>&1 || true
loginctl enable-linger "$(id -un)" 2>/dev/null || log "note: 'loginctl enable-linger' failed; the herdr server may stop at logout"

# 1. herdr -------------------------------------------------------------------
if ! command -v herdr >/dev/null 2>&1; then
  log "installing herdr (herdr.dev/install.sh)"
  curl -fsSL https://herdr.dev/install.sh | sh
  hash -r 2>/dev/null || true
fi
herdr_bin="$(command -v herdr 2>/dev/null || echo "$HOME/.local/bin/herdr")"
[ -x "$herdr_bin" ] || command -v herdr >/dev/null 2>&1 || { echo "ERROR: herdr not installed" >&2; exit 4; }
log "herdr $("$herdr_bin" --version 2>/dev/null)"

# 2. systemd --user unit -----------------------------------------------------
# Written unconditionally (marked managed) so re-provisioning refreshes it.
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
mkdir -p "$unit_dir"
log "writing $unit_dir/herdr.service"
cat > "$unit_dir/herdr.service" <<EOF
[Unit]
Description=herdr — headless terminal workspace server for AI coding agents
# managed by lasso host provisioning; edits may be overwritten on re-provision
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$HOME
Environment=PATH=$HOME/.local/bin:$HOME/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin
ExecStart=$herdr_bin server
# Graceful shutdown via herdr's own API so panes are torn down cleanly.
ExecStop=$herdr_bin server stop
# Only signal the main server process, not every pane in the cgroup.
KillMode=mixed
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
log "starting herdr under systemd --user"
systemctl --user enable --now herdr.service || { echo "ERROR: 'systemctl --user enable --now herdr' failed" >&2; exit 5; }

# 3. agent-state integrations ------------------------------------------------
# Lifecycle hooks give herdr authoritative idle/working/blocked states for the
# agents lasso spawns; without them it falls back to screen-buffer detection.
# Best-effort: an integration for a CLI that isn't installed yet still stages
# its hook files and starts working once that CLI arrives.
for agent in claude codex opencode; do
  if "$herdr_bin" integration install "$agent" >/dev/null 2>&1; then
    log "integration $agent installed"
  else
    log "note: 'herdr integration install $agent' failed; agent state falls back to screen detection"
  fi
done

# 4. verify ------------------------------------------------------------------
sleep 1
if "$herdr_bin" status server --json 2>/dev/null | grep -Eq '"running"[[:space:]]*:[[:space:]]*true'; then
  log "herdr server running"
else
  echo "ERROR: herdr server not running after setup" >&2
  "$herdr_bin" status server --json 2>/dev/null || true
  exit 6
fi
log "done"
`

// serveHostProvision installs herdr on a remote host (if missing) and brings it
// up supervised by systemd --user, so a host that has no herdr — or has it but
// with no server running — can be made selectable. Unlike the update path this
// is fully non-interactive (the install scripts and systemctl commands don't
// prompt), so no PTY is needed: we pipe provisionScript to `bash -s`.
func serveHostProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" || req.Host == "local" {
		http.Error(w, "remote host required", http.StatusBadRequest)
		return
	}
	// Only provision a host we've probed as reachable (herdr may be missing or its
	// server down — that's the point). The alias rides ssh's argv, not a shell.
	hi, ok := findHost(req.Host)
	if !ok || !hi.Reachable {
		http.Error(w, "host not reachable", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), hostProvisionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		req.Host, "bash -s")
	cmd.Stdin = strings.NewReader(provisionScript)
	out, err := cmd.CombinedOutput()

	resp := map[string]any{"ok": err == nil, "output": strings.TrimSpace(string(out))}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			resp["error"] = "timed out"
		} else {
			resp["error"] = err.Error()
		}
	} else {
		invalidateHostCache()
	}
	writeJSON(w, resp)
}
