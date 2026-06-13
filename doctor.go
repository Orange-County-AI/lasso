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

// `lasso doctor` — a quick health check of the local install: is tmux/ttyd
// present, is the state dir writable, is the binary on PATH, and is a newer
// release available. Prints a line per check and exits non-zero if any hard
// requirement fails.

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

	// Required binaries — lasso drives tmux through ttyd and is supervised by
	// pitchfork.
	for _, bin := range []string{"tmux", "ttyd", "pitchfork"} {
		if path, err := exec.LookPath(bin); err == nil {
			d.line(checkPass, bin+" binary", path)
		} else {
			d.line(checkFail, bin+" binary", "not found on PATH")
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

	// Server status: is something serving on the default port, and is the
	// pitchfork daemon registered?
	if portInUse(defaultListenAddr) {
		d.line(checkPass, "lasso server", "running on "+defaultListenAddr)
	} else {
		d.line(checkPass, "lasso server", "stopped (run `lasso start`)")
	}
	if pitchforkRegistered(lassoDaemon()) {
		d.line(checkPass, "pitchfork daemon", lassoDaemon()+" registered (see `pitchfork status`)")
	} else {
		d.line(checkWarn, "pitchfork daemon", lassoDaemon()+" not registered — run `lasso start`")
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
