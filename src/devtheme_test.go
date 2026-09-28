package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// asDevLasso runs the rest of the test as a `-dev` lasso, with the theme-sync
// opt-in set to optIn ("" = unset). Every path below touches only temp dirs:
// the whole point of the gate is that a dev process must not reach the real
// herdr config, the real agent theme files or the fleet.
func asDevLasso(t *testing.T, optIn string) {
	t.Helper()
	prev := *devMode
	*devMode = true
	t.Cleanup(func() { *devMode = prev })
	t.Setenv(devThemeSyncEnv, optIn)
}

// hermeticThemeHome points HOME and herdr's config at temp paths and returns
// both, so a write that slipped through the gate would land here (and be seen)
// rather than in the human's real files.
func hermeticThemeHome(t *testing.T) (home, cfg string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfg = filepath.Join(home, ".config", "herdr", "config.toml")
	t.Setenv("HERDR_CONFIG_PATH", cfg)
	return home, cfg
}

func assertEmptyDir(t *testing.T, dir, why string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("%s: %s gained %v, want untouched", why, dir, names)
	}
}

// The gate itself: production is always allowed (byte-for-byte unchanged), a
// dev lasso never is, and only the exact opt-in "1" gives it back.
func TestThemeWritesAllowedGate(t *testing.T) {
	prev := *devMode
	t.Cleanup(func() { *devMode = prev })

	*devMode = false
	t.Setenv(devThemeSyncEnv, "")
	if !themeWritesAllowed() {
		t.Error("production lasso: theme writes refused")
	}
	t.Setenv(devThemeSyncEnv, "0")
	if !themeWritesAllowed() {
		t.Error("production lasso: the dev opt-in variable must not affect it")
	}

	*devMode = true
	for _, v := range []string{"", "0", "true", "yes"} {
		t.Setenv(devThemeSyncEnv, v)
		if themeWritesAllowed() {
			t.Errorf("dev lasso with %s=%q: theme writes allowed, want refused", devThemeSyncEnv, v)
		}
	}
	t.Setenv(devThemeSyncEnv, "1")
	if !themeWritesAllowed() {
		t.Errorf("dev lasso with %s=1: theme writes refused, want allowed", devThemeSyncEnv)
	}
}

