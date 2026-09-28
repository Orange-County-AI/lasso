package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

  lasso plugin install <source> [--ref R] [-y] [--no-enable]
                                    install from GitHub: owner/repo[/subdir] or
                                    https://github.com/owner/repo[/tree/<ref>/<subdir>];
                                    shows the permissions first, then asks
  lasso plugin update <name> [-y]   re-fetch a GitHub install's source and ref
  lasso plugin uninstall <name> [--purge-data]
                                    remove a GitHub install (keeps its data dir
                                    unless --purge-data)
  lasso plugin link <path> [--enable]
                                    use a local checkout in place (development)
  lasso plugin unlink <name>        forget a linked checkout (files untouched)
  lasso plugin log <name> [-n 200] [-f]
                                    its MCP server's recent output
  lasso plugin data-dir <name>      print its writable data directory

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
	case "install":
		cliPluginInstall(rest)
	case "update":
		cliPluginUpdate(rest)
	case "uninstall":
		pos, fl := parsePluginFlags(sub, rest, map[string]bool{"purge-data": true}, nil)
		name := oneName(sub, pos)
		var out pluginsPayload
		pluginAPI(http.MethodPost, "/api/plugins/"+name+"/uninstall", map[string]bool{"purge_data": fl.bools["purge-data"]}, &out)
		fmt.Printf("%s: uninstalled\n", name)
	case "link":
		cliPluginLink(rest)
	case "unlink":
		name := needName()
		var out pluginsPayload
		pluginAPI(http.MethodPost, "/api/plugins/"+name+"/unlink", nil, &out)
		fmt.Printf("%s: unlinked (its files were left alone)\n", name)
	case "log", "logs":
		cliPluginLog(rest)
	case "data-dir":
		name := needName()
		var cur pluginsPayload
		pluginAPI(http.MethodGet, "/api/plugins", nil, &cur)
		p := findPluginPayload(cur, name)
		if p == nil {
			fatal("plugin: no plugin %q", name)
		}
		fmt.Println(p.DataDir)
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
	fmt.Fprintln(tw, "NAME\tVERSION\tSTATE\tTRUSTED\tMCP\tSOURCE\tTOOLS")
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Version, p.State, trusted, mcpStatus, pluginSourceLabel(p.Source), tools)
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

// pluginSourceLabel is the list's SOURCE column: github owner/repo@abc1234,
// linked <path>, or local.
func pluginSourceLabel(s pluginSourceOut) string {
	switch s.Kind {
	case pluginSourceGitHub:
		l := "github " + s.Source
		if s.Commit != "" {
			l += "@" + shortCommit(s.Commit)
		}
		return l
	case pluginSourceLinked:
		return "linked " + s.Path
	}
	return pluginSourceLocal
}

// pluginFlags is what parsePluginFlags found.
type pluginFlags struct {
	bools map[string]bool
	vals  map[string]string
}

// parsePluginFlags reads flags anywhere among the arguments (`install x --ref
// v1` and `install --ref v1 x` alike), in -x or --x spelling, with `--k v` or
// `--k=v` for valued ones. Anything unknown is a usage error.
func parsePluginFlags(sub string, args []string, bools map[string]bool, vals map[string]bool) ([]string, pluginFlags) {
	out := pluginFlags{bools: map[string]bool{}, vals: map[string]string{}}
	var pos []string
	usage := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "lasso plugin %s: "+format+"\n\n", append([]any{sub}, a...)...)
		printPluginUsage(os.Stderr)
		os.Exit(2)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		k := strings.TrimLeft(a, "-")
		v, hasV := "", false
		if kk, vv, ok := strings.Cut(k, "="); ok {
			k, v, hasV = kk, vv, true
		}
		switch {
		case bools[k]:
			if hasV {
				usage("-%s takes no value", k)
			}
			out.bools[k] = true
		case vals[k]:
			if !hasV {
				if i+1 >= len(args) {
					usage("-%s needs a value", k)
				}
				i++
				v = args[i]
			}
			out.vals[k] = v
		default:
			usage("unknown flag %s", a)
		}
	}
	return pos, out
}

