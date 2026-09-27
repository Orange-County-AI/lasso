package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// `lasso plugin` — the Settings pane's plugin controls from a shell.
//
// Unlike `lasso mcp-client`, which writes the db directly, this talks to the
// RUNNING server's /api/plugins: enabling a plugin is not just a row, it is a
// child to start (or a microVM to boot) and tools to mirror onto /mcp, and only
// the server process can do that. So these commands need a running lasso,
// found the way notify and mcp find it (LASSO_URL, else LASSO_LISTEN, else the
// default loopback bind), and send UI_AUTH's basic credentials when set — the
// plugin routes are UI routes.

func printPluginUsage(w io.Writer) {
	fmt.Fprint(w, `lasso plugin — manage lasso plugins (sidebar tabs, MCP tools, themes, fonts)

usage:
  lasso plugin list [-json]         list plugins, their state and MCP status
  lasso plugin enable <name>        approve the plugin's current permissions and load it
  lasso plugin disable <name>       unload it and withdraw the approval
  lasso plugin trust <name>         run its MCP server on the HOST, outside the sandbox
  lasso plugin untrust <name>       back into a microsandbox microVM (the default)
  lasso plugin restart <name>       restart its MCP server
  lasso plugin reload               rescan the plugins directory

Plugins live in <LASSO_DIR or ~/.lasso>/plugins/<name>/plugin.json; see docs/plugins.md.
Talks to the running server (LASSO_URL / LASSO_LISTEN; UI_AUTH if set).
`)
}

func cliPlugin(args []string) {
	if len(args) == 0 || wantsHelp(args) {
		printPluginUsage(os.Stdout)
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	sub, rest := args[0], args[1:]
	needName := func() string {
		if len(rest) != 1 || !pluginNameRE.MatchString(rest[0]) {
			fmt.Fprintf(os.Stderr, "lasso plugin %s: expected one plugin name\n\n", sub)
			printPluginUsage(os.Stderr)
			os.Exit(2)
		}
		return rest[0]
	}
	switch sub {
	case "list", "ls":
		asJSON := len(rest) > 0 && (rest[0] == "-json" || rest[0] == "--json")
		var out pluginsPayload
		pluginAPI(http.MethodGet, "/api/plugins", nil, &out)
		if asJSON {
			b, _ := json.MarshalIndent(out, "", "  ")
			fmt.Println(string(b))
			return
		}
		printPluginList(os.Stdout, out)
	case "enable":
		name := needName()
		// Show what is being approved, then approve exactly that: the
		// fingerprint makes the server refuse if the manifest changed between
		// this read and the enable.
		var cur pluginsPayload
		pluginAPI(http.MethodGet, "/api/plugins", nil, &cur)
		p := findPluginPayload(cur, name)
		if p == nil {
			fatal("plugin: no plugin %q in %s", name, cur.Dir)
		}
		if p.State == pluginStateInvalid {
			fatal("plugin: %s is invalid: %s", name, p.Error)
		}
		fmt.Printf("approving %s's permissions:\n", name)
		printPluginPerms(os.Stdout, p.Permissions)
		var out pluginsPayload
		pluginAPI(http.MethodPost, "/api/plugins/"+name+"/enable", map[string]string{"fingerprint": p.Fingerprint}, &out)
		reportPlugin(out, name)
	case "disable":
		name := needName()
		var out pluginsPayload
		pluginAPI(http.MethodPost, "/api/plugins/"+name+"/disable", nil, &out)
		reportPlugin(out, name)
	case "trust", "untrust":
		name := needName()
		if sub == "trust" {
			fmt.Fprintf(os.Stderr, "warning: a trusted plugin's MCP server runs as your user on this machine, outside the sandbox\n")
		}
		var out pluginsPayload
		pluginAPI(http.MethodPost, "/api/plugins/"+name+"/trust", map[string]bool{"trusted": sub == "trust"}, &out)
		reportPlugin(out, name)
	case "restart":
		name := needName()
		var out pluginsPayload
		pluginAPI(http.MethodPost, "/api/plugins/"+name+"/restart", nil, &out)
		reportPlugin(out, name)
	case "reload":
		var out pluginsPayload
		pluginAPI(http.MethodPost, "/api/plugins/reload", nil, &out)
		printPluginList(os.Stdout, out)
	default:
		fmt.Fprintf(os.Stderr, "lasso plugin: unknown command %q\n\n", sub)
		printPluginUsage(os.Stderr)
		os.Exit(2)
	}
}

// pluginAPI does one request against the running server and decodes the
// answer, exiting with the server's own message on failure.
func pluginAPI(method, path string, body any, out any) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	base := lassoBaseURL()
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		fatal("plugin: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user, pass, ok := parseAuth(os.Getenv("UI_AUTH")); ok {
		req.SetBasicAuth(user, pass)
	}
	// A restart or enable of a sandboxed plugin waits for the previous
	// microVM to be stopped and removed; that is seconds, not minutes.
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		fatal("plugin: reach lasso at %s: %v (is the server running? set LASSO_URL or LASSO_LISTEN)", base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		fatal("plugin: %s", strings.TrimSpace(string(b)))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			fatal("plugin: unexpected answer from %s: %v", base, err)
		}
	}
}

