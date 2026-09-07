package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// themeSyncHome sets up a home dir with a ghostty config to be repointed, and
// returns a backend writing into it. It stands in for any host lasso can write
// files on — including one whose Luvus this build cannot speak to, since theme
// writes need file I/O and nothing else.
func themeSyncHome(t *testing.T) (Backend, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LASSO_DIR", t.TempDir())
	ghostty := filepath.Join(home, ".config", "ghostty", "config")
	if err := os.MkdirAll(filepath.Dir(ghostty), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ghostty, []byte("font-size = 14\ntheme = Dracula\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &localBackend{}, home
}

// A host lasso can only write files on still gets every agent theme file: that
// is the whole point of the files-only path. Nothing is asked of its Luvus, and
// the sync reports success rather than a failure that would make the converger
// retry forever.
func TestSyncAgentThemesWritesEveryCLI(t *testing.T) {
	b, home := themeSyncHome(t)
	rt := bundledTheme("gruvbox")

	if err := syncAgentThemesVia(b, rt); err != nil {
		t.Fatalf("syncAgentThemesVia: %v", err)
	}

	ghosttyTheme := filepath.Join(home, ".config", "ghostty", "themes", ghosttyThemeName)
	body, err := os.ReadFile(ghosttyTheme)
	if err != nil {
		t.Fatalf("ghostty theme not written: %v", err)
	}
	if !strings.Contains(string(body), rt.p.Mantle) {
		t.Errorf("ghostty theme is not gruvbox (want background %s):\n%s", rt.p.Mantle, body)
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".config", "ghostty", "config"))
	if err != nil {
		t.Fatalf("ghostty config unreadable: %v", err)
	}
	if !strings.Contains(string(cfg), "theme = "+ghosttyThemeName) {
		t.Errorf("ghostty config not repointed:\n%s", cfg)
	}
	claude, err := os.ReadFile(filepath.Join(home, ".claude", "themes", claudeThemeName+".json"))
	if err != nil {
		t.Fatalf("claude theme not written: %v", err)
	}
	if !strings.Contains(string(claude), rt.p.Accent) {
		t.Errorf("claude theme did not take the palette:\n%s", claude)
	}
	oc, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "themes", opencodeThemeName+".json"))
	if err != nil {
		t.Fatalf("opencode theme not written: %v", err)
	}
	if !strings.Contains(string(oc), rt.p.Accent) {
		t.Errorf("opencode theme did not take the palette:\n%s", oc)
	}
	if _, err := os.ReadFile(filepath.Join(home, ".lasso", "settings.json")); err != nil {
		t.Fatalf("lasso appearance not written: %v", err)
	}
}

// lasso can start before ttyd autostarts Luvus, so "no palette yet" is a normal
// boot state. Writing the stand-in into a host's agent config would leave it
// painted in a theme nobody chose, and no later pass would know to correct it.
func TestUnavailableThemeWritesNothing(t *testing.T) {
	b, home := themeSyncHome(t)

	if err := syncAgentThemesVia(b, unavailableTheme()); err != nil {
		t.Fatalf("an unavailable theme must be a no-op, not an error: %v", err)
	}
	for _, path := range []string{
		filepath.Join(home, ".config", "ghostty", "themes", ghosttyThemeName),
		filepath.Join(home, ".claude", "themes", claudeThemeName+".json"),
		filepath.Join(home, ".config", "opencode", "themes", opencodeThemeName+".json"),
		filepath.Join(home, ".lasso", "settings.json"),
	} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("wrote %s from a theme lasso never resolved", path)
		}
	}
	if cfg, err := os.ReadFile(filepath.Join(home, ".config", "ghostty", "config")); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(cfg), "theme = Dracula") {
		t.Errorf("repointed ghostty at a theme lasso never resolved:\n%s", cfg)
	}
}