func oneName(sub string, pos []string) string {
	if len(pos) != 1 || !pluginNameRE.MatchString(pos[0]) {
		fmt.Fprintf(os.Stderr, "lasso plugin %s: expected one plugin name\n\n", sub)
		printPluginUsage(os.Stderr)
		os.Exit(2)
	}
	return pos[0]
}

// confirmPrompt asks a yes/no question on the terminal. Without a terminal
// and without -y it refuses: an install nobody saw is not an install anybody
// approved.
func confirmPrompt(question string, yes bool, hint string, cancel func()) bool {
	if yes {
		return true
	}
	if !promptableStdin() {
		cancel() // the preview is staged server-side; don't leave it to expire
		fatal("plugin: not a terminal, so nothing was confirmed; %s", hint)
	}
	fmt.Printf("%s [y/N] ", question)
	var ans string
	_, _ = fmt.Scanln(&ans)
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}

func printPluginPreview(p pluginPreview) {
	ref := ""
	if p.Ref != "" {
		ref = " (ref " + p.Ref + ")"
	}
	ver := ""
	if p.Version != "" {
		ver = " " + p.Version
	}
	fmt.Printf("%s%s from github %s@%s%s\n", p.Name, ver, p.Source, shortCommit(p.Commit), ref)
	if p.Description != "" {
		fmt.Printf("  %s\n", p.Description)
	}
	fmt.Println("permissions:")
	printPluginPerms(os.Stdout, p.Permissions)
	for _, w := range p.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}

func cliPluginInstall(args []string) {
	pos, fl := parsePluginFlags("install", args, map[string]bool{"y": true, "yes": true, "no-enable": true}, map[string]bool{"ref": true})
	if len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "lasso plugin install: expected one source (owner/repo[/subdir])\n\n")
		printPluginUsage(os.Stderr)
		os.Exit(2)
	}
	yes := fl.bools["y"] || fl.bools["yes"]
	enable := !fl.bools["no-enable"]
	var p pluginPreview
	pluginAPI(http.MethodPost, "/api/plugins/install/preview", map[string]string{"source": pos[0], "ref": fl.vals["ref"]}, &p)
	printPluginPreview(p)
	q := "Install and enable?"
	if !enable {
		q = "Install (disabled)?"
	}
	cancel := func() {
		pluginAPI(http.MethodPost, "/api/plugins/install/cancel", map[string]string{"token": p.Token}, nil)
	}
	if !confirmPrompt(q, yes, "pass -y to install what is shown above", cancel) {
		cancel()
		fmt.Println("cancelled")
		os.Exit(1)
	}
	var out pluginsPayload
	pluginAPI(http.MethodPost, "/api/plugins/install/confirm", map[string]any{"token": p.Token, "fingerprint": p.Fingerprint, "enable": enable}, &out)
	reportPlugin(out, p.Name)
}

func cliPluginUpdate(args []string) {
	pos, fl := parsePluginFlags("update", args, map[string]bool{"y": true, "yes": true}, nil)
	name := oneName("update", pos)
	var p pluginPreview
	pluginAPI(http.MethodPost, "/api/plugins/"+name+"/update/preview", nil, &p)
	printPluginPreview(p)
	fmt.Printf("current commit: %s\n", shortCommit(p.CurrentCommit))
	changes := p.ChangesPermissions != nil && *p.ChangesPermissions
	if changes {
		fmt.Println("permissions change: YES (it will need approval again after the update)")
	} else {
		fmt.Println("permissions change: no")
	}
	if p.CurrentCommit != "" && p.CurrentCommit == p.Commit && !changes {
		fmt.Println("already up to date")
	}
	cancel := func() {
		pluginAPI(http.MethodPost, "/api/plugins/install/cancel", map[string]string{"token": p.Token}, nil)
	}
	if !confirmPrompt("Update?", fl.bools["y"] || fl.bools["yes"], "pass -y to update to what is shown above", cancel) {
		cancel()
		fmt.Println("cancelled")
		os.Exit(1)
	}
	var out pluginsPayload
	pluginAPI(http.MethodPost, "/api/plugins/"+name+"/update/confirm", map[string]string{"token": p.Token, "fingerprint": p.Fingerprint}, &out)
	reportPlugin(out, name)
}

