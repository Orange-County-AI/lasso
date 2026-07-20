package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// nastyPrompt builds a multi-KB markdown prompt with every class of character
// that broke typed delivery: newlines, backticks, $VARs, command substitution,
// apostrophes, and double quotes — the shape of the real bug report that
// reproduced the launch failure.
func nastyPrompt() string {
	para := "# Bug: the caller's pane broke\n\n" +
		"Run `whoami` and `close_agent`, then check `$HERDR_PANE_ID`.\n" +
		"Beware $(rm -rf /tmp/nope), \"double quotes\", 'single quotes',\n" +
		"backslashes \\ and % signs.\n\n"
	return strings.Repeat(para, 30) // ~4KB, like the observed failure
}

// A short single-line prompt must keep riding the typed launch line inline —
// the pre-existing, known-good path — while newlines or size force the staged
// file. The size check looks at the whole built command, so a modest prompt on
// top of long flags still trips it.
func TestNeedsPromptFile(t *testing.T) {
	short := "fix the login bug"
	if needsPromptFile(short, agentCommand("claude", launchOpts{prompt: short})) {
		t.Errorf("short single-line prompt must not need a file")
	}
	multi := "fix the login bug\nthen run the tests"
	if !needsPromptFile(multi, agentCommand("claude", launchOpts{prompt: multi})) {
		t.Errorf("multi-line prompt must need a file")
	}
	cr := "fix the login bug\rplease"
	if !needsPromptFile(cr, agentCommand("claude", launchOpts{prompt: cr})) {
		t.Errorf("carriage-return prompt must need a file")
	}
	long := strings.Repeat("all work and no play makes jack a dull agent ", 20)
	if !needsPromptFile(long, agentCommand("claude", launchOpts{prompt: long})) {
		t.Errorf("oversized command must need a file")
	}
}

// A short prompt's command must be byte-identical to the historical inline
// form (no regression on the common path), and a staged prompt must yield a
// short, single-line command that references the file instead of carrying the
// prompt text.
func TestPromptFileCommandShape(t *testing.T) {
	inline := agentCommand("claude", launchOpts{prompt: "do it"})
	if !strings.HasSuffix(inline, " 'do it'") || strings.Contains(inline, "$(cat") {
		t.Errorf("inline prompt path regressed: %q", inline)
	}

	cmd := agentCommand("claude", launchOpts{model: "opus", promptFile: "/home/u/.lasso/prompts/a1.md"})
	if !strings.HasSuffix(cmd, ` "$(cat '/home/u/.lasso/prompts/a1.md')"`) {
		t.Errorf("claude promptFile command must end with the $(cat …) operand: %q", cmd)
	}
	if strings.Contains(cmd, "\n") || len(cmd) > maxTypedLaunch {
		t.Errorf("promptFile command must stay short and single-line: %q", cmd)
	}

	oc := agentCommand("opencode", launchOpts{promptFile: "/home/u/.lasso/prompts/a1.md"})
	if !strings.HasSuffix(oc, ` --prompt "$(cat '/home/u/.lasso/prompts/a1.md')"`) {
		t.Errorf("opencode promptFile command must ride in via --prompt: %q", oc)
	}
}

// The reproduction the fix is proved against: stage a multi-KB prompt full of
// newlines, backticks, $VARs, apostrophes and quotes, then actually run the
// built launch command through a real shell (sh, and zsh when present — the
// pane shell in production) with a stub `claude` that records its argv. The
// prompt must arrive intact, byte-for-byte, as the single final positional
// argument.
func TestStagedPromptDeliveredAsSingleArgument(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	prompt := nastyPrompt()

	path, err := stageAgentPrompt(&localBackend{}, "repro1", prompt)
	if err != nil {
		t.Fatalf("stageAgentPrompt: %v", err)
	}
	cmd := agentCommand("claude", launchOpts{model: "claude-fable-5", promptFile: path})
	if strings.ContainsAny(cmd, "\n\r") {
		t.Fatalf("typed command must be single-line: %q", cmd)
	}
	if len(cmd) > maxTypedLaunch {
		t.Fatalf("typed command len = %d, must be <= %d: %q", len(cmd), maxTypedLaunch, cmd)
	}

	// Stub claude: writes each argv element NUL-terminated so the test can
	// verify argument boundaries exactly.
	stubDir := t.TempDir()
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done > \"$ARGV_OUT\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, shell := range []string{"sh", "zsh"} {
		if _, err := exec.LookPath(shell); err != nil {
			continue // zsh may be absent on CI runners
		}
		t.Run(shell, func(t *testing.T) {
			argvOut := filepath.Join(t.TempDir(), "argv")
			c := exec.Command(shell, "-c", cmd)
			c.Env = append(os.Environ(),
				"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"ARGV_OUT="+argvOut,
			)
			var stderr bytes.Buffer
			c.Stderr = &stderr
			if err := c.Run(); err != nil {
				t.Fatalf("%s -c launch command failed: %v (stderr: %s)", shell, err, stderr.String())
			}
			raw, err := os.ReadFile(argvOut)
			if err != nil {
				t.Fatalf("stub never ran: %v", err)
			}
			args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
			// Command substitution strips trailing newlines — the one byte-level
			// delta this delivery has, and one that carries no meaning in a
			// prompt. Everything else must arrive exactly.
			want := []string{"--dangerously-skip-permissions", "--model", "claude-fable-5", strings.TrimRight(prompt, "\n")}
			if len(args) != len(want) {
				t.Fatalf("claude got %d args, want %d (prompt split across arguments?)", len(args), len(want))
			}
			for i := range want {
				if args[i] != want[i] {
					t.Fatalf("arg %d = %q, want %q", i, args[i], want[i])
				}
			}
		})
	}
}

