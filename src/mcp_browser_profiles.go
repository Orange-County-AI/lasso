package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Browser profiles and tabs over MCP: an agent can create, rename, re-proxy
// and delete profiles (browserprofiles.go), and open, show, list and close
// tabs in any of them, with the tab it opens appearing on the human's screen
// the way open_file puts a file there — a `browser-open` SSE event every
// visible lasso tab acts on (lib/browser-profiles.ts).
//
// These tools manage WHICH pages exist and which one the human is looking at.
// Driving a page (clicking, typing, reading it) is still /browser-mcp's job —
// one server for every profile, each tool taking `profile` — or raw CDP's at
// /cdp/p/<id>.

const browserProfileArg = "Browser profile: its id (e.g. \"work\") or its display name. Omit for the default profile. list_browser_profiles shows them."

// browserProfileOut is a profile as the tools report it, with absolute
// endpoints when lasso could tell which URL the caller reached it on.
type browserProfileOut struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Proxy       string        `json:"proxy"`
	Default     bool          `json:"default"`
	Running     bool          `json:"running"`
	Tabs        []browserPage `json:"tabs"`
	MCPEndpoint string        `json:"mcp_endpoint"` // the one /browser-mcp URL; its tools take profile: <id>
	WSEndpoint  string        `json:"ws_endpoint"`  // CDP websocket for THIS profile's browser
	Note        string        `json:"note,omitempty"`
}

func profileOut(req *mcp.CallToolRequest, st browserProfileStatus) browserProfileOut {
	o := browserProfileOut{ID: st.ID, Name: st.Name, Proxy: st.Proxy, Default: st.Default,
		Running: st.Running, Tabs: st.Pages, MCPEndpoint: st.MCPPath, WSEndpoint: st.WSPath}
	if o.Tabs == nil {
		o.Tabs = []browserPage{}
	}
	if base := sharedBrowserBase(req); base != "" {
		o.MCPEndpoint = base + st.MCPPath
		o.WSEndpoint = "ws" + strings.TrimPrefix(base, "http") + st.WSPath
	}
	if !st.Running && st.Reason != "" {
		o.Note = st.Reason
	}
	return o
}

// requireLocalBrowser is every browser tool's gate: the browsers run on
// lasso's own machine, and a credential confined elsewhere does not reach it.
func requireLocalBrowser(req *mcp.CallToolRequest) error {
	cs := callerFrom(req)
	if !cs.allows("local") {
		return fmt.Errorf("lasso's browsers run on lasso's own machine, which is outside this credential's reach (%s)", cs.reachSummary())
	}
	return nil
}

// ---- list_browser_profiles --------------------------------------------------

const listBrowserProfilesDescription = "List lasso's shared-browser PROFILES. Each profile is its own Chromium on lasso's machine, with its own persistent cookies and logins and optionally its own proxy (e.g. socks5://host:1080), shown to the human in lasso's Browser tab (they pick the profile at the bottom of it). `default` is the profile lasso always had. For each profile you get its open `tabs` (only while it runs), `mcp_endpoint` — lasso's ONE browser MCP URL (/browser-mcp, the same for every profile): its chrome-devtools-mcp tools each take an optional `profile`, so pass this profile's `id` to drive its browser; no per-profile MCP server is needed — and `ws_endpoint` for raw CDP/Playwright, which IS per profile."

type listBrowserProfilesIn struct{}

type listBrowserProfilesOut struct {
	Profiles []browserProfileOut `json:"profiles"`
}

func listBrowserProfilesTool(ctx context.Context, req *mcp.CallToolRequest, _ listBrowserProfilesIn) (*mcp.CallToolResult, listBrowserProfilesOut, error) {
	if err := requireLocalBrowser(req); err != nil {
		return nil, listBrowserProfilesOut{}, err
	}
	out := listBrowserProfilesOut{Profiles: []browserProfileOut{}}
	for _, st := range sharedBrowsers.statuses() {
		out.Profiles = append(out.Profiles, profileOut(req, st))
	}
	return nil, out, nil
}

