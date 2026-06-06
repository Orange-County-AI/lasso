package main

import "testing"

// TestAgentKindFromToken locks the argv0→agent mapping (basename match, ignoring
// quotes/flags) that identifies a live agent the way herdr does.
func TestAgentKindFromToken(t *testing.T) {
	cases := []struct {
		token string
		want  string
	}{
		{"claude", "claude"},
		{"/home/stephan/.local/bin/claude", "claude"},
		{"claude-code", "claude"},
		{"codex", "codex"},
		{"/home/linuxbrew/.linuxbrew/bin/codex", "codex"},
		{`"claude"`, "claude"},
		{"bash", ""},
		{"node", ""},
		{"-claude", ""}, // a flag, not a command
		{"", ""},
		{"2.1.167", ""}, // claude's symlink-target basename must NOT match
	}
	for _, c := range cases {
		if got := agentKindFromToken(c.token); got != c.want {
			t.Errorf("agentKindFromToken(%q) = %q, want %q", c.token, got, c.want)
		}
	}
}

// TestSubtreeAgentKind verifies the process-subtree walk finds an agent nested
// below the pane's shell and returns "" for a plain shell subtree.
func TestSubtreeAgentKind(t *testing.T) {
	// shell(100) → claude(101) → child(102); and a bare shell 200 → cat 201.
	procs := map[int]procInfo{
		100: {ppid: 1, kind: ""},
		101: {ppid: 100, kind: "claude"},
		102: {ppid: 101, kind: ""},
		200: {ppid: 1, kind: ""},
		201: {ppid: 200, kind: ""},
	}
	children := map[int][]int{}
	for pid, info := range procs {
		children[info.ppid] = append(children[info.ppid], pid)
	}
	if got := subtreeAgentKind(100, children, procs); got != "claude" {
		t.Errorf("subtreeAgentKind(shell with claude) = %q, want claude", got)
	}
	if got := subtreeAgentKind(200, children, procs); got != "" {
		t.Errorf("subtreeAgentKind(plain shell) = %q, want empty", got)
	}
}
