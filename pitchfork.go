package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// pitchfork integration. lasso is supervised by pitchfork (https://pitchfork.jdx.dev):
// `lasso start/stop/restart/status` drive a `[daemons.<name>]` entry in the global
// pitchfork config, and `lasso update` restarts that daemon onto the new binary.
// lasso owns its own block in the shared config (delimited by markers) so it can
// rewrite it idempotently — e.g. to add `--tailscale` — without disturbing other
// daemons or hand-written settings.

// miseLassoTool is the mise backend spec lasso is installed under (see install.sh),
// used by `lasso update` to defer to `mise upgrade`.
const miseLassoTool = "ubi:52labs/lasso"

// lassoDaemon is the pitchfork daemon name lasso registers + controls (override
// via LASSO_PITCHFORK_DAEMON for side-by-side installs).
func lassoDaemon() string {
	if d := os.Getenv("LASSO_PITCHFORK_DAEMON"); d != "" {
		return d
	}
	return "lasso"
}

// requirePitchfork returns the pitchfork binary path or aborts with an install
// hint — lasso's lifecycle commands are supervised by pitchfork.
func requirePitchfork() string {
	pf, err := exec.LookPath("pitchfork")
	if err != nil {
		fatal("pitchfork not found on PATH — lasso is supervised by pitchfork.\n" +
			"    install it with:  mise use -g pitchfork    (or see https://pitchfork.jdx.dev)")
	}
	return pf
}

// pitchforkRegistered reports whether the named daemon is known to pitchfork
// (`pitchfork status` exits non-zero for an unregistered daemon).
func pitchforkRegistered(daemon string) bool {
	pf, err := exec.LookPath("pitchfork")
	if err != nil {
		return false
	}
	return exec.Command(pf, "status", daemon).Run() == nil
}

// pitchforkRestart restarts the daemon without touching its config (used by
// `lasso update`, which must not rewrite the run line).
func pitchforkRestart(daemon string) error {
	out, err := exec.Command("pitchfork", "restart", daemon).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------------------------------------------------------------------------
// global config: lasso's [daemons.<name>] block
// ---------------------------------------------------------------------------

// pitchforkGlobalConfig is ~/.config/pitchfork/config.toml, where boot-start
// daemons live (honoring XDG_CONFIG_HOME).
func pitchforkGlobalConfig() string {
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		cfg = filepath.Join(home, ".config")
	}
	return filepath.Join(cfg, "pitchfork", "config.toml")
}

// lassoRunCommand is the command pitchfork execs to serve lasso. Prefer the mise
// shim (version-stable across `mise upgrade`) over the resolved install path, so
// an upgrade is picked up without rewriting the daemon config.
func lassoRunCommand() string {
	if shim := miseShimPath("lasso"); shim != "" {
		return shim
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, e := filepath.EvalSymlinks(exe); e == nil {
			return resolved
		}
		return exe
	}
	return "lasso"
}

func beginMarker(name string) string { return "# >>> lasso-daemon:" + name }
func endMarker(name string) string   { return "# <<< lasso-daemon:" + name }

// daemonBlock renders the marker-delimited [daemons.<name>] section. The markers
// let upsertDaemonBlock rewrite exactly this block on later runs without touching
// anything else in the shared config. pathEnv pins the daemon's PATH so lasso can
// find its child tools (tmux/ttyd/tailscale) even under a scrubbed supervisor env.
func daemonBlock(name, runLine, dir, readyURL, pathEnv string) string {
	var b strings.Builder
	fmt.Fprintln(&b, beginMarker(name))
	fmt.Fprintln(&b, "# lasso daemon — managed by `lasso start`; re-run `lasso start [--tailscale]` to change.")
	fmt.Fprintf(&b, "[daemons.%s]\n", name)
	fmt.Fprintf(&b, "run = %q\n", runLine)
	fmt.Fprintf(&b, "dir = %q\n", dir)
	fmt.Fprintln(&b, "boot_start = true")
	fmt.Fprintln(&b, "retry = true")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "[daemons.%s.env]\n", name)
	fmt.Fprintf(&b, "PATH = %q\n", pathEnv)
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "[daemons.%s.ready]\n", name)
	fmt.Fprintf(&b, "http = %q\n", readyURL)
	fmt.Fprint(&b, endMarker(name))
	return b.String()
}

// daemonPATH returns the PATH to pin into the daemon's env: the current process's
// PATH (which already resolved lasso + pitchfork) with the mise shims dir ensured
// up front, so a mise-managed runtime stays reachable.
func daemonPATH() string {
	p := os.Getenv("PATH")
	shims := filepath.Join(miseDataDir(), "shims")
	sep := string(os.PathListSeparator)
	if shims != "" {
		for _, d := range strings.Split(p, sep) {
			if d == shims {
				return p // already present
			}
		}
		if p == "" {
			return shims
		}
		return shims + sep + p
	}
	return p
}

