package main

// ---------------------------------------------------------------------------
// agent-record projection parity
// ---------------------------------------------------------------------------

// agentInfo is the output-side twin of the create payload's input-side problem
// (see create_params.go). AgentRecord is what lasso persists; agentInfo is what
// get_agent, list_agents and whoami hand back. Nothing tied them together, so a
// field could join the record and never surface — which is exactly what had
// happened to Effort: create_agent grew the parameter and a caller still could
// not read back whether the level took, even though normalizeEffort silently
// DROPS one the harness doesn't offer. "Did my setting apply?" was unanswerable
// without reading the pane.
//
// agentInfoParams is the same contract as createParams, pointed the other way:
// one entry per AgentRecord field saying which agentInfo field carries it, or
// WHY it is deliberately not surfaced. The tests in agentinfo_params_test.go
// fail on any undeclared field.
//
// The line this draws: agentInfo answers "how was this agent configured, and
// how is it doing" — every knob create_agent accepts reads back. It is NOT a
// transport for the free-text bodies (the prompt, the notes), which are
// unbounded per agent and would be multiplied by every list_agents call.

// agentInfoParam records how one AgentRecord field reaches agentInfo. Exactly
// one of Info/Skip must be set; Skip is the recorded reason a field stays
// server-side, so an omission is always an argued decision.
type agentInfoParam struct {
	// Info is the agentInfo field carrying this value.
	Info string
	// Derived, when set, says the value is transformed rather than copied, and
	// why. Derived fields are exempt from the automatic copy check and instead
	// need a named case in the parity test.
	Derived string
	// Skip is why the field is deliberately not surfaced over MCP.
	Skip string
}

var agentInfoParams = map[string]agentInfoParam{
	"ID":          {Info: "ID"},
	"Title":       {Info: "Title"},
	"Type":        {Info: "Type"},
	"Repo":        {Info: "Repo"},
	"BaseBranch":  {Info: "BaseBranch"},
	"Branch":      {Info: "Branch"},
	"Agent":       {Info: "Agent"},
	"Model":       {Info: "Model"},
	"Effort":      {Info: "Effort"},
	"ExtraArgs":   {Info: "ExtraArgs"},
	"PlanMode":    {Info: "PlanMode"},
	"WorkDir":     {Info: "WorkDir"},
	"WorkspaceID": {Info: "WorkspaceID"},
	"RootPane":    {Info: "RootPane"},
	"BootStatus":  {Info: "BootStatus"},
	"BootError":   {Info: "BootError"},
	"Host": {
		// agentInfoFrom takes the host as an argument rather than reading
		// rec.Host: the record may have been loaded from another host's db (the
		// cross-host whoami search), and the answer must name the host the
		// caller can actually address it on.
		Info:    "Host",
		Derived: "passed in by the caller — the host the records were loaded from, not rec.Host",
	},
	"CreatedAt": {
		Info:    "CreatedAt",
		Derived: "time.Time rendered as an RFC3339 string, since MCP output is JSON",
	},
	"Description": {
		// This is the agent's full initial prompt. list_agents returns every
		// agent on a host, so surfacing it would multiply an unbounded body by
		// the agent count on a call whose job is enumeration.
		Skip: "the agent's full prompt — unbounded, and list_agents would repeat it per agent; read it from the pane with read_agent",
	},
	"Notes": {
		Skip: "unbounded free text, same reason as Description; bootAgent writes it to NOTES.md in the work dir",
	},
	"Attachments": {
		Skip: "filenames from the browser upload-staging flow, already moved into the work dir by bootAgent; list the dir instead",
	},
	"ClosedAt": {
		// Every MCP path that builds an agentInfo resolves through listAgents /
		// listAllAgents, which exclude tombstones — so this would be the empty
		// string on every agent a caller can ever see. A field that is
		// unconditionally empty is worse than absent: it reads as "this agent is
		// not closed" on a listing where being closed was already impossible.
		Skip: "always empty in MCP output — a tombstoned record is filtered out of every listing before it can become an agentInfo",
	},
}

// agentInfoComputed are the agentInfo fields that come from somewhere other than
// the persisted record — live herdr state or the shape's dual use for foreign
// sessions. They have no AgentRecord source to be checked against, so each one
// records where it does come from.
var agentInfoComputed = map[string]string{
	"SidebarName":  "herdr's workspace label, read live from the pane (and the only name a foreign session has)",
	"Status":       "live herdr pane status, reconciled with the boot outcome by surfacedStatus",
	"LassoCreated": "false for foreign herdr sessions lasso never created, which agentInfoFromPane builds without a record",
}