func cliPluginLink(args []string) {
	pos, fl := parsePluginFlags("link", args, map[string]bool{"enable": true}, nil)
	if len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "lasso plugin link: expected one path\n\n")
		printPluginUsage(os.Stderr)
		os.Exit(2)
	}
	abs, err := filepath.Abs(pos[0])
	if err != nil {
		fatal("plugin: %v", err)
	}
	var out pluginsPayload
	pluginAPI(http.MethodPost, "/api/plugins/link", map[string]string{"path": abs}, &out)
	var p *pluginPayload
	for i := range out.Plugins {
		if out.Plugins[i].Source.Kind == pluginSourceLinked && out.Plugins[i].Source.Path == abs {
			p = &out.Plugins[i]
		}
	}
	if p == nil {
		fatal("plugin: linked %s, but the listing does not show it", abs)
	}
	fmt.Printf("%s: linked from %s\n", p.Name, abs)
	if !fl.bools["enable"] {
		reportPlugin(out, p.Name)
		return
	}
	fmt.Printf("approving %s's permissions:\n", p.Name)
	printPluginPerms(os.Stdout, p.Permissions)
	pluginAPI(http.MethodPost, "/api/plugins/"+p.Name+"/enable", map[string]string{"fingerprint": p.Fingerprint}, &out)
	reportPlugin(out, p.Name)
}

func cliPluginLog(args []string) {
	pos, fl := parsePluginFlags("log", args, map[string]bool{"f": true, "follow": true}, map[string]bool{"n": true, "lines": true})
	name := oneName("log", pos)
	n := 200
	if v := fl.vals["n"] + fl.vals["lines"]; v != "" {
		k, err := strconv.Atoi(v)
		if err != nil || k < 1 {
			fatal("plugin: -n wants a positive number")
		}
		n = k
	}
	follow := fl.bools["f"] || fl.bools["follow"]
	var out pluginLogPayload
	pluginAPI(http.MethodGet, "/api/plugins/"+name+"/log?lines="+strconv.Itoa(n), nil, &out)
	if follow && out.Sandboxed {
		// A microVM's log is msb's; follow it there directly (stdin nil — an
		// inherited stdin hangs msb).
		msb, reason, ok := resolveMSB()
		if !ok {
			fatal("plugin: %s", reason)
		}
		cmd := exec.Command(msb, "logs", "-f", "--tail", strconv.Itoa(n), pluginSandboxName(name))
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fatal("plugin: msb logs: %v", err)
		}
		return
	}
	for _, l := range out.Lines {
		fmt.Println(l)
	}
	if out.Note != "" {
		fmt.Fprintf(os.Stderr, "(%s)\n", out.Note)
	}
	if !follow {
		return
	}
	// Trusted: poll the ring and print what is new. Lines carry no ids, so the
	// overlap is found by matching the previous tail.
	prev := out.Lines
	for {
		time.Sleep(2 * time.Second)
		var cur pluginLogPayload
		pluginAPI(http.MethodGet, "/api/plugins/"+name+"/log?lines=500", nil, &cur)
		for _, l := range newLogLines(prev, cur.Lines) {
			fmt.Println(l)
		}
		prev = cur.Lines
	}
}

// newLogLines returns the lines of cur after the longest suffix of prev that
// is a prefix-aligned run in cur — i.e. what arrived since prev was read.
func newLogLines(prev, cur []string) []string {
	if len(prev) == 0 {
		return cur
	}
	for k := min(len(prev), len(cur)); k > 0; k-- {
		tail := prev[len(prev)-k:]
		for start := len(cur) - k; start >= 0; start-- {
			match := true
			for i := range k {
				if cur[start+i] != tail[i] {
					match = false
					break
				}
			}
			if match {
				return cur[start+k:]
			}
		}
	}
	return cur
}

// promptableStdin is isTerminal minus /dev/null, which is a character device
// too — and exactly what a script or an agent's shell hands a command.
func promptableStdin() bool {
	if !isTerminal(os.Stdin) {
		return false
	}
	in, err1 := os.Stdin.Stat()
	null, err2 := os.Stat(os.DevNull)
	return err1 != nil || err2 != nil || !os.SameFile(in, null)
}
