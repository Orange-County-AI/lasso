package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain fences the whole test binary off from the machine it runs on.
//
// Tests are usually run from inside a herdr pane on the real box, so the
// process inherits the human's HOME and the live HERDR_SOCKET_PATH. Anything
// not sandboxed per test — and anything that outlives a test's t.Setenv, like
// the backdrop re-sync timer that fires 1.5s after a ui-state write — then
// rewrote the real agent theme files (~/.omp, ~/.claude, ~/.config/opencode,
// ghostty) and reloaded the live herdr, flipping every terminal's colours
// mid-run. Pointing the defaults at a throwaway home makes a leak land there.
func TestMain(m *testing.M) {
	// The /browser-mcp tests re-exec this binary as their chrome-devtools-mcp
	// stand-in (browsermcp_test.go), so no node is needed to test the bridge.
	if mode := os.Getenv(fakeBrowserMCPEnv); mode != "" {
		os.Exit(runFakeBrowserMCP(mode))
	}
	home, err := os.MkdirTemp("", "lasso-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	for k, v := range map[string]string{
		"HOME":              home,
		"XDG_CONFIG_HOME":   home + "/.config",
		"LASSO_DIR":         home + "/.lasso",
		"HERDR_CONFIG_PATH": home + "/.config/herdr/config.toml",
		"CODEX_HOME":        home + "/.codex",
		"KIMI_CODE_HOME":    home + "/.kimi",
	} {
		os.Setenv(k, v)
	}
	for _, k := range []string{"HERDR_SOCKET_PATH", "HERDR_PANE_ID", "HERDR_TAB_ID", "HERDR_WORKSPACE_ID", "HERDR_ENV", "LASSO_URL", "LASSO_LISTEN", "LASSO_MCP_TOKEN", "UI_AUTH", "MCP_OAUTH"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