// ---- create / update / delete -------------------------------------------------

const createBrowserProfileDescription = "Create a shared-browser profile: a separate Chromium with its own persistent cookies/logins and, optionally, a proxy all its traffic goes through. It appears in the human's Browser-tab profile picker at once. It starts on first use (open_browser_tab, or connecting to its endpoints). `proxy` is scheme://host[:port] with scheme socks5, socks4, http or https — no credentials (Chromium cannot authenticate to a SOCKS proxy) and no socks5h (Chromium's socks5 already resolves DNS through the proxy). `id` is optional and derived from the name when omitted; it is what you pass as `profile` to the /browser-mcp tools, and what appears in its CDP URL (/cdp/p/<id>). The /browser-mcp server you already have drives it at once: no new MCP server, no reconnect."

type createBrowserProfileIn struct {
	Name  string `json:"name" jsonschema:"Display name, e.g. \"Work\" or \"US exit\"."`
	ID    string `json:"id,omitempty" jsonschema:"Optional id: 1-32 lowercase letters, digits or dashes. Derived from the name when omitted."`
	Proxy string `json:"proxy,omitempty" jsonschema:"Optional proxy for all of this profile's traffic: socks5://host:port (also socks4, http, https). Omit for a direct connection."`
}

func createBrowserProfileTool(ctx context.Context, req *mcp.CallToolRequest, in createBrowserProfileIn) (*mcp.CallToolResult, browserProfileOut, error) {
	if err := requireLocalBrowser(req); err != nil {
		return nil, browserProfileOut{}, err
	}
	if sharedBrowsers == nil {
		return nil, browserProfileOut{}, errors.New("browser profiles are not configured on this lasso")
	}
	p, err := sharedBrowsers.create(in.Name, in.ID, in.Proxy)
	if err != nil {
		return nil, browserProfileOut{}, err
	}
	browserProfilesChanged()
	return nil, profileOut(req, sharedBrowsers.statusOf(p)), nil
}

const updateBrowserProfileDescription = "Rename a shared-browser profile and/or change its proxy. Pass only what changes: `proxy: \"\"` switches it to a direct connection, an omitted proxy leaves it alone. A proxy change on a RUNNING profile relaunches its Chromium (Chromium reads its proxy once, at launch): the URLs of its open tabs are reopened, but tab ids change and any CDP connection to it is closed and must reconnect, and /browser-mcp's page ids for it are gone (its next call starts fresh: list_pages again). The default profile can be renamed and re-proxied too."

type updateBrowserProfileIn struct {
	Profile string  `json:"profile" jsonschema:"The profile to change: its id or display name."`
	Name    *string `json:"name,omitempty" jsonschema:"New display name."`
	Proxy   *string `json:"proxy,omitempty" jsonschema:"New proxy (socks5://host:port, socks4, http, https), or \"\" for a direct connection. Omit to leave it unchanged."`
}

func updateBrowserProfileTool(ctx context.Context, req *mcp.CallToolRequest, in updateBrowserProfileIn) (*mcp.CallToolResult, browserProfileOut, error) {
	if err := requireLocalBrowser(req); err != nil {
		return nil, browserProfileOut{}, err
	}
	if strings.TrimSpace(in.Profile) == "" {
		return nil, browserProfileOut{}, errors.New("profile is required")
	}
	if in.Name == nil && in.Proxy == nil {
		return nil, browserProfileOut{}, errors.New("nothing to change: pass name and/or proxy")
	}
	if sharedBrowsers == nil {
		return nil, browserProfileOut{}, errors.New("browser profiles are not configured on this lasso")
	}
	id, err := resolveProfile(in.Profile)
	if err != nil {
		return nil, browserProfileOut{}, err
	}
	st, err := sharedBrowsers.edit(ctx, id, in.Name, in.Proxy)
	var rl errRelaunch
	if errors.As(err, &rl) {
		out := profileOut(req, st)
		out.Note = rl.Error()
		return nil, out, nil
	}
	if err != nil {
		return nil, browserProfileOut{}, err
	}
	return nil, profileOut(req, st), nil
}

