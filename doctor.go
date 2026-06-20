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

	// herdr binary — lasso is a UI over herdr, so it's required.
	if path, err := exec.LookPath("herdr"); err == nil {
		d.line(checkPass, "herdr binary", path)
	} else {
		d.line(checkFail, "herdr binary", "not found on PATH — install: curl -fsSL https://herdr.dev/install.sh | sh")
	}

	// herdr socket + protocol compatibility.
	sock := defaultSock()
	if v, p, err := herdrPing(sock); err != nil {
		d.line(checkWarn, "herdr daemon", fmt.Sprintf("socket %s unreachable (%v) — start herdr", sock, err))
	} else if p != lassoHerdrProtocol {
		d.line(checkWarn, "herdr protocol", fmt.Sprintf("herdr %s speaks protocol %d, lasso targets %d — update one to match", v, p, lassoHerdrProtocol))
	} else {
		d.line(checkPass, "herdr daemon", fmt.Sprintf("%s, protocol %d", v, p))
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