// listenFromArgs extracts the -listen value from server args, defaulting to the
// loopback default. Used to point the daemon's HTTP readiness check at the right
// port.
func listenFromArgs(args []string) string {
	for i, a := range args {
		switch {
		case a == "-listen" || a == "--listen":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "-listen="):
			return strings.TrimPrefix(a, "-listen=")
		case strings.HasPrefix(a, "--listen="):
			return strings.TrimPrefix(a, "--listen=")
		}
	}
	return defaultListenAddr
}

// ensureLassoDaemon writes (or rewrites) lasso's block in the global pitchfork
// config so `run` reflects the given server args (e.g. --tailscale). Returns the
// readiness URL the daemon will be checked against.
func ensureLassoDaemon(args []string) (readyURL string, err error) {
	name := lassoDaemon()
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	// The daemon runs the foreground server by passing server flags to the binary
	// (no `serve` subcommand). Ensure at least `-listen` so the run line is never a
	// bare `lasso`, which prints help instead of serving.
	runArgs := args
	if len(runArgs) == 0 {
		runArgs = []string{"-listen", defaultListenAddr}
	}
	runLine := lassoRunCommand() + " " + strings.Join(runArgs, " ")
	readyURL = "http://" + listenFromArgs(args) + "/"
	block := daemonBlock(name, runLine, home, readyURL, daemonPATH())

	path := pitchforkGlobalConfig()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	orig, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", readErr
	}
	updated := upsertDaemonBlock(string(orig), name, block)
	if updated == string(orig) {
		return readyURL, nil
	}
	return readyURL, os.WriteFile(path, []byte(updated), 0o644)
}

// daemonBlockPresent reports whether the config already defines [daemons.<name>]
// (marked or hand-written).
func daemonBlockPresent(name string) bool {
	b, err := os.ReadFile(pitchforkGlobalConfig())
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "[daemons."+name+"]")
}

// upsertDaemonBlock replaces lasso's marker-delimited block in orig with block,
// or — if no marked block exists — strips any pre-existing unmarked
// [daemons.<name>] section (to avoid a duplicate-key TOML error) and appends the
// fresh marked block. It is idempotent: feeding it its own output is a no-op.
func upsertDaemonBlock(orig, name, block string) string {
	begin, end := beginMarker(name), endMarker(name)
	if i := strings.Index(orig, begin); i >= 0 {
		if j := strings.Index(orig[i:], end); j >= 0 {
			j = i + j + len(end)
			return orig[:i] + block + orig[j:]
		}
	}
	cleaned := strings.TrimRight(removeUnmarkedDaemonSection(orig, name), "\n")
	if cleaned != "" {
		cleaned += "\n\n"
	}
	return cleaned + block + "\n"
}

// removeUnmarkedDaemonSection removes a hand-written [daemons.<name>] table (and
// its [daemons.<name>.*] subtables, plus any contiguous comment block directly
// above it) from TOML text, leaving every other section intact.
func removeUnmarkedDaemonSection(orig, name string) string {
	lines := strings.Split(orig, "\n")
	header := "[daemons." + name + "]"
	subPrefix := "[daemons." + name + "."
	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		if strings.TrimSpace(lines[i]) == header {
			// Drop the comment/blank lead-in we'd previously emitted for this block.
			for len(out) > 0 {
				last := strings.TrimSpace(out[len(out)-1])
				if last == "" || strings.HasPrefix(last, "#") {
					out = out[:len(out)-1]
				} else {
					break
				}
			}
			i++ // skip the header
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				if strings.HasPrefix(t, "[") {
					if t == header || strings.HasPrefix(t, subPrefix) {
						i++
						continue // a subtable of ours
					}
					break // an unrelated section begins
				}
				i++
			}
			for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++ // swallow trailing blank lines
			}
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// mise integration
// ---------------------------------------------------------------------------

func miseDataDir() string {
	if d := os.Getenv("MISE_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "mise")
}

// miseShimPath returns the path to a mise shim for tool, or "" if there isn't one.
func miseShimPath(tool string) string {
	dir := miseDataDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, "shims", tool)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p
	}
	return ""
}

// miseManaged reports whether the running binary is the mise-installed copy, so
// `lasso update` should defer to `mise upgrade` rather than replace the binary.
func miseManaged() bool {
	if _, err := exec.LookPath("mise"); err != nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, e := filepath.EvalSymlinks(exe); e == nil {
		exe = resolved
	}
	installs := filepath.Join(miseDataDir(), "installs")
	return strings.HasPrefix(exe, installs+string(os.PathSeparator))
}
