package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The lasso CLI. The binary is both the server and its own control surface:
//
//	lasso                 print help (a bare invocation does NOT start the server)
//	lasso start|up        start the server, supervised by pitchfork
//	lasso stop|down       stop the supervised server
//	lasso restart         restart the supervised server
//	lasso status          report the pitchfork daemon's status
//	lasso update          update to the latest release (mise upgrade, or self-replace)
//	lasso doctor          check the local install (tmux, ttyd, port, version)
//	lasso closeme         close the calling agent itself (uses $LASSO_TAB_ID)
//	lasso version         print the version
//
// Subcommands are dispatched in main() BEFORE flag.Parse so the server's flags
// don't have to coexist with subcommand names. The foreground server is run by
// passing server flags (e.g. `lasso -listen … --tailscale`) — which is how the
// pitchfork daemon execs it; bare `lasso` prints help instead. (`serve` is kept
// as an undocumented back-compat alias for pre-existing daemon run lines.)

// defaultListenAddr is the server's default bind address, shared by the -listen
// flag (main.go) and the CLI (status/doctor/URL display) so they never drift.
const defaultListenAddr = "127.0.0.1:8090"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			// Undocumented back-compat alias: pre-existing pitchfork daemon run
			// lines may still say `lasso serve …`. Strip it and run the server.
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
		case "closeme":
			cliCloseMe()
			return
		case "version", "--version", "-v":
			fmt.Println(lassoVersion())
			return
		case "help", "-h", "--help":
			printUsage(os.Stdout)
			return
		}
		// A leading-flag invocation (e.g. `lasso -listen … --tailscale`) runs the
		// foreground server — that's how the pitchfork daemon execs it. Any other
		// first token is a mistyped subcommand.
		if strings.HasPrefix(os.Args[1], "-") {
			runServer()
			return
		}
		fmt.Fprintf(os.Stderr, "lasso: unknown command %q\n\n", os.Args[1])
		printUsage(os.Stderr)
		os.Exit(2)
	}
	// Bare `lasso` prints help — it does NOT start the server (use `lasso start`,
	// or pass server flags for a foreground run).
	printUsage(os.Stdout)
}

func printUsage(w *os.File) {
	fmt.Fprint(w, `lasso — a web UI for launching and managing agents

usage:
  lasso                    print this help
  lasso start|up [flags]   start the server, supervised by pitchfork
  lasso stop|down          stop the supervised server
  lasso restart [flags]    restart the supervised server
  lasso status             show the pitchfork daemon's status
  lasso update             update lasso to the latest release
  lasso doctor             check the local install
  lasso closeme            close the calling agent itself (uses $LASSO_TAB_ID)
  lasso version            print the version

start/restart take server flags (--tailscale, -listen, …) and persist them into
the daemon's run line. lasso is supervised by pitchfork — see the README.
`)
}

// ---------------------------------------------------------------------------
// closeme — an agent closes itself
// ---------------------------------------------------------------------------

// cliCloseMe lets a lasso-spawned agent shut itself down with a single command,
// no MCP round-trip and no need to know its own id beyond the $LASSO_TAB_ID the
// session already exports. It POSTs that tab id to the running server's
// /api/agent/close (the same soft-close the UI and the close_agent MCP tool use),
// so the server kills the agent process, closes the pane, and notifies the UI.
//
// The server is assumed to be the local default loopback bind; override the
// address with LASSO_LISTEN (host:port) for a non-default port. UI_AUTH is
// honored if set in the environment.
func cliCloseMe() {
	tabID := os.Getenv("LASSO_TAB_ID")
	if tabID == "" {
		fatal("closeme: $LASSO_TAB_ID is unset — you are not running inside a lasso-managed terminal")
	}
	addr := defaultListenAddr
	if env := os.Getenv("LASSO_LISTEN"); env != "" {
		addr = env
	}
	user, pass, hasAuth := parseAuth(os.Getenv("UI_AUTH"))
	if err := postAgentClose(addr, tabID, user, pass, hasAuth); err != nil {
		fatal("closeme: %v", err)
	}
	fmt.Printf("closeme: closing agent %s\n", tabID)
}