// The path that re-themed the fleet: a fresh dev process has an empty write
// record, so every probed host read as behind and was pushed the dev's theme.
// A dev lasso never converges; the opt-in restores the production behaviour.
func TestDevLassoDoesNotConvergeOnProbe(t *testing.T) {
	openTestDB(t)
	resetThemeSynced(t)
	prevHub := srvHub
	srvHub = newHub()
	srvHub.curTheme = resolveThemeByName("nord")
	t.Cleanup(func() { srvHub = prevHub })

	pushed := make(chan string, 8)
	prevFn := syncThemeToHostFn
	syncThemeToHostFn = func(host string, rt resolvedTheme) {
		markThemeSynced(host, themeStampFor(rt))
		pushed <- host
	}
	t.Cleanup(func() { syncThemeToHostFn = prevFn })

	row := HostInfo{Alias: "minime", Reachable: true, Running: true}
	asDevLasso(t, "")
	convergeThemeOnProbe(row)
	select {
	case h := <-pushed:
		t.Fatalf("dev lasso converged %s onto its theme", h)
	case <-time.After(200 * time.Millisecond):
	}
	if _, ok := themeSyncedFor("minime"); ok {
		t.Error("dev lasso recorded a write it did not make")
	}

	t.Setenv(devThemeSyncEnv, "1")
	convergeThemeOnProbe(row)
	select {
	case h := <-pushed:
		if h != "minime" {
			t.Errorf("pushed %q, want minime", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("with the opt-in a stale host was never converged")
	}
}

// Every write chokepoint refuses in dev, and each would otherwise have written
// into the temp home: the boot/local agent mirror, a remote (host-attach) sync,
// the per-host push, the fleet fan-out, the backdrop re-mirror, the local herdr
// config write and the boot tidy of that config.
func TestDevLassoWritesNoThemeState(t *testing.T) {
	openTestDB(t)
	resetThemeSynced(t)
	stubSSHHosts(t)
	resetHostStore(t)
	t.Cleanup(func() { resetHostStore(t) })
	home, cfg := hermeticThemeHome(t)
	asDevLasso(t, "")

	rt := resolveThemeByName("nord")
	if err := syncAgentThemesVia(localFsBackend(), rt); err != nil {
		t.Errorf("syncAgentThemesVia: %v", err)
	}
	if err := syncThemeToHostErr("local", rt); err != nil {
		t.Errorf("syncThemeToHostErr: %v", err)
	}
	syncThemeEverywhere(rt)
	syncAgentThemesEverywhere(rt)

	stub := &themeTargetStub{Backend: &localBackend{}, cfg: cfg, sock: "/tmp/no-such-herdr.sock"}
	if err := syncRemoteTheme(stub, "nord"); err != nil {
		t.Errorf("syncRemoteTheme: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("dev lasso asked a herdr to reload %d times", stub.calls)
	}

	if err := setLocalHerdrTheme("nord"); !errors.Is(err, errThemeReadOnly) {
		t.Errorf("setLocalHerdrTheme = %v, want errThemeReadOnly", err)
	}
	assertEmptyDir(t, home, "dev theme writes")
	if _, ok := themeSyncedFor("local"); ok {
		t.Error("dev lasso recorded a local write it did not make")
	}

	// The boot tidy rewrites a legacy config in production; a dev lasso leaves
	// that to the production lasso sharing the file.
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	const legacy = "[theme]\nname = \"retro-82\"\n"
	if err := os.WriteFile(cfg, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	tidyHerdrThemeConfig("test")
	if got, _ := os.ReadFile(cfg); string(got) != legacy {
		t.Errorf("dev lasso rewrote herdr's config:\n%s", got)
	}

	// The opt-in writes again — the same call, the same temp home.
	t.Setenv(devThemeSyncEnv, "1")
	if err := syncAgentThemesVia(localFsBackend(), rt); err != nil {
		t.Fatalf("opted-in syncAgentThemesVia: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "themes", "herdr.json")); err != nil {
		t.Errorf("opted-in dev lasso did not write the agent theme: %v", err)
	}
}

// A human changing the theme from a dev lasso's UI is refused out loud (409
// with the reason), never applied to half the world or silently dropped. That
// covers the herdr picker, the sync-policy toggles (which live in the shared
// lasso.db and steer the PRODUCTION lasso's writes) and the fleet push "Sync
// now" and every appearance change make. A plain read still answers.
func TestDevLassoRefusesThemeEndpoints(t *testing.T) {
	openTestDB(t)
	stubSSHHosts(t, "minime")
	_, cfg := hermeticThemeHome(t)
	asDevLasso(t, "")

	set := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		serveThemeSet(rec, httptest.NewRequest(http.MethodPost, "/api/theme-set", strings.NewReader(body)))
		return rec
	}
	for _, body := range []string{
		`{"name":"dracula"}`,
		`{"sync_agent_themes":false}`,
		`{"theme_sync_host":"minime","theme_sync":false}`,
	} {
		rec := set(body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), devThemeSyncEnv) {
			t.Errorf("%s: %d %q, want 409 naming %s", body, rec.Code, rec.Body.String(), devThemeSyncEnv)
		}
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Error("herdr's config was written by a refused request")
	}
	if !syncAgentThemesEnabled() || len(themeSyncOffHosts()) != 0 {
		t.Errorf("a refused request changed the shared sync policy: agents=%v off=%v",
			syncAgentThemesEnabled(), themeSyncOffHosts())
	}
	if rec := set(`{}`); rec.Code != http.StatusOK {
		t.Errorf("plain read: %d %q, want 200", rec.Code, rec.Body.String())
	}

	for _, body := range []string{`{"palette":"nord"}`, `{"palette":"nord","quiet":true}`} {
		rec := httptest.NewRecorder()
		serveThemeSync(rec, httptest.NewRequest(http.MethodPost, "/api/theme-sync", strings.NewReader(body)))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "dev lasso") {
			t.Errorf("theme-sync %s: %d %q, want 409 from the dev gate", body, rec.Code, rec.Body.String())
		}
	}
	if themeSyncRunning.Load() {
		t.Error("a refused theme-sync left a fanout marked running")
	}

	// With the opt-in the policy toggle is honoured again.
	t.Setenv(devThemeSyncEnv, "1")
	if rec := set(`{"sync_agent_themes":false}`); rec.Code != http.StatusOK {
		t.Fatalf("opted-in toggle: %d %q, want 200", rec.Code, rec.Body.String())
	}
	if syncAgentThemesEnabled() {
		t.Error("opted-in toggle did not land")
	}
}

// A dev lasso FOLLOWS herdr's config.toml, palette or not. Production refuses a
// config theme it did not write while a palette governs, telling its own push
// from a hand edit by its write record — but a dev lasso has no record of the
// production lasso's writes, so the refusal pinned it to the theme the fleet had
// just left ("config.toml names execution-associates, which lasso did not
// write … stays on ocai"), and that stale theme is what it then pushed. It
// adopts instead, and the adoption writes nothing.
func TestDevLassoFollowsConfigWhilePaletteGoverns(t *testing.T) {
	openTestDB(t)
	resetThemeSynced(t)
	stubSSHHosts(t)
	resetHostStore(t)
	t.Cleanup(func() { resetHostStore(t) })
	home, cfg := hermeticThemeHome(t)
	if err := setHerdrThemeName(cfg, "nord"); err != nil {
		t.Fatal(err)
	}
	asDevLasso(t, "")
	h := &hub{curTheme: loadHerdrTheme("auto")}

	postUIState(t, `{"appearance_mode":"system","palette_dark":"dracula","client_id":"A","user_intent":true}`)
	// The production lasso pushes the palette: it writes config.toml and marks
	// the write in ITS memory, not this process's.
	if err := setHerdrThemeName(cfg, "dracula"); err != nil {
		t.Fatal(err)
	}
	h.refreshTheme()
	if h.curTheme.Resolved != "dracula" {
		t.Errorf("dev hub on %q, want it to follow config.toml to dracula", h.curTheme.Resolved)
	}
	// Give an (erroneous) fan-out goroutine a moment to land before checking.
	time.Sleep(100 * time.Millisecond)
	for _, sub := range []string{".claude", ".omp", filepath.Join(".config", "ghostty"), filepath.Join(".config", "opencode")} {
		if _, err := os.Stat(filepath.Join(home, sub)); err == nil {
			t.Errorf("following a theme wrote %s", sub)
		}
	}
	if _, ok := themeSyncedFor("local"); ok {
		t.Error("following a theme recorded a write")
	}
}
