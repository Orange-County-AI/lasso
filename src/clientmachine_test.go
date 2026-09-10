package main

import (
	"os"
	"path/filepath"
	"testing"
)

// clientStateBackend serves HomeDir/ReadFile out of a temp dir under a chosen
// name; every other Backend method panics, so a test that strays says so.
type clientStateBackend struct {
	Backend
	name string
	home string
}

func (b *clientStateBackend) Name() string                      { return b.name }
func (b *clientStateBackend) HomeDir() (string, error)          { return b.home, nil }
func (b *clientStateBackend) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

// The shapes herdr 0.9's client actually writes, trimmed to the fields lasso
// reads (captured from titan while switching machines in the sidebar).
const (
	selectionTicket500 = `{"version":1,"selected_profile":"3f87b9ed37174500ee3dcbc5f75b1616"}`
	selectionLocal     = `{"version":1,"selected_profile":null}`
	machineEndpoints   = `{
  "version": 1,
  "ssh": [
    {"id":"3f87b9ed37174500ee3dcbc5f75b1616","label":"ticket500","target":"ticket500","session":"default","enabled":true},
    {"id":"0fb972d6907b631dd4410d4ce1eda82e","label":"minime","target":"minime","session":"default","enabled":false},
    {"id":"52f5244ed5481df92051ceb44ea6c6a2","label":"agents box","target":"ticket500","session":"agents","enabled":true}
  ]
}`
)

// machineFixture writes a client state dir holding the given files and returns a
// backend rooted at its home. The selection is re-read on a 1s TTL, so the
// caches are cleared for each fixture: two fixtures in one test would otherwise
// serve the first one's answer.
func machineFixture(t *testing.T, name string, files map[string]string) *clientStateBackend {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "state", "herdr", "client")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resetMachineCaches()
	t.Cleanup(resetMachineCaches)
	return &clientStateBackend{name: name, home: home}
}

func resetMachineCaches() {
	selectionCache.Lock()
	selectionCache.m = map[string]selectionEntry{}
	selectionCache.Unlock()
	profilesCache.Lock()
	profilesCache.m = map[string]profilesEntry{}
	profilesCache.Unlock()
}

// The bug this exists for: the terminal shows ticket500's session while the
// local herdr server — the only one lasso can ask — still reports a local pane.
func TestClientMachineHopSelected(t *testing.T) {
	// hostAliasFor answers only for a host lasso may drive, and the default
	// host always is; standing it up as ticket500 is how the fake fleet gets a
	// second driveable alias beside "local".
	swapBackend(t, &paneHostNamedBackend{name: "ticket500"})
	be := machineFixture(t, "local", map[string]string{
		"endpoint-selection.json": selectionTicket500,
		"endpoints.json":          machineEndpoints,
	})

	hop, ok := clientMachineHop(be)
	if !ok || hop.host != "ticket500" || hop.agent != "" {
		t.Fatalf("clientMachineHop(selected) = (%+v, %v), want host ticket500, session-level", hop, ok)
	}
}

// Everything that must read as "the client is on Local", because being wrong
// here writes a pasted screenshot to a machine the agent cannot see.
func TestClientMachineHopSilentCases(t *testing.T) {
	swapBackend(t, &paneHostNamedBackend{name: "ticket500"})

	cases := []struct {
		name  string
		files map[string]string
	}{
		{"local selected", map[string]string{
			"endpoint-selection.json": selectionLocal,
			"endpoints.json":          machineEndpoints,
		}},
		// The profile is saved but not connected, so it cannot be on screen.
		{"disabled profile", map[string]string{
			"endpoint-selection.json": `{"version":1,"selected_profile":"0fb972d6907b631dd4410d4ce1eda82e"}`,
			"endpoints.json":          machineEndpoints,
		}},
		// A named remote session is a herdr server lasso does not talk to: its
		// panes are not the ones we would resolve on that host.
		{"named remote session", map[string]string{
			"endpoint-selection.json": `{"version":1,"selected_profile":"52f5244ed5481df92051ceb44ea6c6a2"}`,
			"endpoints.json":          machineEndpoints,
		}},
		{"unknown profile id", map[string]string{
			"endpoint-selection.json": `{"version":1,"selected_profile":"deadbeef"}`,
			"endpoints.json":          machineEndpoints,
		}},
		// A target lasso has no driveable host for has no filesystem to show.
		{"undriveable target", map[string]string{
			"endpoint-selection.json": `{"version":1,"selected_profile":"x"}`,
			"endpoints.json":          `{"version":1,"ssh":[{"id":"x","target":"not-a-configured-host","session":"default","enabled":true}]}`,
		}},
		{"no state files", map[string]string{}},
		{"unparseable selection", map[string]string{
			"endpoint-selection.json": "{not json",
			"endpoints.json":          machineEndpoints,
		}},
		{"unparseable profiles", map[string]string{
			"endpoint-selection.json": selectionTicket500,
			"endpoints.json":          "{not json",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			be := machineFixture(t, "local", c.files)
			if hop, ok := clientMachineHop(be); ok {
				t.Errorf("clientMachineHop(%s) = (%+v, true), want no hop", c.name, hop)
			}
		})
	}
}

// A tab on a remote host runs `herdr --remote <alias>` — a standalone attach
// with no machine sidebar — and that client runs on lasso's own box, sharing the
// selection file with the local one. Honoring it there would point the tab at
// whatever machine the LOCAL terminal happens to be showing.
func TestClientMachineHopOnlyLocalBackend(t *testing.T) {
	swapBackend(t, &paneHostNamedBackend{name: "ticket500"})
	be := machineFixture(t, "norm", map[string]string{
		"endpoint-selection.json": selectionTicket500,
		"endpoints.json":          machineEndpoints,
	})

	if hop, ok := clientMachineHop(be); ok {
		t.Errorf("clientMachineHop(remote tab) = (%+v, true), want no hop", hop)
	}
}

// A machine added and selected inside clientProfilesTTL must still resolve: an
// id the cached profiles don't know forces one re-read.
func TestClientMachineHopRereadsProfilesForUnknownID(t *testing.T) {
	swapBackend(t, &paneHostNamedBackend{name: "ticket500"})
	be := machineFixture(t, "local", map[string]string{
		"endpoint-selection.json": selectionTicket500,
		"endpoints.json":          `{"version":1,"ssh":[]}`,
	})
	if _, ok := clientMachineHop(be); ok {
		t.Fatal("clientMachineHop(no profiles) resolved, want no hop")
	}

	dir := filepath.Join(be.home, ".local", "state", "herdr", "client")
	if err := os.WriteFile(filepath.Join(dir, "endpoints.json"), []byte(machineEndpoints), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only the profiles are re-read here; the selection is still cached, which
	// is the point — the newly added id is what forces the refresh.
	hop, ok := clientMachineHop(be)
	if !ok || hop.host != "ticket500" {
		t.Fatalf("clientMachineHop(after machine add) = (%+v, %v), want host ticket500", hop, ok)
	}
}
