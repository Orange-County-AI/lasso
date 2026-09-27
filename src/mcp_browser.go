package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// shared_browser: how an agent finds the Chromium a human is watching in
// lasso's Browser tab (browser.go, cdpproxy.go). The tool does not drive the
// browser — CDP clients already do that far better than a tool surface could —
// it starts it and says where to connect.

const sharedBrowserDescription = "Get (and by default start) lasso's SHARED BROWSER: a real Chromium that a human is watching live in lasso's Browser tab, which you can drive over the Chrome DevTools Protocol. Connect with `npx chrome-devtools-mcp@latest --wsEndpoint <ws_endpoint>` or Playwright's `chromium.connectOverCDP(<ws_endpoint>)`. The human sees and can click in the same pages you do. Their Browser tab shows ONE page, whichever was opened most recently, so opening a new page puts them on it (the page they were on keeps running out of sight). Open your own page rather than navigating one you did not open, unless the human asked you to work in theirs, and close the pages you opened when you are done. The browser runs on lasso's machine, not necessarily yours: `localhost` inside it means lasso's machine. `pages` lists the pages open right now. If `available` is false, `note` says why (usually no Chromium installed there)."

type sharedBrowserIn struct {
	Start *bool `json:"start,omitempty" jsonschema:"Start the browser if it is not running (default true). Pass false to only report its state."`
}

type sharedBrowserOut struct {
	Available    bool          `json:"available"`
	Running      bool          `json:"running"`
	WSEndpoint   string        `json:"ws_endpoint"`   // absolute CDP websocket URL, e.g. ws://lasso.example:8190/cdp
	WSPath       string        `json:"ws_path"`       // always /cdp — prefix lasso's own URL when ws_endpoint is empty
	HTTPEndpoint string        `json:"http_endpoint"` // the /cdp HTTP base (…/cdp/json/list, /json/version)
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
	if base := sharedBrowserBase(req); base != "" {
		out.HTTPEndpoint = base + "/cdp"
		out.WSEndpoint = "ws" + strings.TrimPrefix(base, "http") + "/cdp"
	} else {
		notes = append(notes, "lasso could not tell which URL you reached it on: prefix lasso's own URL to ws_path (ws://<lasso-host>/cdp, wss:// behind TLS)")
	}
	if m.cfg.AuthRequired {
		notes = append(notes, `/cdp requires the same credentials as /mcp: send your bearer token, e.g. chrome-devtools-mcp --wsHeaders '{"Authorization":"Bearer <token>"}'`)
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
