package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// `lasso doctor` — a quick health check of the local install: is luvus present
// and speaking a UHP contract lasso can use, is the state dir writable, is the
// binary on PATH, and is a newer release available. Prints a line per check and
// exits non-zero if any hard requirement fails.

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

	// luvus binary — lasso is a UI over Luvus, so it's required.
	if path, err := exec.LookPath(luvusCLI()); err == nil {
		d.line(checkPass, "luvus binary", path)
	} else {
		d.line(checkFail, "luvus binary", "not found on PATH — install: curl -fsSL https://luvus.dev/install.sh | sh")
	}

	// The session endpoint Luvus itself reports, then the UHP handshake against
	// it. Asking Luvus where its socket is (rather than assembling a path) is
	// what makes this check agree with the server lasso actually drives — a
	// custom LUVUS_HOME, or a named session, moves it.
	sock := defaultSock()
	if disc := discoverLocalLuvusSocket(); disc != "" && disc != sock {
		d.line(checkWarn, "luvus endpoint", fmt.Sprintf("lasso targets %s but luvus reports %s — unset LUVUS_SOCKET_PATH or pass -luvus-sock", sock, disc))
	}
	if v, p, err := luvusPing(sock); err != nil {
		d.line(checkWarn, "luvus server", fmt.Sprintf("%s: %v — start it with `luvus server start`", sock, err))
	} else {
		d.line(checkPass, "luvus server", fmt.Sprintf("%s, %s", v, uhpLabel(luvusUHPName, p)))
	}

	// Agent-state integrations — the session-resume hooks that give Luvus
	// authoritative agent identity and state for the harnesses lasso can spawn.
	// Missing ones degrade to process/screen detection, so warn rather than fail.
	// Harness IDs match Luvus's `integration install` targets.
	//
	// integration.status is a HOST-PROFILE method: it is answered inside a
	// short-lived `luvus uhp proxy` and is deliberately not served by the session
	// socket, so this reports honestly even with no server running.
	ctx, cancel := context.WithTimeout(context.Background(), luvusProxyTimeout)
	defer cancel()
	if res, err := luvusProxyCall(ctx, "integration.status", nil); err == nil {
		var st struct {
			Integrations []struct {
				Agent     string `json:"agent"`
				Installed bool   `json:"installed"`
			} `json:"integrations"`
		}
		if json.Unmarshal(res, &st) == nil {
			installed := make(map[string]bool, len(st.Integrations))
			known := make(map[string]bool, len(st.Integrations))
			for _, in := range st.Integrations {
				installed[in.Agent] = in.Installed
				known[in.Agent] = true
			}
			var have, missing, unsupported []string
			for _, h := range harnesses {
				switch {
				case installed[h.ID]:
					have = append(have, h.ID)
				case known[h.ID]:
					missing = append(missing, h.ID)
				default:
					// A harness Luvus has no integration for: its agents are
					// detected, never reported. Saying "missing" would suggest a
					// fix that does not exist.
					unsupported = append(unsupported, h.ID)
				}
			}
			detail := strings.Join(have, ", ")
			if len(missing) > 0 {
				detail = strings.Join(missing, ", ") + " missing — `luvus integration install <agent>` gives Luvus authoritative agent states"
			}
			if len(unsupported) > 0 {
				if detail != "" {
					detail += "; "
				}
				detail += "no luvus integration for " + strings.Join(unsupported, ", ") + " (detection only)"
			}
			if len(missing) == 0 {
				d.line(checkPass, "agent integrations", detail)
			} else {
				d.line(checkWarn, "agent integrations", detail)
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
