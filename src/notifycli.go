package main

// `lasso notify` — an agent gets its human's attention from a shell.
//
// It is a client of the `notify` MCP tool rather than a second implementation of
// it: one description the model reads, one set of rules about attribution and
// rate limiting, one place where "sent" is decided. An agent with lasso's MCP
// server configured can call the tool directly; one that only has a terminal —
// which is most of them, most of the time — types this instead and gets exactly
// the same behavior.
//
// That does mean this speaks Streamable HTTP MCP to the local server, which is
// the one thing `lasso closeme` deliberately avoids (it POSTs a plain endpoint).
// The difference is what the two commands are: closeme is lasso acting on its
// own agent record, while notify is an agent using a tool. Making the CLI the
// odd one out — its own /api endpoint, its own defaults — is how the two drift.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// notifyCLITimeout bounds the whole round trip: connect, initialize, call. A
// push service can be slow, and the tool waits for it on purpose (it reports
// whether the notification actually landed), so this sits above
// notifDeliverTimeout rather than under it.
const notifyCLITimeout = 45 * time.Second

func printNotifyUsage(w *os.File) {
	fmt.Fprint(w, `lasso notify — push a notification to the human who runs this lasso

usage:
  lasso notify [flags] <message...>
  <command> | lasso notify [flags]        read the message from stdin

flags:
  -title <text>   headline (default: your agent's name, resolved from your pane)
  -pane <id>      your herdr pane id (default: $HERDR_PANE_ID)
  -host <alias>   host you run on (default: resolved from the pane)

It reaches a locked phone, so use it when you actually need the human: a
decision only they can make, a question that blocks you, a long job finishing
while they are away. Exits non-zero when nothing is subscribed — so a script
can tell "delivered" from "nobody was listening".

environment:
  LASSO_LISTEN      host:port of the local lasso (default `+defaultListenAddr+`)
  LASSO_URL         full base URL, if lasso is not on plain http loopback
  LASSO_MCP_TOKEN   bearer token, when /mcp is gated by MCP_OAUTH
  UI_AUTH           user:pass, when the server runs behind basic auth
`)
}

// cliNotify parses the flags and reports the outcome. The exit code is the
// contract for a script: 0 only when a device actually took the notification.
func cliNotify(args []string) {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printNotifyUsage(os.Stderr) }
	title := fs.String("title", "", "headline (default: your agent's name)")
	pane := fs.String("pane", os.Getenv("HERDR_PANE_ID"), "your herdr pane id")
	host := fs.String("host", "", "host you run on")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	msg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if msg == "" {
		// No positional message: accept a piped one, so `make test 2>&1 | tail -5
		// | lasso notify` works. A terminal on stdin means the user just forgot
		// the message, and reading it would hang looking like nothing happened.
		if isTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, "lasso notify: no message given")
			printNotifyUsage(os.Stderr)
			os.Exit(2)
		}
		piped, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<10))
		if err != nil {
			fatal("notify: read stdin: %v", err)
		}
		msg = strings.TrimSpace(string(piped))
	}
	if msg == "" {
		fmt.Fprintln(os.Stderr, "lasso notify: the message is empty")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), notifyCLITimeout)
	defer cancel()
	out, err := callNotifyTool(ctx, mcpEndpoint(), mcpCLIClient(), notifyIn{
		Message: msg,
		Title:   *title,
		PaneID:  *pane,
		Host:    *host,
	})
	if err != nil {
		fatal("notify: %v", err)
	}
	if !out.Sent {
		// Not a crash — the call worked, there was just nobody to tell. Said on
		// stderr with a non-zero exit so an agent does not report to its human
		// that it pinged them.
		fmt.Fprintf(os.Stderr, "lasso notify: not delivered — %s\n", out.Detail)
		os.Exit(1)
	}
	line := fmt.Sprintf("notified: %q via %s", out.Title, strings.Join(out.Transports, ", "))
	if out.Detail != "" {
		line += " (" + out.Detail + ")"
	}
	fmt.Println(line)
}

// mcpEndpoint is the local server's /mcp URL. LASSO_URL wins for a lasso that
// is not on plain http loopback (a tunnel, a TLS terminator); LASSO_LISTEN
// covers the common case of a non-default port, matching closeme.
func mcpEndpoint() string {
	if base := strings.TrimSpace(os.Getenv("LASSO_URL")); base != "" {
		return strings.TrimSuffix(base, "/") + "/mcp"
	}
	addr := defaultListenAddr
	if env := strings.TrimSpace(os.Getenv("LASSO_LISTEN")); env != "" {
		addr = env
	}
	return "http://" + addr + "/mcp"
}

// mcpCLIClient is an HTTP client carrying whatever credential the environment
// offers. /mcp is open by default, so both are usually absent; a lasso with
// MCP_OAUTH set accepts either a bearer token or the UI_AUTH basic credentials
// (see withMCPAuth), and this sends whichever it has.
func mcpCLIClient() *http.Client {
	token := strings.TrimSpace(os.Getenv("LASSO_MCP_TOKEN"))
	user, pass, hasAuth := parseAuth(os.Getenv("UI_AUTH"))
	if token == "" && !hasAuth {
		return http.DefaultClient
	}
	return &http.Client{Transport: cliAuthTransport{token: token, user: user, pass: pass, basic: hasAuth}}
}

type cliAuthTransport struct {
	token, user, pass string
	basic             bool
}

func (t cliAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	switch {
	case t.token != "":
		r.Header.Set("Authorization", "Bearer "+t.token)
	case t.basic:
		r.SetBasicAuth(t.user, t.pass)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// callNotifyTool connects to endpoint, calls the notify tool once, and decodes
// its structured output. Split from cliNotify (which owns the flags and the exit
// codes) so a test can drive the real /mcp handler over httptest.
func callNotifyTool(ctx context.Context, endpoint string, hc *http.Client, in notifyIn) (notifyOut, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "lasso-cli", Version: lassoSemver}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: hc,
		// Nothing here consumes server-initiated messages, and a one-shot command
		// must not hold a second connection open for them.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return notifyOut{}, fmt.Errorf("reach lasso's MCP endpoint at %s: %w (is the server running? set LASSO_LISTEN for a non-default port)", endpoint, err)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "notify",
		Arguments: map[string]any{
			"message": in.Message,
			"title":   in.Title,
			"pane_id": in.PaneID,
			"host":    in.Host,
		},
	})
	if err != nil {
		return notifyOut{}, err
	}
	if res.IsError {
		return notifyOut{}, errors.New(toolErrorText(res))
	}
	// The SDK fills StructuredContent from the tool's typed Out value, so this
	// round-trips through JSON rather than being handed the struct directly.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return notifyOut{}, fmt.Errorf("tool result: %w", err)
	}
	var out notifyOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return notifyOut{}, fmt.Errorf("tool result: %w", err)
	}
	return out, nil
}

// toolErrorText joins the text content of a failed tool call — where the SDK
// puts a handler's error, rather than in a protocol-level error.
func toolErrorText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteString("; ")
			}
			b.WriteString(strings.TrimSpace(tc.Text))
		}
	}
	if b.Len() == 0 {
		return "the notify tool failed without saying why"
	}
	return b.String()
}

// isTerminal reports whether f is a tty, so a missing message is a usage error
// rather than a command that hangs on an empty stdin.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
