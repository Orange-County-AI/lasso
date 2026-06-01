package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// `lasso cli <subcommand>` is a thin, JSON-in / JSON-out interface over THIS
// host's own state DB (~/.lasso/lasso.db) and filesystem. A controlling lasso
// runs it on a remote host over SSH (see remoteBackend.runCLI) to read/write
// that host's settings against its own database — so each host owns its config.
// It operates on host "local" (every host is "local" to itself) and never starts
// a server. Stdout carries the JSON result; a non-zero exit + stderr signals
// failure. Today it covers creator settings; later it will grow more commands.
func runCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: lasso cli <config-get|config-set|repos|repo-config-get|repo-config-set|repo-branches>")
		return 2
	}
	// ~-expansion and git/fs ops route through curBackend(); the CLI drives the
	// local machine, so point it at a localBackend before anything calls them.
	setBackend(&localBackend{sock: defaultSock()})
	if err := openDB(); err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		return 1
	}
	defer db.Close()

	const host = "local"
	be := curBackend()
	cmd, rest := args[0], args[1:]

	var (
		result any
		err    error
	)
	switch cmd {
	case "config-get":
		var s appSettings
		s, err = getSettings()
		result = creatorDefaults{s.ReposRoot, s.BranchPrefix, s.DefaultAgent, s.ScratchSetup}
	case "config-set":
		var p defaultsPatch
		if err = decodeStdin(&p); err == nil {
			if err = applyDefaults(p); err == nil {
				var s appSettings
				if s, err = getSettings(); err == nil {
					result = creatorDefaults{s.ReposRoot, s.BranchPrefix, s.DefaultAgent, s.ScratchSetup}
				}
			}
		}
	case "repos":
		var (
			root  string
			repos []repoEntry
		)
		root, repos, err = reposList(be, host)
		result = map[string]any{"root": root, "repos": repos}
	case "repo-config-get":
		if len(rest) == 0 || rest[0] == "" {
			err = fmt.Errorf("path required")
		} else {
			result, err = getRepoState(host, expandTilde(rest[0]))
		}
	case "repo-config-set":
		var p repoConfigPatch
		if err = decodeStdin(&p); err == nil {
			path := expandTilde(strings.TrimSpace(p.Path))
			if path == "" {
				err = fmt.Errorf("path required")
			} else {
				result, err = applyRepoConfig(host, path, p.CopyFiles, p.Setup)
			}
		}
	case "repo-branches":
		if len(rest) == 0 || rest[0] == "" {
			err = fmt.Errorf("path required")
		} else {
			local, remote, def := branchList(be, expandTilde(rest[0]))
			result = map[string]any{"branches": local, "remoteBranches": remote, "default": def}
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown cli subcommand %q\n", cmd)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return 0
}

// decodeStdin reads stdin and unmarshals it into v; an empty stdin leaves v at
// its zero value (so a no-body call is a valid "change nothing" request).
func decodeStdin(v any) error {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}
