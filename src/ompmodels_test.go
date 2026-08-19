package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The creator's OMP model list is the host's own role assignments, in config
// order, deduplicated — a role's ":<thinking level>" suffix dropped (lasso has
// its own effort field), a colon that ISN'T a level kept (it's part of the id),
// and the per-task-agent overrides folded in after the named roles.
func TestOmpRoleModels(t *testing.T) {
	cfg := []byte(`theme:
  dark: herdr
modelRoles:
  default: anthropic/claude-opus-5:high
  vision: openai-codex/gpt-5.6-sol
  CYBERSECURITY: zai/glm-5.3:max
  designer: anthropic/claude-opus-5:high
  smol: openai-codex/gpt-5.6-terra:medium
  cheap: openrouter/qwen3:free
retry:
  fallbackChains:
    default:
      - never-suggest/from-a-fallback-chain
task:
  agentModelOverrides:
    scout: openai-codex/gpt-5.6-terra
    security-reviewer: kimi-code/k3
`)
	want := []string{
		// Named roles first, in the order the user wrote them; the duplicate
		// "designer" assignment and smol's already-seen terra collapse away.
		"anthropic/claude-opus-5",
		"openai-codex/gpt-5.6-sol",
		"zai/glm-5.3",
		"openai-codex/gpt-5.6-terra",
		// ":free" is part of the openrouter id, not a thinking level.
		"openrouter/qwen3:free",
		// Then the task-agent overrides — only the one that's new.
		"kimi-code/k3",
	}
	got := ompRoleModels(cfg)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("omp role models = %q, want %q", got, want)
	}
}

// A config lasso can't make sense of must yield nothing, so the caller falls
// back to the compiled-in suggestions instead of serving a half-read list.
func TestOmpRoleModelsRejectsUnusableConfig(t *testing.T) {
	for name, in := range map[string]string{
		"not yaml":        "modelRoles: [unclosed\n",
		"no roles":        "theme:\n  dark: herdr\n",
		"roles not a map": "modelRoles: opus\n",
		"empty":           "",
	} {
		if got := ompRoleModels([]byte(in)); len(got) != 0 {
			t.Errorf("%s: got %q, want nothing", name, got)
		}
	}
}

// hostHarnesses serves the registry with ONLY omp's suggestions swapped for the
// host's role models — and leaves the compiled-in table untouched, since it's
// shared global state every other host's response is built from.
func TestHostHarnessesUsesOmpRoleModels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := &localBackend{}

	// A host where omp has never run keeps the compiled-in list.
	if got := hostHarnesses(b); &got[0] != &harnesses[0] {
		t.Errorf("no omp config must serve the compiled-in registry unchanged")
	}

	dir := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "modelRoles:\n  default: zai/glm-5.3:max\n  smol: kimi-code/k3\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	out := hostHarnesses(b)
	if len(out) != len(harnesses) {
		t.Fatalf("registry length changed: %d, want %d", len(out), len(harnesses))
	}
	var omp *harnessDef
	for i := range out {
		if out[i].ID == "omp" {
			omp = &out[i]
		}
	}
	if omp == nil {
		t.Fatal("omp missing from the served registry")
	}
	if strings.Join(omp.ModelSuggestions, ",") != "zai/glm-5.3,kimi-code/k3" {
		t.Errorf("omp suggestions = %q, want the host's role models", omp.ModelSuggestions)
	}
	// Every other harness — and the shared registry itself — is untouched.
	for i := range out {
		if out[i].ID == "omp" {
			continue
		}
		if strings.Join(out[i].ModelSuggestions, ",") !=
			strings.Join(harnesses[i].ModelSuggestions, ",") {
			t.Errorf("%s suggestions changed: %q", out[i].ID, out[i].ModelSuggestions)
		}
	}
	if got := harnessByID("omp").ModelSuggestions; strings.Contains(strings.Join(got, ","), "glm") {
		t.Errorf("the compiled-in registry was mutated: %q", got)
	}
}