func findPluginPayload(l pluginsPayload, name string) *pluginPayload {
	for i := range l.Plugins {
		if l.Plugins[i].Name == name {
			return &l.Plugins[i]
		}
	}
	return nil
}

func reportPlugin(l pluginsPayload, name string) {
	p := findPluginPayload(l, name)
	if p == nil {
		fmt.Printf("%s: gone\n", name)
		return
	}
	line := fmt.Sprintf("%s: %s", name, p.State)
	if p.Trusted {
		line += ", trusted"
	}
	if p.MCP != nil {
		line += ", mcp " + p.MCP.Status
		if p.MCP.Detail != "" {
			line += " (" + p.MCP.Detail + ")"
		}
	}
	fmt.Println(line)
}

func printPluginList(w io.Writer, l pluginsPayload) {
	fmt.Fprintf(w, "plugins directory: %s\n", l.Dir)
	if l.MSB.Available {
		fmt.Fprintf(w, "microsandbox:      %s\n", l.MSB.Path)
	} else {
		fmt.Fprintf(w, "microsandbox:      unavailable — %s\n", l.MSB.Reason)
	}
	if len(l.Plugins) == 0 {
		fmt.Fprintln(w, "\nno plugins installed")
		return
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tSTATE\tTRUSTED\tMCP\tTOOLS")
	for _, p := range l.Plugins {
		mcpStatus, tools := "-", "-"
		if p.MCP != nil {
			mcpStatus = p.MCP.Status
			if len(p.MCP.Tools) > 0 {
				tools = strings.Join(p.MCP.Tools, ",")
			}
		}
		trusted := "no"
		if p.Trusted {
			trusted = "YES"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Version, p.State, trusted, mcpStatus, tools)
	}
	_ = tw.Flush()
	for _, p := range l.Plugins {
		switch {
		case p.Error != "":
			fmt.Fprintf(w, "  %s: %s\n", p.Name, p.Error)
		case p.MCP != nil && p.MCP.Detail != "":
			fmt.Fprintf(w, "  %s: %s\n", p.Name, p.MCP.Detail)
		}
		for _, warn := range p.Warnings {
			fmt.Fprintf(w, "  %s: warning: %s\n", p.Name, warn)
		}
	}
}

func printPluginPerms(w io.Writer, p pluginPerms) {
	for _, t := range p.Tabs {
		where := t.Entry
		if t.URL != "" {
			where = t.URL
		}
		fmt.Fprintf(w, "  tab %-12s %s\n", t.ID, where)
	}
	if len(p.Themes) > 0 {
		fmt.Fprintf(w, "  themes        %s\n", strings.Join(p.Themes, ", "))
	}
	if len(p.Fonts) > 0 {
		fonts := make([]string, 0, len(p.Fonts))
		for _, f := range p.Fonts {
			fonts = append(fonts, fmt.Sprintf("%s (%s)", f.Family, f.Category))
		}
		fmt.Fprintf(w, "  fonts         %s\n", strings.Join(fonts, ", "))
	}
	if p.MCP == nil {
		return
	}
	fmt.Fprintf(w, "  mcp image     %s\n", p.MCP.Image)
	fmt.Fprintf(w, "  mcp command   %s\n", strings.Join(p.MCP.Command, " "))
	if len(p.MCP.Network) == 0 {
		fmt.Fprintf(w, "  mcp network   none (no egress)\n")
	} else {
		fmt.Fprintf(w, "  mcp network   %s\n", strings.Join(p.MCP.Network, ", "))
	}
	if len(p.MCP.EnvKeys) > 0 {
		fmt.Fprintf(w, "  mcp env       %s\n", strings.Join(p.MCP.EnvKeys, ", "))
	}
	for _, s := range p.MCP.Secrets {
		fmt.Fprintf(w, "  mcp secret    %s -> %s\n", s.Name, strings.Join(s.Hosts, ", "))
	}
}
