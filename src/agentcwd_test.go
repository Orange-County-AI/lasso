package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeProjectSlug(t *testing.T) {
	cases := map[string]string{
		"/home/stephan":                          "-home-stephan",
		"/home/stephan/.lasso/worktrees/lasso/x": "-home-stephan--lasso-worktrees-lasso-x",
		"/home/stephan/projects/ocai/Recap":      "-home-stephan-projects-ocai-Recap",
		"/home/u/my_proj":                        "-home-u-my-proj",
	}
	for dir, want := range cases {
		if got := claudeProjectSlug(dir); got != want {
			t.Errorf("claudeProjectSlug(%q) = %q, want %q", dir, got, want)
		}
	}
}

func TestLastTranscriptCwd(t *testing.T) {
	// A tail read starts mid-line, and plenty of entries carry no cwd.
	data := []byte(strings.Join([]string{
		`ol result","cwd":"/old/fragment"}`,
		`{"type":"user","cwd":"/home/u"}`,
		`{"type":"summary"}`,
		`not json at all`,
		`{"type":"assistant","cwd":"/home/u/projects/k8s"}`,
		`{"type":"system"}`,
		``,
	}, "\n"))
	if got := lastTranscriptCwd(data); got != "/home/u/projects/k8s" {
		t.Errorf("lastTranscriptCwd = %q, want the newest entry's cwd", got)
	}
	if got := lastTranscriptCwd([]byte("{\"type\":\"x\"}\n")); got != "" {
		t.Errorf("lastTranscriptCwd with no cwd = %q, want \"\"", got)
	}
	if got := lastTranscriptCwd([]byte(`{"cwd":"relative/path"}`)); got != "" {
		t.Errorf("lastTranscriptCwd with a relative cwd = %q, want \"\"", got)
	}
}

func TestLeaderCwd(t *testing.T) {
	// The leader is claude; the second process is one of its Bash-tool shells,
	// which is exactly the drift we must not follow.
	res := json.RawMessage(`{"process_info":{"foreground_process_group_id":42,
		"foreground_processes":[
			{"pid":99,"name":"bash","cwd":"/home/u/.claude/plugins/cache"},
			{"pid":42,"name":"claude","cwd":"/home/u/projects/app"}]}}`)
	if got := leaderCwd(parsePaneProcessInfo(res)); got != "/home/u/projects/app" {
		t.Errorf("leaderCwd = %q, want the leader's cwd", got)
	}

	// No leader in the list (its cwd was unreadable) -> no answer.
	res = json.RawMessage(`{"process_info":{"foreground_process_group_id":42,
		"foreground_processes":[{"pid":99,"name":"bash","cwd":"/tmp"}]}}`)
	if got := leaderCwd(parsePaneProcessInfo(res)); got != "" {
		t.Errorf("leaderCwd without the leader = %q, want \"\"", got)
	}

	// herdr too old for pane.process_info / no foreground job.
	if got := leaderCwd(parsePaneProcessInfo(json.RawMessage(`{}`))); got != "" {
		t.Errorf("leaderCwd of an empty result = %q, want \"\"", got)
	}
}

func TestClaudeSessionRef(t *testing.T) {
	live := pane{Agent: "claude", AgentSession: &agentSession{Agent: "claude", Kind: "id", Value: "24a7c912-71da"}}
	if id, path := claudeSessionRef(live); id != "24a7c912-71da" || path != "" {
		t.Errorf("claudeSessionRef(live) = (%q, %q), want the id", id, path)
	}

	// herdr keeps agent_session around for resume after the agent exits; once
	// the shell is back, the pane's own cwd is the truth again.
	exited := live
	exited.Agent = ""
	if id, path := claudeSessionRef(exited); id != "" || path != "" {
		t.Errorf("claudeSessionRef(exited agent) = (%q, %q), want empty", id, path)
	}

	// A session id must never be able to escape the projects dir.
	evil := live
	evil.AgentSession = &agentSession{Agent: "claude", Kind: "id", Value: "../../etc/passwd"}
	if id, _ := claudeSessionRef(evil); id != "" {
		t.Errorf("claudeSessionRef with a path-traversing id = %q, want \"\"", id)
	}

	other := live
	other.AgentSession = &agentSession{Agent: "codex", Kind: "id", Value: "abc"}
	if id, path := claudeSessionRef(other); id != "" || path != "" {
		t.Errorf("claudeSessionRef(codex) = (%q, %q), want empty — only claude's format is parsed", id, path)
	}

	if id, path := claudeSessionRef(pane{Agent: "claude"}); id != "" || path != "" {
		t.Errorf("claudeSessionRef without a session = (%q, %q), want empty", id, path)
	}
}

