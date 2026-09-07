package main

// runtimePanes removes command-fallback identities before exposing agent state.
// UHP's detector is authoritative; its public contract does not expose OSC titles.
func paneAgentPresence(p pane) (kind, status string) {
	return p.Agent, p.AgentStatus
}

func paneHasLiveAgent(p pane) bool { return p.Agent != "" }
