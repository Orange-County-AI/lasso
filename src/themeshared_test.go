package main

import (
	"os"
	"path/filepath"
	"testing"
)

// simLasso is one lasso PROCESS as far as theme ownership goes: its identity,
// the record of what it wrote itself, and its hub. Two of them over one
// lasso.db is a production lasso and a `mise run dev` one on the same machine —
// the pair that reverted each other on 2026-09-28. Everything else a process
// has (the db, herdr's config.toml, the fleet) is shared, which is the point.
type simLasso struct {
	instance string
	by       map[string]themeStamp
	hub      *hub
}

// sharedThemeWorld wires the package globals for a hermetic multi-process run:
// a temp db, HOME and herdr config, no ssh hosts, no reachable herdr socket,
// and a convergence seam that records which process pushed what.
type sharedThemeWorld struct {
	t      *testing.T
	cfg    string
	pushed chan string
}

func newSharedThemeWorld(t *testing.T) *sharedThemeWorld {
	t.Helper()
	openTestDB(t)
	resetThemeSynced(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfg := filepath.Join(home, ".config", "herdr", "config.toml")
	t.Setenv("HERDR_CONFIG_PATH", cfg)
	stubSSHHosts(t)
	resetHostStore(t)
	t.Cleanup(func() { resetHostStore(t) })

	prevSock, prevInst, prevHub, prevFn := *herdrSock, themeInstance, srvHub, syncThemeToHostFn
	*herdrSock = filepath.Join(home, "no-herdr.sock")
	w := &sharedThemeWorld{t: t, cfg: cfg, pushed: make(chan string, 16)}
	syncThemeToHostFn = func(host string, rt resolvedTheme) {
		markThemeSynced(host, themeStampFor(rt))
		w.pushed <- themeInstance + ":" + host + ":" + rt.Resolved
	}
	t.Cleanup(func() {
		*herdrSock, themeInstance, srvHub, syncThemeToHostFn = prevSock, prevInst, prevHub, prevFn
	})
	// The shared settings of the incident: "system" with the brand pair, so the
	// fleet's theme is a palette and the palette-governs rule is in force.
	postUIState(t, `{"appearance_mode":"system","palette_light":"ocai","palette_dark":"execution-associates","client_id":"A","user_intent":true}`)
	// Registered last so it runs FIRST: every background theme write the code
	// under test started (fan-outs, convergence pushes) finishes before the db
	// closes and the temp HOME is removed, instead of writing into both after.
	t.Cleanup(drainThemeBackground)
	return w
}

// as runs fn as process p: its identity, its own write record, its hub. The
// background work fn starts is drained before the swap back out, so a fan-out
// p kicked records its writes as p's, not as whichever process runs next.
func (w *sharedThemeWorld) as(p *simLasso, fn func()) {
	themeInstance = p.instance
	themeSynced.mu.Lock()
	themeSynced.by, themeSynced.inFlight = p.by, nil
	themeSynced.mu.Unlock()
	srvHub = p.hub
	fn()
	drainThemeBackground()
	themeSynced.mu.Lock()
	p.by = themeSynced.by
	themeSynced.mu.Unlock()
}

// boot starts a process the way runServer does: bootTheme over config.toml.
func (w *sharedThemeWorld) boot(instance string) *simLasso {
	p := &simLasso{instance: instance, by: map[string]themeStamp{}}
	w.as(p, func() {
		h := newHub()
		h.curTheme = bootTheme(loadHerdrTheme("auto"))
		p.hub = h
	})
	return p
}

// converge delivers one probe of host to p and reports what it pushed, "" if
// nothing. as drains the push before returning, so the answer is final — no
// timeout to guess at — and the next process swap cannot race it.
func (w *sharedThemeWorld) converge(p *simLasso, host string) string {
	w.as(p, func() {
		convergeThemeOnProbe(HostInfo{Alias: host, Reachable: true, Running: true})
	})
	select {
	case got := <-w.pushed:
		return got
	default:
		return ""
	}
}

func (w *sharedThemeWorld) theme(p *simLasso) string { return p.hub.themeSnapshot().Resolved }

// The 2026-09-28 race, replayed. Production and a dev lasso share one db and
// one herdr config; production pushes the palette a browser resolved. With
// each process's record private, the dev hub called that write a stray edit,
// stayed on the old theme, and converged it back over the fleet. With the
// record shared, the dev hub adopts the push, and after that neither process
// pushes anything the other has already put on a host — nor does a process
// that boots later.
func TestTwoLassosOnOneDBDoNotRevertEachOther(t *testing.T) {
	w := newSharedThemeWorld(t)
	if err := setHerdrThemeName(w.cfg, "ocai"); err != nil {
		t.Fatal(err)
	}

	prod := w.boot("prod")
	if got := w.converge(prod, "minime"); got != "prod:minime:ocai" {
		t.Fatalf("prod's boot convergence pushed %q, want ocai to minime", got)
	}
	dev := w.boot("dev")
	if w.theme(dev) != "ocai" {
		t.Fatalf("dev booted on %q, want ocai", w.theme(dev))
	}
	// Same build, record already at dev's theme: a fresh process does not
	// re-push a host another lasso already put in step.
	if got := w.converge(dev, "minime"); got != "" {
		t.Errorf("fresh dev re-pushed a host already in step: %q", got)
	}

	// 19:10:40 — a production tab's appearance push (POST /api/theme-sync) makes
	// prod write execution-associates into herdr's config.
	w.as(prod, func() {
		if err := setLocalHerdrTheme("execution-associates"); err != nil {
			t.Fatalf("setLocalHerdrTheme: %v", err)
		}
	})
	if w.theme(prod) != "execution-associates" {
		t.Fatalf("prod hub on %q after its own push", w.theme(prod))
	}
	if got := w.converge(prod, "minime"); got != "prod:minime:execution-associates" {
		t.Fatalf("prod did not converge minime onto its push: %q", got)
	}

	// 19:10:42 — dev's poll sees the file. It is lasso's write, just not THIS
	// process's, and it must be adopted rather than refused as a stray.
	w.as(dev, func() { dev.hub.refreshTheme() })
	if w.theme(dev) != "execution-associates" {
		t.Fatalf("dev refused production's palette push: hub on %q, want execution-associates", w.theme(dev))
	}
	// And the two now agree, so round after round nobody pushes anything.
	for range 3 {
		for _, p := range []*simLasso{prod, dev} {
			if got := w.converge(p, "minime"); got != "" {
				t.Errorf("%s re-pushed minime once both agreed: %q", p.instance, got)
			}
		}
	}

	// A process that boots after the write comes up on it and pushes nothing.
	late := w.boot("late")
	if w.theme(late) != "execution-associates" {
		t.Errorf("late boot on %q, want execution-associates", w.theme(late))
	}
	if got := w.converge(late, "minime"); got != "" {
		t.Errorf("late boot re-pushed a host already in step: %q", got)
	}

	// A stale push that LANDS late — dev's boot pass for another host, dispatched
	// before the change, completing after it — is visible in the shared record,
	// so whichever lasso probes that host next puts it back on the fleet theme.
	w.as(dev, func() { markThemeSynced("gigachad", themeStampFor(resolveThemeByName("ocai"))) })
	if got := w.converge(prod, "gigachad"); got != "prod:gigachad:execution-associates" {
		t.Errorf("a stale write another lasso landed was not caught up: %q", got)
	}
	if got := w.converge(dev, "gigachad"); got != "" {
		t.Errorf("dev fought prod's catch-up: %q", got)
	}

	if name, _ := themeSyncedFor("minime"); name != "execution-associates" {
		t.Errorf("minime's shared record = %q, want execution-associates", name)
	}
}

// A hand edit (or herdr's own popup) while a palette governs is refused by
// every running lasso — and now by one booting over it too. A booting process
// used to adopt it (its record was empty), and the two then converged the fleet
// onto their two themes in turn, every probe, for as long as both ran.
func TestBootingLassoRefusesAStrayEditTheRunningOnesRefuse(t *testing.T) {
	w := newSharedThemeWorld(t)
	if err := setHerdrThemeName(w.cfg, "ocai"); err != nil {
		t.Fatal(err)
	}
	prod := w.boot("prod")
	w.as(prod, func() {
		if err := setLocalHerdrTheme("execution-associates"); err != nil {
			t.Fatal(err)
		}
	})
	w.converge(prod, "minime")

	if err := os.WriteFile(w.cfg, []byte("[theme]\nname = \"nord\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.as(prod, func() { prod.hub.refreshTheme() })
	if w.theme(prod) != "execution-associates" {
		t.Fatalf("prod adopted a stray edit: %q", w.theme(prod))
	}

	dev := w.boot("dev")
	if w.theme(dev) != "execution-associates" {
		t.Errorf("dev booted onto the stray edit %q, want execution-associates like prod", w.theme(dev))
	}
	w.as(dev, func() { dev.hub.refreshTheme() })
	if w.theme(dev) != "execution-associates" {
		t.Errorf("dev's poll adopted the stray edit: %q", w.theme(dev))
	}
	for range 2 {
		for _, p := range []*simLasso{prod, dev} {
			if got := w.converge(p, "minime"); got != "" {
				t.Errorf("%s pushed %q over a host already on the fleet theme", p.instance, got)
			}
		}
	}

	// herdr mode hands the theme back to herdr's config: both follow the edit.
	postUIState(t, `{"appearance_mode":"herdr","client_id":"A","user_intent":true}`)
	for _, p := range []*simLasso{prod, dev} {
		w.as(p, func() { p.hub.refreshTheme() })
		if w.theme(p) != "nord" {
			t.Errorf("%s in herdr mode on %q, want nord", p.instance, w.theme(p))
		}
	}
}

// A record left by a different BUILD is re-verified once per process — a new
// build may write different bytes for the same theme, which is how `lasso
// update` reaches the fleet on restart — and only once, so a dev build and a
// production one sharing a db cannot re-push each other forever.
func TestOtherBuildsRecordIsReverifiedOnce(t *testing.T) {
	w := newSharedThemeWorld(t)
	if err := setHerdrThemeName(w.cfg, "nord"); err != nil {
		t.Fatal(err)
	}
	prevSemver := lassoSemver
	t.Cleanup(func() { lassoSemver = prevSemver })

	prod := w.boot("prod")
	if got := w.converge(prod, "minime"); got == "" {
		t.Fatal("prod never converged minime")
	}

	lassoSemver = "9.9.9-dev"
	dev := w.boot("dev")
	pushes := 0
	for range 3 {
		if w.converge(dev, "minime") != "" {
			pushes++
		}
	}
	if pushes != 1 {
		t.Errorf("dev (another build) pushed minime %d times, want exactly 1", pushes)
	}

	lassoSemver = prevSemver
	for range 3 {
		if got := w.converge(prod, "minime"); got != "" {
			t.Errorf("prod re-pushed after dev's same-theme write: %q", got)
		}
	}
}
