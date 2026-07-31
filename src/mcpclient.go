package main

import (
	"fmt"
	"os"
	"strconv"
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
// enforcing on. Scope defaults to "self" (agents on that host see that host,
// plus whatever host groups add to it — see `lasso mcp-group`); --fleet opts a
// trusted host up to the whole addressable set.
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
	// Tokens is how many access tokens are outstanding for this client, and
	// Immortal how many of those never expire. Together they answer the question
	// an operator actually has in front of this table — "have I already keyed
	// this host?" — which the client row alone cannot.
	Tokens   int
	Immortal int
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
	case "token":
		mcpClientToken(args[1:])
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
  lasso mcp-client token <client_id> [--ttl <duration>]
  lasso mcp-client rm <client_id>

  --host   the host this credential's agents run on: "local" for the box lasso
           runs on, else an ssh-config alias exactly as list_hosts shows it
  --fleet  let those agents address every host lasso can reach (default: their
           own host, plus any host their host's groups reach -- see
           "lasso mcp-group")
  --name   a label for the listing
  --ttl    how long the minted token lives: 90d, 12h, 30m, 2w. Omit (or pass
           "never") for a token that does not expire — the default, since these
           sit unattended in a host's MCP config where a rolling expiry is an
           outage on a timer, not a security win.

The printed client_id/client_secret are for the client_credentials grant — put
them on that host and nowhere else. The secret is shown once and stored hashed.

The token subcommand mints a bearer token directly, for pasting into an MCP
client that has no way to run a grant (claude mcp add --header "Authorization:
Bearer ..."). To revoke one, remove its client with rm — that drops every token
issued to it — then add a replacement. One client is one host, so that blast
radius is exactly the host you are re-keying.

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
		fmt.Printf("Agents authenticating with it may see and message agents on %q, plus any host\n", host)
		fmt.Printf("a group puts within reach of %q — `lasso mcp-group reach %s` shows the exact set.\n", host, host)
	} else {
		fmt.Print("Agents authenticating with it may address every host lasso can reach.\n")
	}
}

// mcpClientList shows the provisioned clients. Only host clients are listed:
// DCR registrations are per-connection browser clients, not credentials an
// operator manages, and the MCP_OAUTH client lives in the environment.
func mcpClientList() {
	rows, err := db.Query(
		`SELECT c.client_id, c.name, c.host, c.mcp_scope, c.created_at,
		        (SELECT COUNT(*) FROM oauth_tokens t
		          WHERE t.client_id = c.client_id AND t.kind = 'access') AS tokens,
		        (SELECT COUNT(*) FROM oauth_tokens t
		          WHERE t.client_id = c.client_id AND t.kind = 'access' AND t.expires_at = '') AS immortal
		   FROM oauth_clients c WHERE c.host != '' ORDER BY c.host, c.created_at`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	var out []mcpClientRow
	for rows.Next() {
		var r mcpClientRow
		if err := rows.Scan(&r.ClientID, &r.Name, &r.Host, &r.Scope, &r.CreatedAt, &r.Tokens, &r.Immortal); err != nil {
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
	fmt.Fprintln(tw, "HOST\tSCOPE\tTOKENS\tCLIENT_ID\tNAME\tCREATED")
	for _, r := range out {
		scope := r.Scope
		if scope == "" {
			scope = scopeFleet // an empty scope on a host client reads as unrestricted
		}
		tokens := strconv.Itoa(r.Tokens)
		if r.Immortal > 0 {
			tokens = fmt.Sprintf("%d (%d never expire)", r.Tokens, r.Immortal)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Host, scope, tokens, r.ClientID, r.Name, r.CreatedAt)
	}
	_ = tw.Flush()
}

// parseTTL reads a --ttl value. Go's ParseDuration stops at hours, but the
// useful units for a machine credential are days and weeks, so those are
// handled here. An empty value, "never", "none", or "0" means no expiry.
func parseTTL(s string) (time.Duration, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "never", "none", "0":
		return 0, nil
	}
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if num, ok := strings.CutSuffix(s, suffix); ok {
			n, err := strconv.ParseFloat(num, 64)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad --ttl %q: expected something like 90d, 2w, 12h, or \"never\"", s)
			}
			return time.Duration(n * float64(unit)), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad --ttl %q: expected something like 90d, 2w, 12h, or \"never\"", s)
	}
	return d, nil
}

// mintClientToken issues a bearer token for a provisioned host client. Split
// from the CLI wrapper so it is testable without os.Exit, and so the guard
// below — machine tokens are only for credentials an operator minted — lives
// with the minting rather than with the argument parsing.
func mintClientToken(clientID string, ttl time.Duration) (string, oauthClient, error) {
	c, found := lookupOAuthClient(clientID)
	if !found {
		return "", oauthClient{}, fmt.Errorf("no client %q — `lasso mcp-client list` shows the provisioned ones", clientID)
	}
	// Same rule as the client_credentials grant: a self-registered DCR client
	// must not get a machine token, whether it asks over HTTP or an operator
	// fat-fingers its id here.
	if !c.Static && c.Host == "" {
		return "", oauthClient{}, fmt.Errorf("client %q is not a per-host credential (it registered itself via DCR), so it gets no machine token — mint one with `lasso mcp-client add --host <alias>`", clientID)
	}
	tok, err := issueToken("access", c.ID, oauthScope, ttl)
	if err != nil {
		return "", oauthClient{}, err
	}
	return tok, c, nil
}

// mcpClientToken mints a long-lived bearer token for a host's MCP client config.
func mcpClientToken(args []string) {
	var id string
	ttlArg := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ttl":
			if i+1 < len(args) {
				i++
				ttlArg = args[i]
			}
		default:
			if strings.HasPrefix(args[i], "-") || id != "" {
				fmt.Fprintf(os.Stderr, "lasso mcp-client token: unexpected argument %q\n\n", args[i])
				printMCPClientUsage(os.Stderr)
				os.Exit(2)
			}
			id = args[i]
		}
	}
	if id == "" {
		fmt.Fprint(os.Stderr, "lasso mcp-client token: a client_id is required\n\n")
		printMCPClientUsage(os.Stderr)
		os.Exit(2)
	}
	ttl, err := parseTTL(ttlArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso mcp-client token: %v\n", err)
		os.Exit(2)
	}
	tok, c, err := mintClientToken(id, ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lasso mcp-client token: %v\n", err)
		os.Exit(1)
	}
	lifetime := "never expires"
	if ttl > 0 {
		lifetime = "expires " + time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	host := c.Host
	if host == "" {
		host = "(not host-scoped)"
	}
	fmt.Printf("bearer token for host %s — %s\n\n", host, lifetime)
	fmt.Printf("  %s\n\n", tok)
	fmt.Print("Shown once — only its hash is stored. Put it in that host's MCP client config,\n")
	fmt.Print("e.g. claude mcp add --transport http --scope user lasso <url> \\\n")
	fmt.Print("       --header \"Authorization: Bearer <token>\"\n")
	if !oauthCfg.Enabled {
		fmt.Print("\nNOTE: MCP_OAUTH is unset, so /mcp accepts everything and this token is not\n")
		fmt.Print("enforced — every caller is unidentified and fleet-scoped until you set it.\n")
	}
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
