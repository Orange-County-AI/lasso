package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The lasso CLI. The binary is both the server and its own control surface:
//
//	lasso                 run the server in the foreground (historical behavior)
//	lasso serve [flags]   same, explicitly (flags are the server flags)
//	lasso start|up        start the server in the background (PID file under ~/.lasso)
//	lasso stop|down       stop the background server
//	lasso restart         stop (if running) then start
//	lasso status          report whether the background server is running
//	lasso update          update to the latest release (or git-pull a supervised checkout)
//	lasso doctor          check the local install (herdr, socket, port, version)
//	lasso version         print the version
//
// Subcommands are dispatched in main() BEFORE flag.Parse so the server's flags
// don't have to coexist with subcommand names. A bare invocation, or anything
// whose first arg looks like a flag, falls through to the foreground server —
// keeping `exec ./lasso` (the pitchfork run script) working unchanged.

// defaultListenAddr is the server's default bind address, shared by the -listen
// flag (main.go) and the CLI (status/doctor/URL display) so they never drift.
const defaultListenAddr = "127.0.0.1:8090"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			// Drop the subcommand so the server's flag.Parse sees only its flags.
			os.Args = append(os.Args[:1], os.Args[2:]...)
			runServer()
			return
		case "start", "up":
			cliStart(os.Args[2:])
			return
		case "stop", "down":
			cliStop()
			return
		case "restart":
			cliRestart(os.Args[2:])
			return
		case "status":
			cliStatus()
			return
		case "update":
			cliUpdate()
			return
		case "doctor":
			cliDoctor()
			return
		case "version", "--version", "-v":
			fmt.Println(lassoVersion())
			return
		case "help", "-h", "--help":
			printUsage(os.Stdout)
			return
		}
		// An unknown FIRST token that isn't a flag is a mistyped subcommand — the
		// server takes only flags, never a positional arg. Flags (leading "-")
		// fall through to the foreground server.
		if !strings.HasPrefix(os.Args[1], "-") {
			fmt.Fprintf(os.Stderr, "lasso: unknown command %q\n\n", os.Args[1])
			printUsage(os.Stderr)
			os.Exit(2)
		}
	}
	runServer()
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `lasso — a web UI over herdr for launching and managing agents

usage:
  lasso [flags]            run the server in the foreground
  lasso serve [flags]      run the server in the foreground (explicit)
  lasso start|up [flags]   start the server in the background
  lasso stop|down          stop the background server
  lasso restart [flags]    restart the background server
  lasso status             show whether the background server is running
  lasso update             update lasso to the latest release
  lasso doctor             check the local install
  lasso version            print the version

run "lasso -h" style flags after serve/start/restart; see the README for details.
`)
}

// ---------------------------------------------------------------------------
// state dir + pid/log files
// ---------------------------------------------------------------------------

// lassoStateDir is ~/.lasso (the same dir the state DB lives in), created on
// demand. The CLI keeps its pid + log here.
func lassoStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".lasso")
}

func pidFilePath() string { return filepath.Join(lassoStateDir(), "lasso.pid") }
func logFilePath() string { return filepath.Join(lassoStateDir(), "lasso.log") }

// readPid returns the PID recorded in the pid file and whether that process is
// currently alive. A stale pid file (process gone) reports alive=false.
func readPid() (pid int, alive bool) {
	b, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// Signal 0 probes for existence without delivering a signal.
	return pid, syscall.Kill(pid, 0) == nil
}

// ---------------------------------------------------------------------------
// start / stop / restart / status
// ---------------------------------------------------------------------------

// cliStart launches `lasso serve [args]` detached in its own session, writing
// its PID and redirecting output to ~/.lasso/lasso.log. args are passed straight
// through to the server (e.g. -listen, -theme), so the daemon honors the same
// flags as a foreground run.
func cliStart(args []string) {
	if pid, alive := readPid(); alive {
		fmt.Printf("lasso is already running (pid %d)\n", pid)
		return
	}
	if err := os.MkdirAll(lassoStateDir(), 0o755); err != nil {
		fatal("create state dir: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		fatal("locate lasso binary: %v", err)
	}
	logf, err := os.OpenFile(logFilePath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fatal("open log file: %v", err)
	}
	defer logf.Close()

	cmd := exec.Command(self, append([]string{"serve"}, args...)...)
	// Setsid detaches into a new session + process group so the daemon outlives
	// this CLI process and isn't tied to the controlling terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		fatal("start lasso: %v", err)
	}
	// Capture the PID before Release — Release invalidates cmd.Process (its .Pid
	// then reads back as -1).
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidFilePath(), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		fatal("write pid file: %v", err)
	}
	_ = cmd.Process.Release()

	// Confirm it bound by watching the log for the "UI:" line (or an early exit).
	url := waitForServerURL(pid, 5*time.Second)
	if url == "" {
		fmt.Printf("lasso started (pid %d); waiting on it to bind — check %s\n", pid, logFilePath())
		return
	}
	fmt.Printf("lasso started (pid %d) → %s\n", pid, url)
}

// cliStop sends SIGTERM to the recorded daemon and clears the pid file.
func cliStop() {
	pid, alive := readPid()
	if !alive {
		fmt.Println("lasso is not running")
		_ = os.Remove(pidFilePath()) // clear any stale pid file
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fatal("stop lasso (pid %d): %v", pid, err)
	}
	// Wait briefly for it to exit so `restart` doesn't race the port.
	for i := 0; i < 50; i++ {
		if syscall.Kill(pid, 0) != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(pidFilePath())
	fmt.Printf("lasso stopped (pid %d)\n", pid)
}

func cliRestart(args []string) {
	if _, alive := readPid(); alive {
		cliStop()
	}
	cliStart(args)
}

func cliStatus() {
	pid, alive := readPid()
	if !alive {
		fmt.Println("lasso: stopped")
		return
	}
	url := serverURLFromLog()
	if url == "" {
		url = "http://" + defaultListenAddr
	}
	fmt.Printf("lasso: running (pid %d) → %s\n", pid, url)
}

// uiLineRe extracts the bound URL from the server's "UI: http://host:port" log line.
var uiLineRe = regexp.MustCompile(`UI:\s+(http://\S+)`)

// waitForServerURL tails the log until the server prints its bound URL, the
// process exits, or the deadline passes.
func waitForServerURL(pid int, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if url := serverURLFromLog(); url != "" {
			return url
		}
		if syscall.Kill(pid, 0) != nil {
			return "" // process exited before binding
		}
		time.Sleep(150 * time.Millisecond)
	}
	return ""
}

// serverURLFromLog returns the last "UI:" URL the daemon logged, or "".
func serverURLFromLog() string {
	f, err := os.Open(logFilePath())
	if err != nil {
		return ""
	}
	defer f.Close()
	var url string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := uiLineRe.FindStringSubmatch(sc.Text()); m != nil {
			url = m[1] // keep the latest match
		}
	}
	return url
}

// fatal prints to stderr and exits non-zero — for CLI handlers, which (unlike the
// server) shouldn't dump a stack via log.Fatalf.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lasso: "+format+"\n", args...)
	os.Exit(1)
}
