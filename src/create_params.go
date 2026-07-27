package main

// ---------------------------------------------------------------------------
// create-payload parameter parity
// ---------------------------------------------------------------------------

// createAgentReq — the body of POST /api/create-agent, which the web creator
// form posts — is mirrored by two hand-written copies: createAgentIn (the MCP
// create_agent tool's input, copied field-by-field into a createAgentReq) and
// CreateAgentPayload in src/web/src/lib/api.ts. Nothing tied the three
// together, so a field could be added to one and silently missed by the
// others. That is exactly what happened to Effort: the creator grew a
// "Thinking effort" select, createAgentReq and the agents table grew the
// column, and the MCP tool kept launching every agent at the CLI default — no
// error, no hint, an agent that asked for xhigh reasoning quietly getting none.
//
// createParams closes that hole. One entry per createAgentReq field says how
// each copy exposes it — or, for the fields deliberately left out, WHY. The
// tests in create_params_test.go reflect over createAgentReq and fail on any
// field missing from this map, so adding a field without making that decision
// breaks the test suite instead of shipping a silent drop.
//
// Why a registry rather than the obvious "one struct": putting jsonschema tags
// on createAgentReq and generating the MCP schema straight from it looks
// tidier, but the MCP surface is not the HTTP body — it differs in three ways
// a merged struct would have to carry anyway.
//   - Prose. MCP descriptions are written for a tool-calling model and
//     reference the rest of the tool surface ("Use list_branches to choose
//     one"), which is meaningless to the browser form.
//   - Optionality. The SDK derives "required" from `omitempty`, a JSON-encoding
//     concern the request body — which is only ever decoded — has no opinion on.
//   - Polarity. MCP exposes a positive `focus`; the body carries a negative
//     `no_focus` (see the NoFocus entry).
//
// Merging would push MCP-caller prose onto the type the browser posts to and
// STILL need a per-field override table for the rest — this table, minus the
// separation. So the copies stay and the map makes an *undeclared* field
// impossible, which is the property that actually failed here.

// createParam records how one createAgentReq field reaches the MCP tool and the
// browser's TypeScript payload. Exactly one of MCP/MCPSkip must be set, and
// exactly one of TS/TSSkip: the *Skip strings are the recorded reason a copy
// leaves the field out, and are required so "not exposed" is always an argued
// decision rather than an oversight that looks like one.
type createParam struct {
	// MCP is the createAgentIn field carrying this value.
	MCP string
	// MCPDerived, when set, says the value is transformed on the way in rather
	// than copied field-for-field, and why. Derived fields are exempt from the
	// automatic copy check and instead require a named case in the parity test,
	// so the transform itself is covered rather than waved through.
	MCPDerived string
	// MCPSkip is why the field is deliberately absent from the MCP tool.
	MCPSkip string
	// TS is the CreateAgentPayload property in src/web/src/lib/api.ts.
	TS string
	// TSSkip is why the browser payload leaves the field out.
	TSSkip string
}

var createParams = map[string]createParam{
	"Host": {
		// Not copied into the req: the MCP tool resolves the host to a Backend
		// itself and hands createAgent that backend, which takes the host from
		// b.Name(). (serveCreateAgent reads req.Host for the same purpose, then
		// passes the resolved backend the same way.)
		MCP:        "Host",
		MCPDerived: "consumed by createAgentTool to pick the backend, not copied into the req",
		TS:         "host",
	},
	"Type":         {MCP: "Type", TS: "type"},
	"Prompt":       {MCP: "Prompt", TS: "prompt"},
	"Repo":         {MCP: "Repo", TS: "repo"},
	"BaseBranch":   {MCP: "BaseBranch", TS: "base_branch"},
	"BranchPrefix": {MCP: "BranchPrefix", TS: "branch_prefix"},
	"BranchName":   {MCP: "BranchName", TS: "branch_name"},
	"Agent":        {MCP: "Agent", TS: "agent"},
	"Model":        {MCP: "Model", TS: "model"},
	"Effort":       {MCP: "Effort", TS: "effort"},
	"ExtraArgs":    {MCP: "ExtraArgs", TS: "extra_args"},
	"Notes":        {MCP: "Notes", TS: "notes"},
	"Title": {
		MCP: "Title",
		// The creator has no title field — it always derives the title from the
		// prompt's first line. MCP keeps the override because a caller composing
		// a long machine-written prompt has no useful first line to fall back on.
		TSSkip: "the creator form derives the title from the prompt's first line and offers no override",
	},
	"PlanMode": {
		MCPSkip: "agents started via MCP never run in plan mode",
		TS:      "plan_mode",
	},
	"Attachments": {
		MCPSkip: "part of the browser upload-staging flow (/api/agent-upload), which has no MCP equivalent",
		TS:      "attachments",
	},
	"UploadDir": {
		MCPSkip: "staging dir returned by /api/agent-upload; browser-only, like Attachments",
		TS:      "upload_dir",
	},
	"NoFocus": {
		// Polarity flip, deliberately: the field defaults to "do focus", which is
		// what the web "New Agent" flow wants (it's an explicit "take me there"),
		// while an MCP spawn must NOT yank a watching user away from their pane.
		// So MCP exposes a positive `focus` defaulting false and inverts it.
		MCP:        "Focus",
		MCPDerived: "inverted — MCP exposes a positive `focus` defaulting false, so an MCP spawn doesn't steal the user's pane",
		TSSkip:     "the web New Agent flow is an explicit \"take me there\", so the form leaves it false and never sends it",
	},
}
