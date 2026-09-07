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

func TestClaudeSessionID(t *testing.T) {
	live := pane{Agent: "claude", AgentSession: "24a7c912-71da"}
	if id := claudeSessionID(live); id != "24a7c912-71da" {
		t.Errorf("claudeSessionID(live) = %q, want the id", id)
	}

	// Luvus keeps agent_session around for resume after the agent exits; once
	// the shell is back, the pane's own cwd is the truth again.
	exited := live
	exited.Agent = ""
	if id := claudeSessionID(exited); id != "" {
		t.Errorf("claudeSessionID(exited agent) = %q, want empty", id)
	}

	// A session id must never be able to escape the projects dir.
	evil := live
	evil.AgentSession = "../../etc/passwd"
	if id := claudeSessionID(evil); id != "" {
		t.Errorf("claudeSessionID with a path-traversing id = %q, want \"\"", id)
	}

	// The session belongs to whatever agent the pane is running; only claude's
	// transcripts are parseable, so another harness's session resolves to
	// nothing.
	other := live
	other.Agent = "codex"
	if id := claudeSessionID(other); id != "" {
		t.Errorf("claudeSessionID(codex) = %q, want empty — only claude's transcripts are read", id)
	}

	if id := claudeSessionID(pane{Agent: "claude"}); id != "" {
		t.Errorf("claudeSessionID without a session = %q, want empty", id)
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

// agentPane is a pane running claude, as Luvus reports it: the pane cwd sits at
// the dir claude was launched in, whatever the agent has cd'd to since.
func agentPane(launchDir, id string) pane {
	return pane{
		PaneID: "1", Cwd: launchDir, Agent: "claude", AgentSession: id,
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
	if got := harnessCwd(b, p); got != work {
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
	if got := harnessCwd(b, p); got != work {
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
	if got := harnessCwd(b, p); got != first {
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
	if got := harnessCwd(b, p); got != second {
		t.Errorf("harnessCwd after the agent cd'd = %q, want %q", got, second)
	}
}

func TestHarnessCwdIgnoresADirectoryThatIsGone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTranscript(t, home, home, "sess-d", filepath.Join(home, "deleted-worktree"))

	b := &localBackend{}
	if got := harnessCwd(b, agentPane(home, "sess-d")); got != "" {
		t.Errorf("harnessCwd = %q, want \"\" so the viewer falls back to the pane's cwd", got)
	}
}

func TestHarnessCwdOfAPlainShellIsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := &localBackend{}
	p := pane{PaneID: "2", Cwd: home}
	if got := harnessCwd(b, p); got != "" {
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