const deleteBrowserProfileDescription = "Delete a shared-browser profile. Its Chromium is stopped (closing its tabs and any session driving it) and its profile directory is DELETED, with every cookie and login in it — this cannot be undone, so only do it when the human asked. The default profile cannot be deleted."

type deleteBrowserProfileIn struct {
	Profile string `json:"profile" jsonschema:"The profile to delete: its id or display name."`
}

type deleteBrowserProfileOut struct {
	Deleted string `json:"deleted"` // the id that was deleted
}

func deleteBrowserProfileTool(ctx context.Context, req *mcp.CallToolRequest, in deleteBrowserProfileIn) (*mcp.CallToolResult, deleteBrowserProfileOut, error) {
	if err := requireLocalBrowser(req); err != nil {
		return nil, deleteBrowserProfileOut{}, err
	}
	if strings.TrimSpace(in.Profile) == "" {
		return nil, deleteBrowserProfileOut{}, errors.New("profile is required")
	}
	if sharedBrowsers == nil {
		return nil, deleteBrowserProfileOut{}, errors.New("browser profiles are not configured on this lasso")
	}
	id, err := resolveProfile(in.Profile)
	if err != nil {
		return nil, deleteBrowserProfileOut{}, err
	}
	if err := sharedBrowsers.remove(ctx, id); err != nil {
		return nil, deleteBrowserProfileOut{}, err
	}
	browserProfilesChanged()
	return nil, deleteBrowserProfileOut{Deleted: id}, nil
}

// ---- tabs --------------------------------------------------------------------

const listBrowserTabsDescription = "List the open tabs of lasso's shared browser, per profile. With `profile`, just that one (starting nothing: a stopped profile has no tabs). Without it, every profile. Tab ids are CDP target ids, usable with show_browser_tab and close_browser_tab (and as targetId over CDP)."

type listBrowserTabsIn struct {
	Profile string `json:"profile,omitempty" jsonschema:"Only this profile (id or display name). Omit for every profile."`
}

type browserTabsOfProfile struct {
	Profile string        `json:"profile"`
	Name    string        `json:"name"`
	Running bool          `json:"running"`
	Tabs    []browserPage `json:"tabs"`
}

type listBrowserTabsOut struct {
	Profiles []browserTabsOfProfile `json:"profiles"`
}

func listBrowserTabsTool(ctx context.Context, req *mcp.CallToolRequest, in listBrowserTabsIn) (*mcp.CallToolResult, listBrowserTabsOut, error) {
	if err := requireLocalBrowser(req); err != nil {
		return nil, listBrowserTabsOut{}, err
	}
	out := listBrowserTabsOut{Profiles: []browserTabsOfProfile{}}
	add := func(st browserProfileStatus) {
		tabs := st.Pages
		if tabs == nil {
			tabs = []browserPage{}
		}
		out.Profiles = append(out.Profiles, browserTabsOfProfile{Profile: st.ID, Name: st.Name, Running: st.Running, Tabs: tabs})
	}
	if strings.TrimSpace(in.Profile) != "" {
		id, err := resolveProfile(in.Profile)
		if err != nil {
			return nil, listBrowserTabsOut{}, err
		}
		st, err := sharedBrowsers.profileStatus(id)
		if err != nil {
			return nil, listBrowserTabsOut{}, err
		}
		add(st)
		return nil, out, nil
	}
	for _, st := range sharedBrowsers.statuses() {
		add(st)
	}
	return nil, out, nil
}

