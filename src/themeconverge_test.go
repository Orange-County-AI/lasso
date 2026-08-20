package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// themeTargetStub is a themeTarget over the local filesystem: a host whose files
// lasso can write but whose herdr it may not be able to talk to. sock "" is a
// files-only connection (newRemoteFileBackend — a host running a herdr protocol
// this build refuses); a non-empty sock with callErr set is a herdr that answers
// with a failure.
type themeTargetStub struct {
	Backend
	cfg     string
	sock    string
	calls   int
	callErr error
}

func (s *themeTargetStub) herdrConfigPath() string { return s.cfg }
func (s *themeTargetStub) HerdrSock() string       { return s.sock }

func (s *themeTargetStub) HerdrCall(string, any) (json.RawMessage, error) {
	s.calls++
	return nil, s.callErr
}

// themeStubHome sets up a home dir with a ghostty config to be repointed, and
// returns the stub writing into it.
func themeStubHome(t *testing.T, sock string, callErr error) (*themeTargetStub, string) {
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
	return &themeTargetStub{
		Backend: &localBackend{},
		cfg:     filepath.Join(home, ".config", "herdr", "config.toml"),
		sock:    sock,
		callErr: callErr,
	}, home
}

func assertThemeFilesWritten(t *testing.T, home, want string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, ".config", "ghostty", "themes", ghosttyThemeName))
	if err != nil {
		t.Fatalf("ghostty theme not written: %v", err)
	}
	if !strings.Contains(string(body), resolveThemeByName(want).ui.PanelBg) {
		t.Errorf("ghostty theme is not %s:\n%s", want, body)
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".config", "ghostty", "config"))
	if err != nil {
		t.Fatalf("ghostty config unreadable: %v", err)
	}
	if !strings.Contains(string(cfg), "theme = "+ghosttyThemeName) {
		t.Errorf("ghostty config not repointed:\n%s", cfg)
	}
	if _, err := os.ReadFile(filepath.Join(home, ".claude", "themes", "herdr.json")); err != nil {
		t.Fatalf("claude theme not written: %v", err)
	}
	herdr, err := os.ReadFile(filepath.Join(home, ".config", "herdr", "config.toml"))
	if err != nil {
		t.Fatalf("herdr config not written: %v", err)
	}
	if !strings.Contains(string(herdr), `name = "`+want+`"`) {
		t.Errorf("herdr theme name not %s:\n%s", want, herdr)
	}
}

// A host lasso cannot speak herdr to still gets every theme file: that is the
// whole point of the files-only path. Nothing is asked of its herdr (there is no
// socket to ask), and the sync reports success rather than a failure that would
// make the converger retry forever.
func TestSyncRemoteThemeWithoutHerdrSocket(t *testing.T) {
	stub, home := themeStubHome(t, "", nil)

	if err := syncRemoteTheme(stub, "gruvbox"); err != nil {
		t.Fatalf("syncRemoteTheme: %v", err)
	}
	assertThemeFilesWritten(t, home, "gruvbox")
	if stub.calls != 0 {
		t.Errorf("HerdrCall attempted %d times on a files-only target", stub.calls)
	}
}

// A herdr that refuses the reload must not cost the host its theme FILES — the
// old order asked for the reload first and returned on its error, so a host with
// an unreachable herdr kept a stale ghostty palette indefinitely. The reload is
// the last step and its failure is not the caller's problem.
func TestSyncRemoteThemeSurvivesReloadFailure(t *testing.T) {
	stub, home := themeStubHome(t, "/tmp/no-such-herdr.sock", errors.New("connection refused"))

	if err := syncRemoteTheme(stub, "nord"); err != nil {
		t.Fatalf("a failed reload must not fail the sync: %v", err)
	}
	assertThemeFilesWritten(t, home, "nord")
	if stub.calls != 1 {
		t.Errorf("reload attempted %d times, want 1", stub.calls)
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

// The probe path is the reconcile loop: a settled, reachable host whose theme
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
	srvHub.curTheme = resolveThemeByName("nord")
	t.Cleanup(func() { srvHub = prevHub })

	pushed := make(chan string, 8)
	prevFn := syncThemeToHostFn
	syncThemeToHostFn = func(host string, rt resolvedTheme) {
		markThemeSynced(host, rt.Resolved)
		pushed <- host + ":" + rt.Resolved
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
	srvHub.curTheme = resolveThemeByName("dracula")
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
	srvHub.curTheme = resolveThemeByName("nord")
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
	srvHub.curTheme = resolveThemeByName("nord")
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