// postAgentClose POSTs a tab id to the server's /api/agent/close (the soft-close
// shared with the UI and the close_agent MCP tool). Closing our own tab kills
// the tmux session this process runs in, so the connection may drop before the
// response arrives — that's success, not an error. Only a failure to *reach* the
// server, or a non-200 status, is reported.
func postAgentClose(addr, tabID, user, pass string, hasAuth bool) error {
	body, _ := json.Marshal(map[string]string{"tab_id": tabID})
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/agent/close", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if hasAuth {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach lasso at %s: %w (is the server running? set LASSO_LISTEN for a non-default port)", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return nil
}

// ---------------------------------------------------------------------------
// state dir
// ---------------------------------------------------------------------------

// lassoStateDir is ~/.lasso (the same dir the state DB lives in), created on
// demand.
func lassoStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".lasso")
}

// ---------------------------------------------------------------------------
// start / stop / restart / status — all via pitchfork
// ---------------------------------------------------------------------------

// ensureDaemonConfigured writes lasso's [daemons.<name>] block when there are
// server flags to persist (e.g. --tailscale) or when no block exists yet. A bare
// `lasso start`/`restart` on an already-configured daemon leaves the existing run
// line intact, so it never silently strips a previously-set --tailscale.
func ensureDaemonConfigured(args []string) {
	if len(args) == 0 && daemonBlockPresent(lassoDaemon()) {
		return
	}
	if _, err := ensureLassoDaemon(args); err != nil {
		fatal("write pitchfork config %s: %v", pitchforkGlobalConfig(), err)
	}
}

// cliStart registers (or updates) the lasso pitchfork daemon and starts it. args
// are the server flags, persisted into the daemon's run line. `-f` makes
// pitchfork restart it if already running, so a flag change takes effect.
func cliStart(args []string) {
	requirePitchfork()
	daemon := lassoDaemon()
	ensureDaemonConfigured(args)
	if out, err := exec.Command("pitchfork", "start", daemon, "-f").CombinedOutput(); err != nil {
		fatal("pitchfork start %s: %v\n%s", daemon, err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("lasso started (pitchfork daemon %q) → http://%s\n", daemon, listenFromArgs(args))
	if hasFlag(args, "tailscale") {
		if dns, err := tailnetDNSName(); err == nil {
			fmt.Printf("tailnet:  https://%s\n", dns)
		} else {
			fmt.Printf("tailnet:  exposing via `tailscale serve` — see `pitchfork logs %s`\n", daemon)
		}
	}
}

// cliStop stops the supervised daemon.
func cliStop() {
	requirePitchfork()
	daemon := lassoDaemon()
	if out, err := exec.Command("pitchfork", "stop", daemon).CombinedOutput(); err != nil {
		fatal("pitchfork stop %s: %v\n%s", daemon, err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("lasso stopped (pitchfork daemon %q)\n", daemon)
}

// cliRestart re-applies any server flags to the daemon config and restarts it.
func cliRestart(args []string) {
	requirePitchfork()
	daemon := lassoDaemon()
	ensureDaemonConfigured(args)
	if err := pitchforkRestart(daemon); err != nil {
		fatal("pitchfork restart %s: %v", daemon, err)
	}
	fmt.Printf("lasso restarted (pitchfork daemon %q)\n", daemon)
}

// cliStatus streams pitchfork's own status for the lasso daemon.
func cliStatus() {
	if _, err := exec.LookPath("pitchfork"); err != nil {
		fmt.Println("lasso: pitchfork not installed — can't report supervised status")
		return
	}
	daemon := lassoDaemon()
	cmd := exec.Command("pitchfork", "status", daemon)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("lasso: not registered as a pitchfork daemon (run `lasso start`)\n")
	}
}

// hasFlag reports whether the args contain a -name / --name boolean flag (in any
// of its forms: -name, --name, -name=true, --name=true).
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		switch a {
		case "-" + name, "--" + name, "-" + name + "=true", "--" + name + "=true":
			return true
		}
	}
	return false
}

// fatal prints to stderr and exits non-zero — for CLI handlers, which (unlike the
// server) shouldn't dump a stack via log.Fatalf.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lasso: "+format+"\n", args...)
	os.Exit(1)
}
