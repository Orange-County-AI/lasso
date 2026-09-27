package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// shared_browser: how an agent finds the Chromium a human is watching in
// lasso's Browser tab (browser.go, cdpproxy.go). The tool does not drive the
// browser — /browser-mcp (browsermcp.go) serves chrome-devtools-mcp's tools for
// that, and raw CDP clients connect to /cdp — it starts it and says where to
// connect. The MCP URL leads, because it is the one that needs nothing
// installed on the agent's machine.

const sharedBrowserDescription = "To drive the browser, add lasso's browser MCP server: `mcp_endpoint` is a streamable-HTTP MCP URL serving chrome-devtools-mcp's tools, already pointed at lasso's SHARED BROWSER (e.g. `claude mcp add --transport http lasso-browser <mcp_endpoint>`; other agents add it as a streamable-HTTP MCP server) — if you cannot add MCP servers yourself, ask the human to. The shared browser is a real Chromium that a human is watching live in lasso's Browser tab; this tool gets (and by default starts) it and returns where to connect. For Playwright or raw CDP instead, `ws_endpoint` is the Chrome DevTools Protocol websocket (`chromium.connectOverCDP(<ws_endpoint>)`). The human sees and can click in the same pages you do. Their Browser tab shows ONE page, whichever was opened most recently, so opening a new page puts them on it (the page they were on keeps running out of sight). Open your own page rather than navigating one you did not open, unless the human asked you to work in theirs, and close the pages you opened when you are done. The browser runs on lasso's machine, not necessarily yours: `localhost` inside it means lasso's machine. `pages` lists the pages open right now. If `available` is false, `note` says why (usually no Chromium installed there); if `mcp_available` is false, `mcp_reason` says why the MCP URL cannot serve (usually chrome-devtools-mcp not installed on lasso's machine)."

type sharedBrowserIn struct {
	Start *bool `json:"start,omitempty" jsonschema:"Start the browser if it is not running (default true). Pass false to only report its state."`
}

type sharedBrowserOut struct {
	Available    bool          `json:"available"`
	Running      bool          `json:"running"`
	WSEndpoint   string        `json:"ws_endpoint"`   // absolute CDP websocket URL, e.g. ws://lasso.example:8190/cdp
	WSPath       string        `json:"ws_path"`       // always /cdp — prefix lasso's own URL when ws_endpoint is empty
	HTTPEndpoint string        `json:"http_endpoint"` // the /cdp HTTP base (…/cdp/json/list, /json/version)
	MCPEndpoint  string        `json:"mcp_endpoint"`  // absolute streamable-HTTP MCP URL, e.g. https://lasso.example/browser-mcp
	MCPAvailable bool          `json:"mcp_available"` // chrome-devtools-mcp is installed on lasso's machine and the endpoint is on
	MCPReason    string        `json:"mcp_reason,omitempty"`
	Pages        []browserPage `json:"pages"`
	Note         string        `json:"note,omitempty"`
}

func sharedBrowserTool(ctx context.Context, req *mcp.CallToolRequest, in sharedBrowserIn) (*mcp.CallToolResult, sharedBrowserOut, error) {
	cs := callerFrom(req)
	// The browser is on lasso's machine. A credential confined to some other
	// host has no business driving pages from there — it would reach whatever
	// lasso's machine can, which is exactly the boundary its scope draws.
	if !cs.allows("local") {
		return nil, sharedBrowserOut{}, fmt.Errorf("the shared browser runs on lasso's own machine, which is outside this credential's reach (%s)", cs.reachSummary())
	}
	m := sharedBrowser
	out := sharedBrowserOut{WSPath: "/cdp", Pages: []browserPage{}}
	var notes []string
	if m == nil {
		out.Note = "the shared browser is not configured on this lasso"
		return nil, out, nil
	}
	start := in.Start == nil || *in.Start
	if start {
		m.touch() // an agent about to connect: don't let the idle stop race it
		if _, err := m.ensure(ctx); err != nil {
			notes = append(notes, "could not start: "+err.Error())
		}
	}
	st := m.status()
	out.Available, out.Running, out.Pages = st.Available, st.Running, st.Pages
	if !st.Available || (!st.Running && st.Reason != "" && start) {
		if st.Reason != "" && !strings.Contains(strings.Join(notes, " "), st.Reason) {
			notes = append(notes, st.Reason)
		}
	}
	if !st.Running && !start {
		notes = append(notes, "not running; call with start:true (or just connect to /cdp, which starts it)")
	}
	_, out.MCPReason, out.MCPAvailable = browserMCP.resolve()
	if base := sharedBrowserBase(req); base != "" {
		out.HTTPEndpoint = base + "/cdp"
		out.WSEndpoint = "ws" + strings.TrimPrefix(base, "http") + "/cdp"
		out.MCPEndpoint = base + "/browser-mcp"
	} else {
		notes = append(notes, "lasso could not tell which URL you reached it on: prefix lasso's own URL to ws_path (ws://<lasso-host>/cdp, wss:// behind TLS) or to /browser-mcp")
	}
	if m.cfg.AuthRequired {
		notes = append(notes, "/browser-mcp and /cdp require the same credentials as /mcp: send your Authorization header (bearer token, or UI_AUTH basic) with the MCP server config or the websocket")
	}
	out.Note = strings.Join(notes, "; ")
	return nil, out, nil
}

// sharedBrowserBase is the http(s)://host the MCP request reached lasso on, as
// withRequestBase recorded it; "" when the request did not come through it
// (a test server, a transport with no HTTP request).
func sharedBrowserBase(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	b := strings.TrimRight(req.Extra.Header.Get(lassoBaseHeader), "/")
	if !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
		return ""
	}
	return b
}
