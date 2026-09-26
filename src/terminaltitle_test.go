package main

import "testing"

func TestTerminalTabName(t *testing.T) {
	for _, tc := range []struct {
		command, name string
		generate      bool
	}{
		{"", "", false},
		{"btm", "btm", false},
		{"git status", "git status", false},
		{"ssh  citadel", "ssh citadel", false},
		{"npm run dev --port 3000", "npm", true},
		{"tail -f log | grep err", "tail", true},
		{"FOO=1 sudo /usr/bin/htop -d 5", "htop", true},
		{"echo a\necho b", "echo", true},
		// A credential never leaves the box for the titler.
		{"export GITHUB_TOKEN=abc && gh pr list", "export", false},
		{"curl -H 'Authorization: Bearer x' https://example.com", "curl", false},
	} {
		name, gen := terminalTabName(tc.command)
		if name != tc.name || gen != tc.generate {
			t.Errorf("terminalTabName(%q) = %q, %v; want %q, %v", tc.command, name, gen, tc.name, tc.generate)
		}
	}
}

func TestAutoTitleTerminalRenamesTab(t *testing.T) {
	titlerRunner = func(titler, string) (string, error) { return "Lasso dev server with a long tail of words\n", nil }
	t.Cleanup(func() { titlerRunner = runTitler })
	b := &terminalCreateBackend{memBackend: newMemBackend()}
	autoTitleTerminal(b, "ws:t2", "cd ~/projects/lasso && mise run dev")
	if b.renameParams["tab_id"] != "ws:t2" {
		t.Fatalf("rename params = %#v", b.renameParams)
	}
	got, _ := b.renameParams["label"].(string)
	if got != "Lasso dev server with a long" {
		t.Fatalf("label = %q, want the name cut at a word within 30 chars", got)
	}
}