// resetThemeSynced clears the convergence bookkeeping so tests don't inherit
// each other's "already written" records.
func resetThemeSynced(t *testing.T) {
	t.Helper()
	clear := func() {
		themeSynced.mu.Lock()
		themeSynced.by, themeSynced.inFlight = nil, nil
		themeSynced.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// The probe path is the reconcile loop: a settled, reachable host whose palette
// lasso has not written gets one push, and the next probe of a host already in
// step costs nothing. This is what catches up a laptop that was asleep when the
// theme changed — before it, nothing ever revisited such a host.
func TestConvergeThemeOnProbePushesStaleHostOnce(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)
	resetThemeSynced(t)

	prevHub := srvHub
	srvHub = newHub()
	srvHub.curTheme = bundledTheme("nord")
	t.Cleanup(func() { srvHub = prevHub })

	pushed := make(chan string, 8)
	prevFn := syncThemeToHostFn
	syncThemeToHostFn = func(host string, rt resolvedTheme) {
		markThemeSynced(host, rt.fingerprint())
		pushed <- host + ":" + rt.Name
	}
	t.Cleanup(func() { syncThemeToHostFn = prevFn })

	settled := HostInfo{Alias: "sleepy", Reachable: true, Running: true}
	convergeThemeOnProbe(settled)
	select {
	case got := <-pushed:
		if got != "sleepy:nord" {
			t.Fatalf("pushed %q, want sleepy:nord", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stale reachable host was never converged")
	}

	// Already in step: no second write, however often it is probed.
	convergeThemeOnProbe(settled)
	convergeThemeOnProbe(settled)
	// Rows with no verdict, no reachability, or the local machine are not the
	// converger's business.
	convergeThemeOnProbe(HostInfo{Alias: "waiting", Reachable: true, State: hostProbing})
	convergeThemeOnProbe(HostInfo{Alias: "gone"})
	convergeThemeOnProbe(HostInfo{Alias: "local", Reachable: true, Running: true})
	select {
	case got := <-pushed:
		t.Errorf("unexpected extra push: %q", got)
	case <-time.After(200 * time.Millisecond):
	}

	// A theme change makes the same host stale again.
	srvHub.curTheme = bundledTheme("dracula")
	convergeThemeOnProbe(settled)
	select {
	case got := <-pushed:
		if got != "sleepy:dracula" {
			t.Errorf("pushed %q, want sleepy:dracula", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a host was not re-converged after a theme change")
	}
}

// An installed theme can be edited and reinstalled under the SAME id. The
// convergence record is keyed on the palette, not the name, so that edit
// re-converges the fleet instead of reading as "already in step".
func TestConvergeThemeFollowsEditedPalette(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)
	resetThemeSynced(t)

	prevHub := srvHub
	srvHub = newHub()
	t.Cleanup(func() { srvHub = prevHub })

	edited := bundledTheme("nord")
	edited.Name, edited.Resolved, edited.Customized = "team", "nord", true
	srvHub.curTheme = edited

	pushed := make(chan string, 4)
	prevFn := syncThemeToHostFn
	syncThemeToHostFn = func(host string, rt resolvedTheme) {
		markThemeSynced(host, rt.fingerprint())
		pushed <- rt.p.Accent
	}
	t.Cleanup(func() { syncThemeToHostFn = prevFn })

	row := HostInfo{Alias: "sleepy", Reachable: true, Running: true}
	convergeThemeOnProbe(row)
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("first push never happened")
	}

	// Same id, new colors — the user edited the theme file and reinstalled it.
	edited.p.Accent = "#ff00ff"
	srvHub.curTheme = edited
	convergeThemeOnProbe(row)
	select {
	case got := <-pushed:
		if got != "#ff00ff" {
			t.Errorf("re-converged with accent %q, want the edited #ff00ff", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an edited palette under the same id was never re-converged")
	}
}

// A failed push leaves no record, so the next probe retries it. Without this a
// host that was reachable but unwritable (sleeping mid-write, full disk) would
// be remembered as done and never revisited.
func TestConvergeThemeRetriesAfterFailure(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)
	resetThemeSynced(t)

	prevHub := srvHub
	srvHub = newHub()
	srvHub.curTheme = bundledTheme("nord")
	t.Cleanup(func() { srvHub = prevHub })

	pushed := make(chan string, 8)
	prevFn := syncThemeToHostFn
	syncThemeToHostFn = func(host string, rt resolvedTheme) {
		forgetThemeSynced(host) // what a failed write does
		pushed <- host
	}
	t.Cleanup(func() { syncThemeToHostFn = prevFn })

	row := HostInfo{Alias: "flaky", Reachable: true, Running: true}
	for i := range 2 {
		convergeThemeOnProbe(row)
		select {
		case <-pushed:
		case <-time.After(2 * time.Second):
			t.Fatalf("push %d never happened: a failed host must be retried", i+1)
		}
	}
}

// The deny-list wins over convergence: a host the user switched off is not
// written to by the probe path either.
func TestConvergeThemeHonorsDenyList(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)
	resetThemeSynced(t)
	if err := setThemeSyncFor("muted", false); err != nil {
		t.Fatalf("disable muted: %v", err)
	}

	prevHub := srvHub
	srvHub = newHub()
	srvHub.curTheme = bundledTheme("nord")
	t.Cleanup(func() { srvHub = prevHub })

	pushed := make(chan string, 4)
	prevFn := syncThemeToHostFn
	syncThemeToHostFn = func(host string, _ resolvedTheme) { pushed <- host }
	t.Cleanup(func() { syncThemeToHostFn = prevFn })

	convergeThemeOnProbe(HostInfo{Alias: "muted", Reachable: true, Running: true})
	select {
	case got := <-pushed:
		t.Errorf("wrote theme to opted-out host %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// The probe path must not push a theme lasso never resolved, even to a host
// that is perfectly reachable: that is the boot window before Luvus is up.
func TestConvergeThemeSkipsUnavailableTheme(t *testing.T) {
	t.Setenv("LASSO_DIR", t.TempDir())
	if err := openDB(); err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(closeTestDB)
	resetThemeSynced(t)

	prevHub := srvHub
	srvHub = newHub()
	srvHub.curTheme = unavailableTheme()
	t.Cleanup(func() { srvHub = prevHub })

	pushed := make(chan string, 4)
	prevFn := syncThemeToHostFn
	syncThemeToHostFn = func(host string, _ resolvedTheme) { pushed <- host }
	t.Cleanup(func() { syncThemeToHostFn = prevFn })

	convergeThemeOnProbe(HostInfo{Alias: "sleepy", Reachable: true, Running: true})
	select {
	case got := <-pushed:
		t.Errorf("pushed a stand-in palette to %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}
