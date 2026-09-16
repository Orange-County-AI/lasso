package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// `lasso doctor` — a quick health check of the local install: is herdr present
// and speaking a compatible protocol, is the state dir writable, is the binary on
// PATH, and is a newer release available. Prints a line per check and exits
// non-zero if any hard requirement fails.

type checkResult int

const (
	checkPass checkResult = iota
	checkWarn
	checkFail
)

// doctorReport accumulates check lines and whether any hard check failed.
type doctorReport struct{ failed bool }

func (d *doctorReport) line(r checkResult, label, detail string) {
	mark := map[checkResult]string{checkPass: "✓", checkWarn: "⚠", checkFail: "✗"}[r]
	if detail != "" {
		fmt.Printf("  %s %-22s %s\n", mark, label, detail)
	} else {
		fmt.Printf("  %s %s\n", mark, label)
	}
	if r == checkFail {
		d.failed = true
	}
}

func cliDoctor() {
	var d doctorReport
	fmt.Printf("lasso %s\n", lassoVersion())

	// herdr binary — lasso is a UI over herdr, so it's required. The version is
	// worth printing beside the path because it is NOT necessarily the one
	// answering the socket: `herdr update` installs the new binary and
	// deliberately leaves a compatible server running (its own status calls that
	// `server_binary_stale`), so the two disagree until a human restarts the
	// session. Reporting only the daemon made a freshly updated host look
	// un-updated.
	herdrBin := ""
	if path, err := exec.LookPath("herdr"); err == nil {
		herdrBin = herdrBinVersion()
		detail := path
		if herdrBin != "" {
			detail = herdrBin + ", " + path
		}
		d.line(checkPass, "herdr binary", detail)
	} else {
		d.line(checkFail, "herdr binary", "not found on PATH — install: curl -fsSL https://herdr.dev/install.sh | sh")
	}

	// herdr socket + protocol compatibility.
	sock := defaultSock()
	if v, p, err := herdrPing(sock); err != nil {
		d.line(checkWarn, "herdr daemon", fmt.Sprintf("socket %s unreachable (%v) — start herdr", sock, err))
	} else if p != lassoHerdrProtocol {
		d.line(checkWarn, "herdr protocol", fmt.Sprintf("herdr %s speaks protocol %d, lasso targets %d — update one to match", v, p, lassoHerdrProtocol))
	} else if herdrBin != "" && herdrBin != v {
		// Protocol-compatible, so nothing is broken and this is not a failure:
		// the running panes are exactly what a restart would end, which is why
		// herdr leaves the choice to a human.
		d.line(checkWarn, "herdr daemon",
			fmt.Sprintf("%s, protocol %d — the installed binary is %s; new terminals already use it, and the running server adopts it on the next session restart (`herdr update --handoff` moves live panes across)", v, p, herdrBin))
	} else {
		d.line(checkPass, "herdr daemon", fmt.Sprintf("%s, protocol %d", v, p))
	}

	// Agent-state integrations — lifecycle hooks that give herdr authoritative
	// idle/working/blocked states for the harnesses lasso can spawn. Missing
	// ones degrade to screen-buffer detection, so warn rather than fail.
	// Harness IDs (claude/codex/opencode/omp/pi) match herdr's integration targets.
	if _, err := exec.LookPath("herdr"); err == nil {
		if out, err := exec.Command("herdr", "integration", "status").Output(); err == nil {
			installed := map[string]bool{}
			for _, ln := range strings.Split(string(out), "\n") {
				if name, rest, ok := strings.Cut(ln, ":"); ok {
					installed[strings.TrimSpace(name)] = !strings.Contains(rest, "not installed")
				}
			}
			var have, missing []string
			for _, h := range harnesses {
				if installed[h.ID] {
					have = append(have, h.ID)
				} else {
					missing = append(missing, h.ID)
				}
			}
			if len(missing) == 0 {
				d.line(checkPass, "agent integrations", strings.Join(have, ", "))
			} else {
				d.line(checkWarn, "agent integrations",
					strings.Join(missing, ", ")+" missing — `herdr integration install <agent>` gives herdr authoritative agent states")
			}
		}
	}

	// State dir writable (pid/log/db live here).
	dir := lassoStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.line(checkFail, "state dir", fmt.Sprintf("%s not writable: %v", dir, err))
	} else if probe, err := os.CreateTemp(dir, ".doctor-*"); err != nil {
		d.line(checkFail, "state dir", fmt.Sprintf("%s not writable: %v", dir, err))
	} else {
		probe.Close()
		os.Remove(probe.Name())
		d.line(checkPass, "state dir", dir)
	}

	// Server status / port.
	if pid, alive := readPid(); alive {
		url := serverURLFromLog()
		if url == "" {
			url = "http://" + defaultListenAddr
		}
		d.line(checkPass, "lasso server", fmt.Sprintf("running (pid %d) → %s", pid, url))
	} else if portInUse(defaultListenAddr) {
		d.line(checkWarn, "lasso server", defaultListenAddr+" is in use by another process")
	} else {
		d.line(checkPass, "lasso server", "stopped (run `lasso start`)")
	}

	// Binary discoverable on PATH.
	if exe, err := os.Executable(); err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
		if dirOnPath(filepath.Dir(exe)) {
			d.line(checkPass, "lasso on PATH", filepath.Dir(exe))
		} else {
			d.line(checkWarn, "lasso on PATH", filepath.Dir(exe)+" is not on PATH — add it so `lasso` is found")
		}
	}

	// Newer release available?
	if latest, err := latestReleaseTag(); err != nil {
		d.line(checkWarn, "latest release", fmt.Sprintf("couldn't check: %v", err))
	} else if semverNewer(lassoSemver, latest) {
		d.line(checkWarn, "latest release", fmt.Sprintf("%s available — run `lasso update`", latest))
	} else {
		d.line(checkPass, "latest release", "up to date ("+latest+")")
	}

	if d.failed {
		fmt.Println("\nsome checks failed — see above")
		os.Exit(1)
	}
}

// herdrBinVersion reports the INSTALLED herdr binary's version, which is not
// necessarily the running daemon's (see cliDoctor). `herdr --version` prints
// "herdr <semver>"; anything else — an older build, a wrapper — yields "" and
// the check degrades to reporting the daemon alone rather than guessing.
func herdrBinVersion() string {
	cmd := exec.Command("herdr", "--version")
	// The updater refuses to run inside a pane, and a CLI invoked from one can
	// pick up that pane's identity; a version read wants neither.
	cmd.Env = outsideHerdrEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[0] != "herdr" {
		return ""
	}
	return fields[1]
}

// portInUse reports whether something is already listening on addr.
func portInUse(addr string) bool {
	ln, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// dirOnPath reports whether dir is one of the entries in $PATH.
func dirOnPath(dir string) bool {
	for _, p := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p == dir {
			return true
		}
	}
	return false
}
