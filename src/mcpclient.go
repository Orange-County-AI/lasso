package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// `lasso mcp-client` — provision the per-host credentials that make caller
// identity real.
//
// One OAuth client per host, its secret handed to that host and nowhere else.
// Because the client id is what a presented token resolves to (see
// mcpTokenVerifier), the host recorded here is the caller's host — derived from
// the credential, not asserted by the caller, which is the only form of it worth
// enforcing on. Scope defaults to "self" (agents on that host see only that
// host); --fleet opts a trusted host up to the whole addressable set.
//
// Secrets are shown ONCE at creation and stored hashed, like every other
// credential in this db. Losing one means minting a new client, not recovering
// the old secret.

// mcpClientRow is one provisioned client as `list` renders it.
type mcpClientRow struct {
	ClientID  string
	Name      string
	Host      string
	Scope     string
	CreatedAt string
}

// cliMCPClient dispatches `lasso mcp-client <add|list|rm>`. It talks to the db
// directly rather than to the running server: sqlite is in WAL mode with a busy
// timeout, so a write alongside a live lasso is safe, and this way provisioning
// works whether or not the server is up.
func cliMCPClient(args []string) {
	if err := openDB(); err != nil {
		fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		mcpClientAdd(args[1:])
	case "list", "ls":
		mcpClientList()
	case "rm", "remove", "revoke":
		mcpClientRemove(args[1:])
	default:
		printMCPClientUsage(os.Stderr)
		os.Exit(2)
	}
}

func printMCPClientUsage(w *os.File) {
	fmt.Fprint(w, `lasso mcp-client — per-host MCP credentials (who is calling, and how far they reach)

usage:
  lasso mcp-client add --host <alias> [--fleet] [--name <label>]
  lasso mcp-client list
  lasso mcp-client rm <client_id>

  --host   the host this credential's agents run on: "local" for the box lasso
           runs on, else an ssh-config alias exactly as list_hosts shows it
  --fleet  let those agents address every host lasso can reach (default: only
           their own host)
  --name   a label for the listing

The printed client_id/client_secret are for the client_credentials grant — put
them on that host and nowhere else. The secret is shown once and stored hashed.

NOTE: this only bites when MCP_OAUTH is set. With it unset /mcp is open and
every caller is unidentified, so it keeps the fleet-wide view.
`)
}

// hostScopedClientCount counts the provisioned per-host credentials, for the
// startup log. Zero on any error — this only feeds a log line, and a db that
// cannot be read has louder problems.
func hostScopedClientCount() int {
	if db == nil {
		return 0
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM oauth_clients WHERE host != ''`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// mcpClientAdd mints a client for one host and prints its credentials once.
func mcpClientAdd(args []string) {
	var host, name string
	scope := scopeSelf
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--host":
			if i+1 < len(args) {
				i++
				host = strings.TrimSpace(args[i])
			}
		case "--name":
			if i+1 < len(args) {
				i++
				name = strings.TrimSpace(args[i])
			}
		case "--fleet":
			scope = scopeFleet
		default:
			fmt.Fprintf(os.Stderr, "lasso mcp-client add: unknown argument %q\n\n", args[i])
			printMCPClientUsage(os.Stderr)
			os.Exit(2)
		}
	}
	if host == "" {
		fmt.Fprint(os.Stderr, "lasso mcp-client add: --host is required\n\n")
		printMCPClientUsage(os.Stderr)
		os.Exit(2)
	}
	// Refuse a host lasso cannot address: a credential for a machine with no ssh
	// alias would authenticate fine and then reach nothing, which is a confusing
	// way to learn the alias is missing.
	if err := requireAddressableHost(host); err != nil {
		fmt.Fprintf(os.Stderr, "lasso mcp-client add: %v\n", err)
		os.Exit(1)
	}
	if name == "" {
		name = "lasso agents on " + host
	}
	id, err := randToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
		os.Exit(1)
	}
	secret, err := randToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
		os.Exit(1)
	}
	// No redirect URIs: this is a machine credential for client_credentials, not
	// a browser client, so there is nowhere to redirect to.
	if _, err := db.Exec(
		`INSERT INTO oauth_clients (client_id, secret_hash, redirect_uris, name, created_at, host, mcp_scope)
		 VALUES (?, ?, '[]', ?, ?, ?, ?)`,
		id, hashToken(secret), name, time.Now().UTC().Format(time.RFC3339), host, scope,
	); err != nil {
		fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("provisioned an MCP credential for host %q (scope %s)\n\n", host, scope)
	fmt.Printf("  client_id:     %s\n", id)
	fmt.Printf("  client_secret: %s\n\n", secret)
	fmt.Print("Shown once — it is stored hashed. Put it on that host only.\n")
	if scope == scopeSelf {
		fmt.Printf("Agents authenticating with it may see and message agents on %q and nowhere else.\n", host)
	} else {
		fmt.Print("Agents authenticating with it may address every host lasso can reach.\n")
	}
}

// mcpClientList shows the provisioned clients. Only host clients are listed:
// DCR registrations are per-connection browser clients, not credentials an
// operator manages, and the MCP_OAUTH client lives in the environment.
func mcpClientList() {
	rows, err := db.Query(
		`SELECT client_id, name, host, mcp_scope, created_at FROM oauth_clients WHERE host != '' ORDER BY host, created_at`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	var out []mcpClientRow
	for rows.Next() {
		var r mcpClientRow
		if err := rows.Scan(&r.ClientID, &r.Name, &r.Host, &r.Scope, &r.CreatedAt); err != nil {
			fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
			os.Exit(1)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		fmt.Println("no per-host MCP credentials provisioned")
		fmt.Println("every caller is unidentified and keeps the fleet-wide view; see `lasso mcp-client add`")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tSCOPE\tCLIENT_ID\tNAME\tCREATED")
	for _, r := range out {
		scope := r.Scope
		if scope == "" {
			scope = scopeFleet // an empty scope on a host client reads as unrestricted
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Host, scope, r.ClientID, r.Name, r.CreatedAt)
	}
	_ = tw.Flush()
}

// mcpClientRemove revokes a client and every token issued to it, so the
// credential stops working immediately rather than at the next expiry.
func mcpClientRemove(args []string) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprint(os.Stderr, "lasso mcp-client rm: a client_id is required\n\n")
		printMCPClientUsage(os.Stderr)
		os.Exit(2)
	}
	id := strings.TrimSpace(args[0])
	res, err := db.Exec(`DELETE FROM oauth_clients WHERE client_id = ?`, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
		os.Exit(1)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fmt.Fprintf(os.Stderr, "lasso: no client %q\n", id)
		os.Exit(1)
	}
	if _, err := db.Exec(`DELETE FROM oauth_tokens WHERE client_id = ?`, id); err != nil {
		fmt.Fprintf(os.Stderr, "lasso: revoked the client but could not drop its tokens: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("revoked client %s and its outstanding tokens\n", id)
}
