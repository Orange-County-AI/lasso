package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Agent presence is determined the way herdr does it: by inspecting the live
// foreground process of each tmux pane, NOT by a stored flag. A pane counts as
// "an agent" only while an agent binary (claude/codex) is actually running under
// its shell. Two reasons this can't lean on tmux's #{pane_current_command}:
//   - the native claude/codex binaries have unhelpful comm names (claude's exe
//     is a version-numbered file like "2.1.167"; codex's is "codex-x86_64-…"),
//     so the foreground comm doesn't identify them — we match argv0 from
//     /proc/<pid>/cmdline instead, mirroring herdr's argv-based identify_agent;
//   - after the agent exits (or a fresh post-reboot shell), the pane is just a
//     shell and must stop counting as an agent.
//
// Linux-only (/proc); lasso is local-only for now (multi-host is deferred).

// agentKindFromToken maps a single command/path token to "claude"|"codex"|"",
// by its basename — the same rule herdr uses for argv0/path tokens.
func agentKindFromToken(token string) string {
	token = strings.Trim(token, "\"'")
	if token == "" || strings.HasPrefix(token, "-") {
		return ""
	}
	switch strings.ToLower(filepath.Base(token)) {
	case "claude", "claude-code":
		return "claude"
	case "codex":
		return "codex"
	}
	return ""
}

type procInfo struct {
	ppid int
	kind string // agent kind from cmdline, or ""
}

// scanProcs reads /proc once, returning pid → {ppid, agentKind}.
func scanProcs() map[int]procInfo {
	out := map[int]procInfo{}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid dir
		}
		out[pid] = procInfo{ppid: procPPID(pid), kind: procAgentKind(pid)}
	}
	return out
}

// procPPID reads the parent pid from /proc/<pid>/stat (field 4). The comm field
// (2) can contain spaces and parens, so we split after the LAST ')'.
func procPPID(pid int) int {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	s := string(b)
	rp := strings.LastIndexByte(s, ')')
	if rp < 0 || rp+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[rp+2:]) // after ") " → [state, ppid, …]
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// procAgentKind reads /proc/<pid>/cmdline and returns the agent kind from argv0,
// falling back to scanning later argv tokens when argv0 is a generic runtime
// (node/bun launching an agent's JS entrypoint).
func procAgentKind(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || len(b) == 0 {
		return ""
	}
	args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	if len(args) == 0 || args[0] == "" {
		return ""
	}
	if k := agentKindFromToken(args[0]); k != "" {
		return k
	}
	switch strings.ToLower(filepath.Base(args[0])) {
	case "node", "bun", "deno":
		for _, a := range args[1:] {
			if k := agentKindFromToken(a); k != "" {
				return k
			}
		}
	}
	return ""
}

// tabAgentKinds maps live tab id → agent kind for every lasso tmux pane that
// currently has an agent process running. One tmux call + one /proc scan.
// session "lasso_<id>" → tab "<id>".
func tabAgentKinds() map[string]string {
	out := map[string]string{}
	raw, err := tmuxOut("list-panes", "-a", "-F", "#{session_name}\t#{pane_pid}")
	if err != nil {
		return out
	}
	procs := scanProcs()
	children := map[int][]int{}
	for pid, info := range procs {
		children[info.ppid] = append(children[info.ppid], pid)
	}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		session := parts[0]
		if !strings.HasPrefix(session, "lasso_") {
			continue
		}
		panePid, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		if k := subtreeAgentKind(panePid, children, procs); k != "" {
			out[strings.TrimPrefix(session, "lasso_")] = k
		}
	}
	return out
}

// sessionAgentKind returns the agent kind currently running in one tmux session
// ("" for a plain shell). Same /proc inspection as tabAgentKinds, scoped to one
// pane — used by the on-demand callers (MCP status, kill-agent wait).
func sessionAgentKind(session string) string {
	out, err := tmuxOut("display-message", "-p", "-t", session, "#{pane_pid}")
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return ""
	}
	procs := scanProcs()
	children := map[int][]int{}
	for p, info := range procs {
		children[info.ppid] = append(children[info.ppid], p)
	}
	return subtreeAgentKind(pid, children, procs)
}

// subtreeAgentKind BFS-walks the process subtree under root (the pane's shell)
// and returns the first agent kind among its descendants.
func subtreeAgentKind(root int, children map[int][]int, procs map[int]procInfo) string {
	queue := append([]int(nil), children[root]...)
	for i := 0; i < len(queue); i++ {
		pid := queue[i]
		if k := procs[pid].kind; k != "" {
			return k
		}
		queue = append(queue, children[pid]...)
	}
	return ""
}