const openBrowserTabDescription = "Open a NEW tab in lasso's shared browser and put it on the human's screen: every visible lasso tab switches its Browser tab to that profile and tab (opening the sidebar if needed), the way open_file shows a file. Use it when the human asks you to open a page for them, or in a particular profile (\"open this in my work profile\"). Starts the profile's browser if it is stopped. The page loads from LASSO's machine, through the profile's proxy if it has one, so `localhost` means lasso's machine. To then drive the page, use the /browser-mcp tools (`mcp_endpoint`) with `profile` set to this profile (find the tab with list_pages by its URL), or `ws_endpoint`. Check `delivered`: 0 means no lasso tab is open and the human did NOT see it (the tab is still open in the browser). A profile's browser stops after lasso's idle timeout with nothing connected to it, and its tabs close with it: keep a /browser-mcp or CDP session open while you still need the page. Pass your $HERDR_PANE_ID as pane_id so the human is told which agent opened it."

type openBrowserTabIn struct {
	URL     string `json:"url" jsonschema:"The page to open: a full http(s) URL. A bare host[:port] gets https:// (http:// for localhost/127.0.0.1). about:blank opens an empty tab."`
	Profile string `json:"profile,omitempty" jsonschema:"Browser profile (id or display name) to open it in. Omit for the default profile."`
	Show    *bool  `json:"show,omitempty" jsonschema:"Put the tab on the human's screen (default true). false opens it quietly, for a page you will work in without switching their view."`
	PaneID  string `json:"pane_id,omitempty" jsonschema:"Your own herdr pane id ($HERDR_PANE_ID), used only to tell the human which agent opened the tab."`
}