// promptBootFake backs the bootAgent-level test: pane reads return the trust
// dialog (stable text, so waitPaneReady settles fast and confirmAgentTrust
// fires instead of polling out its 30s window) and every pane.send_text
// payload is captured for assertions on what actually got typed.
type promptBootFake struct {
	*memBackend
	mu    sync.Mutex
	sends []string
}

func (b *promptBootFake) HerdrCall(method string, params any) (json.RawMessage, error) {
	switch method {
	case "pane.read":
		return json.RawMessage(`{"read":{"text":"trust this folder"}}`), nil
	case "pane.send_text":
		if p, ok := params.(map[string]any); ok {
			if txt, ok := p["text"].(string); ok {
				b.mu.Lock()
				b.sends = append(b.sends, txt)
				b.mu.Unlock()
			}
		}
	}
	return json.RawMessage(`{}`), nil
}

func (b *promptBootFake) GitOut(string, ...string) (string, error) { return "", nil }

// End-to-end through bootAgent: a long multi-line prompt must be staged to the
// host's prompts dir (never typed into the pane), the typed launch line must
// stay short and single-line, and closing the agent must remove the staged
// file.
func TestBootAgentStagesLongPromptAndCloseCleansUp(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if db != nil {
			db.Close()
			db = nil
		}
	})

	b := &promptBootFake{memBackend: newMemBackend()}
	rec := AgentRecord{
		ID:          "promptboot1",
		Host:        "local",
		Type:        "scratch",
		Agent:       "claude",
		Title:       "Long prompt",
		Description: nastyPrompt(),
		WorkDir:     "/work",
		RootPane:    "p1",
	}
	bootAgent(b, "local", rec, "")

	b.mu.Lock()
	sends := append([]string(nil), b.sends...)
	b.mu.Unlock()
	if len(sends) == 0 {
		t.Fatal("bootAgent never sent the launch command")
	}
	launch := sends[0]
	if !strings.HasSuffix(launch, "\n") {
		t.Errorf("launch line must end with Enter: %q", launch)
	}
	body := strings.TrimSuffix(launch, "\n")
	if strings.ContainsAny(body, "\n\r") {
		t.Errorf("typed launch command must be single-line: %q", body)
	}
	if len(body) > maxTypedLaunch {
		t.Errorf("typed launch command len = %d, want <= %d: %q", len(body), maxTypedLaunch, body)
	}
	if !strings.Contains(body, `"$(cat `) {
		t.Errorf("launch command must expand the staged prompt file: %q", body)
	}

	path := agentPromptPath(b, rec.ID)
	if got, want := b.files[path], agentPrompt(rec); got != want {
		t.Errorf("staged prompt file differs from the prompt:\n got: %q\nwant: %q", got, want)
	}

	// Closing the agent removes the staged file. RootPane is cleared so the
	// fake isn't asked to simulate a kill sequence — the cleanup under test
	// runs regardless.
	closed := rec
	closed.RootPane = ""
	if _, err := closeAgentRecord(b, closed, false, false); err != nil {
		t.Fatalf("closeAgentRecord: %v", err)
	}
	if _, ok := b.files[path]; ok {
		t.Errorf("staged prompt file must be removed on close: %s", path)
	}
}