// writeTranscript writes a claude session transcript under home for launchDir's
// project slug and returns its path.
func writeTranscript(t *testing.T, home, launchDir, id string, cwds ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", claudeProjectSlug(launchDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, cwd := range cwds {
		sb.WriteString(`{"type":"assistant","cwd":` + jsonStr(cwd) + "}\n")
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// agentPane is a pane running claude, as herdr reports it: both cwds sit at the
// dir claude was launched in, whatever the agent has cd'd to since.
func agentPane(launchDir, id string) pane {
	return pane{
		PaneID: "w1:p1", Cwd: launchDir, ForegroundCwd: launchDir, Agent: "claude",
		AgentSession: &agentSession{Source: "herdr:claude", Agent: "claude", Kind: "id", Value: id},
	}
}

func TestHarnessCwdFollowsTheAgentNotThePane(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := filepath.Join(home, "projects", "k8s")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, home, "sess-a", home, work)

	b := &localBackend{}
	p := agentPane(home, "sess-a")
	if got := harnessCwd(b, p, home); got != work {
		t.Errorf("harnessCwd = %q, want the agent's cwd %q", got, work)
	}
}

func TestHarnessCwdScansProjectDirsWhenThePaneCwdIsNotTheLaunchDir(t *testing.T) {
	// `cd repo && claude`: the shell reported ~ before the cd, so the pane's cwd
	// names no project dir and the id has to be found by scanning.
	home := t.TempDir()
	t.Setenv("HOME", home)
	launch := filepath.Join(home, "repo")
	work := filepath.Join(launch, "sub")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, launch, "sess-b", work)

	b := &localBackend{}
	p := agentPane(home, "sess-b")
	if got := harnessCwd(b, p, ""); got != work {
		t.Errorf("harnessCwd = %q, want %q found by scanning the project dirs", got, work)
	}
}

func TestHarnessCwdRereadsWhenTheTranscriptGrows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	first := filepath.Join(home, "one")
	second := filepath.Join(home, "two")
	for _, d := range []string{first, second} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := writeTranscript(t, home, home, "sess-c", first)

	b := &localBackend{}
	p := agentPane(home, "sess-c")
	if got := harnessCwd(b, p, home); got != first {
		t.Fatalf("harnessCwd = %q, want %q", got, first)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","cwd":"` + second + "\"}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	expireHarnessCwdCache()
	if got := harnessCwd(b, p, home); got != second {
		t.Errorf("harnessCwd after the agent cd'd = %q, want %q", got, second)
	}
}

func TestHarnessCwdIgnoresADirectoryThatIsGone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTranscript(t, home, home, "sess-d", filepath.Join(home, "deleted-worktree"))

	b := &localBackend{}
	if got := harnessCwd(b, agentPane(home, "sess-d"), home); got != "" {
		t.Errorf("harnessCwd = %q, want \"\" so the viewer falls back to the pane's cwd", got)
	}
}

func TestHarnessCwdOfAPlainShellIsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := &localBackend{}
	p := pane{PaneID: "w1:p2", Cwd: home, ForegroundCwd: home}
	if got := harnessCwd(b, p, home); got != "" {
		t.Errorf("harnessCwd of a shell pane = %q, want \"\"", got)
	}
}

// expireHarnessCwdCache ages every cache entry past its TTL, so a test can
// observe the next resolution without sleeping.
func expireHarnessCwdCache() {
	harnessCwdCache.Lock()
	defer harnessCwdCache.Unlock()
	for k := range harnessCwdCache.m {
		harnessCwdCache.m[k].at = harnessCwdCache.m[k].at.Add(-time.Hour)
	}
}