type browserTabOut struct {
	TabID       string `json:"tab_id"`
	Profile     string `json:"profile"`
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Delivered   int    `json:"delivered"` // lasso tabs the show request reached; 0 = nobody saw it
	MCPEndpoint string `json:"mcp_endpoint,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// browserOpenEvent is the `browser-open` SSE payload the Browser tab acts on.
type browserOpenEvent struct {
	Profile string `json:"profile"`
	TabID   string `json:"tab_id"`
	URL     string `json:"url"`
	From    string `json:"from"`
}

// browserOpenBroadcast hands the event to every connected tab and reports how
// many took it; 0 before the server is up (tests swap it).
var browserOpenBroadcast = func(ev browserOpenEvent) int {
	if srvHub == nil {
		return 0
	}
	return srvHub.broadcast("browser-open", ev)
}

// normalizeTabURL turns what an agent passes into a URL Chromium will load.
// Schemes are limited to the web and about:blank; file:, chrome: and
// javascript: are refused — this is a tool for showing a human a page, and
// those are not pages to show.
func normalizeTabURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("url is required")
	}
	if s == "about:blank" {
		return s, nil
	}
	if !strings.Contains(s, "://") {
		host := s
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		h := strings.ToLower(host)
		if h == "localhost" || strings.HasPrefix(h, "localhost:") || strings.HasPrefix(h, "127.") || strings.HasPrefix(h, "[::1]") {
			s = "http://" + s
		} else {
			s = "https://" + s
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("url %q is not a URL lasso can open", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", fmt.Errorf("url %q: only http(s) pages (or about:blank) can be opened this way", raw)
	}
	return u.String(), nil
}

// callerName is who the human is told opened something: the agent's name when
// pane_id resolves, "an agent" otherwise, with the reason in detail.
func callerName(ctx context.Context, req *mcp.CallToolRequest, paneID string) (from, detail string) {
	from = "an agent"
	if strings.TrimSpace(paneID) == "" {
		return from, ""
	}
	who, err := resolveCallerAgent(ctx, callerFrom(req), "", paneID)
	switch {
	case err != nil:
		return from, "could not name you: " + err.Error()
	case who.Found:
		if name := firstNonEmpty(who.Agent.SidebarName, who.Agent.Title); name != "" {
			from = name
		}
		return from, ""
	default:
		return from, "could not name you: " + who.Detail
	}
}

func openBrowserTabTool(ctx context.Context, req *mcp.CallToolRequest, in openBrowserTabIn) (*mcp.CallToolResult, browserTabOut, error) {
	if err := requireLocalBrowser(req); err != nil {
		return nil, browserTabOut{}, err
	}
	u, err := normalizeTabURL(in.URL)
	if err != nil {
		return nil, browserTabOut{}, err
	}
	id, err := resolveProfile(in.Profile)
	if err != nil {
		return nil, browserTabOut{}, err
	}
	m, err := browserFor(id)
	if err != nil {
		return nil, browserTabOut{}, err
	}
	m.touch() // opened for someone to look at: don't let the idle stop race it
	wasRunning := m.current() != nil
	p, err := m.ensure(ctx)
	if err != nil {
		return nil, browserTabOut{}, fmt.Errorf("could not start profile %q's browser: %w", id, err)
	}
	// A browser this call just launched has its own about:blank, which would sit
	// in the human's tab strip beside the page they were sent. Its id is taken
	// BEFORE opening, so the new tab (about:blank until it commits) is not it.
	var launchTabs []string
	if !wasRunning {
		if pages, err := browserPages(p); err == nil {
			for _, pg := range pages {
				if pg.URL == "about:blank" {
					launchTabs = append(launchTabs, pg.ID)
				}
			}
		}
	}
	var t struct {
		ID    string `json:"id"`
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	// PUT: GET on /json/new is refused since Chromium 111. The query is the
	// URL itself, unescaped by Chromium.
	if err := devtoolsDo(p, http.MethodPut, "/json/new?"+url.PathEscape(u), &t); err != nil {
		return nil, browserTabOut{}, fmt.Errorf("open %s in profile %q: %w", u, id, err)
	}
	for _, tid := range launchTabs {
		_ = devtoolsDo(p, http.MethodGet, "/json/close/"+tid, nil)
	}
	st, _ := sharedBrowsers.profileStatus(id)
	out := browserTabOut{TabID: t.ID, Profile: id, URL: u, Title: t.Title, MCPEndpoint: profileOut(req, st).MCPEndpoint}
	if in.Show != nil && !*in.Show {
		out.Detail = "opened without showing it (show:false)"
		return nil, out, nil
	}
	from, detail := callerName(ctx, req, in.PaneID)
	out.Delivered = browserOpenBroadcast(browserOpenEvent{Profile: id, TabID: t.ID, URL: u, From: from})
	if out.Delivered == 0 {
		out.Detail = "no lasso tab is open, so nobody saw it (the tab is open in the browser)"
	} else {
		out.Detail = detail
	}
	return nil, out, nil
}

// browserTabID is what a CDP target id looks like; anything else would be a
// path segment lasso has no business putting in a devtools URL.
var browserTabID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// findTab looks a tab up in a RUNNING profile, starting nothing.
func findTab(profileArg, tabID string) (string, *browserProc, browserPage, error) {
	if !browserTabID.MatchString(strings.TrimSpace(tabID)) {
		return "", nil, browserPage{}, fmt.Errorf("tab_id %q is not a tab id (list_browser_tabs shows them)", tabID)
	}
	tabID = strings.TrimSpace(tabID)
	id, err := resolveProfile(profileArg)
	if err != nil {
		return "", nil, browserPage{}, err
	}
	m, err := browserFor(id)
	if err != nil {
		return "", nil, browserPage{}, err
	}
	p := m.current()
	if p == nil {
		return "", nil, browserPage{}, fmt.Errorf("profile %q's browser is not running, so it has no tabs", id)
	}
	pages, err := browserPages(p)
	if err != nil {
		return "", nil, browserPage{}, err
	}
	for _, pg := range pages {
		if pg.ID == tabID {
			return id, p, pg, nil
		}
	}
	return "", nil, browserPage{}, fmt.Errorf("no tab %q in profile %q (list_browser_tabs shows the open ones)", tabID, id)
}

const showBrowserTabDescription = "Put an EXISTING shared-browser tab on the human's screen: every visible lasso tab switches its Browser tab to that profile and tab. Use it to point them at a page you have been working in. `delivered` 0 means no lasso tab is open, so the human did NOT see it."

type showBrowserTabIn struct {
	TabID   string `json:"tab_id" jsonschema:"The tab to show (from list_browser_tabs or open_browser_tab)."`
	Profile string `json:"profile,omitempty" jsonschema:"The tab's profile (id or display name). Omit for the default profile."`
	PaneID  string `json:"pane_id,omitempty" jsonschema:"Your own herdr pane id ($HERDR_PANE_ID), to tell the human who is showing it."`
}

func showBrowserTabTool(ctx context.Context, req *mcp.CallToolRequest, in showBrowserTabIn) (*mcp.CallToolResult, browserTabOut, error) {
	if err := requireLocalBrowser(req); err != nil {
		return nil, browserTabOut{}, err
	}
	id, _, pg, err := findTab(in.Profile, in.TabID)
	if err != nil {
		return nil, browserTabOut{}, err
	}
	from, detail := callerName(ctx, req, in.PaneID)
	out := browserTabOut{TabID: pg.ID, Profile: id, URL: pg.URL, Title: pg.Title}
	out.Delivered = browserOpenBroadcast(browserOpenEvent{Profile: id, TabID: pg.ID, URL: pg.URL, From: from})
	if out.Delivered == 0 {
		out.Detail = "no lasso tab is open, so nobody saw it"
	} else {
		out.Detail = detail
	}
	return nil, out, nil
}

const closeBrowserTabDescription = "Close a tab in lasso's shared browser. Close the tabs you opened when you are done; don't close the human's or another agent's unless asked."

type closeBrowserTabIn struct {
	TabID   string `json:"tab_id" jsonschema:"The tab to close (from list_browser_tabs or open_browser_tab)."`
	Profile string `json:"profile,omitempty" jsonschema:"The tab's profile (id or display name). Omit for the default profile."`
}

type closeBrowserTabOut struct {
	Closed  string `json:"closed"`
	Profile string `json:"profile"`
}

func closeBrowserTabTool(ctx context.Context, req *mcp.CallToolRequest, in closeBrowserTabIn) (*mcp.CallToolResult, closeBrowserTabOut, error) {
	if err := requireLocalBrowser(req); err != nil {
		return nil, closeBrowserTabOut{}, err
	}
	id, p, pg, err := findTab(in.Profile, in.TabID)
	if err != nil {
		return nil, closeBrowserTabOut{}, err
	}
	if err := devtoolsDo(p, http.MethodGet, "/json/close/"+pg.ID, nil); err != nil {
		return nil, closeBrowserTabOut{}, fmt.Errorf("close tab %s: %w", pg.ID, err)
	}
	return nil, closeBrowserTabOut{Closed: pg.ID, Profile: id}, nil
}

// registerBrowserProfileTools adds the profile and tab tools to lasso's MCP
// server.
func registerBrowserProfileTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "list_browser_profiles", Description: listBrowserProfilesDescription}, listBrowserProfilesTool)
	mcp.AddTool(s, &mcp.Tool{Name: "create_browser_profile", Description: createBrowserProfileDescription}, createBrowserProfileTool)
	mcp.AddTool(s, &mcp.Tool{Name: "update_browser_profile", Description: updateBrowserProfileDescription}, updateBrowserProfileTool)
	mcp.AddTool(s, &mcp.Tool{Name: "delete_browser_profile", Description: deleteBrowserProfileDescription}, deleteBrowserProfileTool)
	mcp.AddTool(s, &mcp.Tool{Name: "list_browser_tabs", Description: listBrowserTabsDescription}, listBrowserTabsTool)
	mcp.AddTool(s, &mcp.Tool{Name: "open_browser_tab", Description: openBrowserTabDescription}, openBrowserTabTool)
	mcp.AddTool(s, &mcp.Tool{Name: "show_browser_tab", Description: showBrowserTabDescription}, showBrowserTabTool)
	mcp.AddTool(s, &mcp.Tool{Name: "close_browser_tab", Description: closeBrowserTabDescription}, closeBrowserTabTool)
}
