package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// newTestTtydRole builds a role with no instances and a short bind timeout, for
// tests that never spawn a real ttyd.
func newTestTtydRole(t *testing.T) *ttydRole {
	t.Helper()
	r := newTtydRole(context.Background(), "ttyd", "/terminal")
	r.waitTimeout = 20 * time.Millisecond
	return r
}

// A resident instance is REUSED, not respawned: that is what makes a second tab
// arriving on a host someone already has open free. If ensure ever reached
// startTtyd it would fail (empty PATH), and retiring the old instance would call
// its cancel.
func TestTtydRoleReusesResidentInstance(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r := newTestTtydRole(t)
	stopped := false
	r.inst["ticket500"] = &ttydInstance{
		sock:       "/tmp/lasso-ttyd-test-ticket500.sock",
		cancel:     func() { stopped = true },
		lastActive: time.Now().Add(-time.Hour),
	}
	r.bySlug["ticket500"] = "ticket500"
	r.inst["local"] = &ttydInstance{sock: "/tmp/lasso-ttyd-test-local.sock", cancel: func() {}, lastActive: time.Now()}
	r.bySlug["local"] = "local"

	if err := r.ensure("ticket500", "herdr --remote ticket500", nil); err != nil {
		t.Fatalf("ensure a resident host: %v", err)
	}
	if stopped {
		t.Error("the resident instance was stopped — arriving on a warm host must not respawn it")
	}
	if got, want := r.sockForSlug("ticket500"), "/tmp/lasso-ttyd-test-ticket500.sock"; got != want {
		t.Errorf("sockForSlug = %q, want %q", got, want)
	}
	// The whole point of per-host routing: the other host's terminal is still
	// dialable at the same time, for the tab that is on it.
	if got, want := r.sockForSlug("local"), "/tmp/lasso-ttyd-test-local.sock"; got != want {
		t.Errorf("sockForSlug(local) = %q, want %q — a second host's terminal must stay reachable", got, want)
	}
	if len(r.inst) != 2 {
		t.Errorf("instances = %d, want both hosts resident", len(r.inst))
	}
}

// A spawn that fails registers nothing, so the proxy keeps reporting "no ttyd"
// instead of dialing a path nothing is listening on.
func TestTtydRoleFailedSpawnRegistersNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no ttyd binary
	r := newTestTtydRole(t)
	if err := r.ensure("local", "sh", nil); err == nil {
		t.Fatal("ensure succeeded without a ttyd binary")
	}
	if len(r.inst) != 0 {
		t.Errorf("instances = %d, want 0 after a failed spawn", len(r.inst))
	}
	if got := r.sockForSlug("local"); got != "" {
		t.Errorf("sockForSlug = %q, want empty", got)
	}
}

// An unknown slug — a stale iframe still pointed at a retired host — resolves to
// nothing rather than to some other host's terminal.
func TestTtydRoleUnknownSlugResolvesToNothing(t *testing.T) {
	r := newTestTtydRole(t)
	r.inst["local"] = &ttydInstance{sock: "/tmp/local.sock", cancel: func() {}, lastActive: time.Now()}
	r.bySlug["local"] = "local"
	if got := r.sockForSlug("norm"); got != "" {
		t.Errorf("sockForSlug(unknown) = %q, want empty", got)
	}
}

// Eviction retires the least-recently-active host and never a WATCHED one.
func TestTtydRoleEvictsLeastRecentlyActive(t *testing.T) {
	setDefaultBackend(&localBackend{})
	t.Cleanup(func() { setDefaultBackend(nil) })
	r := newTestTtydRole(t)
	now := time.Now()
	stopped := map[string]bool{}
	add := func(host string, age time.Duration) {
		r.inst[host] = &ttydInstance{
			sock:       "/tmp/" + host + ".sock",
			cancel:     func() { stopped[host] = true },
			lastActive: now.Add(-age),
		}
		r.bySlug[hostSlug(host)] = host
	}
	// One host over the ceiling. The default host is the oldest on purpose: it
	// must survive regardless, since a tab is always able to be sitting on it.
	add("local", 10*time.Hour)
	add("ocai", 3*time.Hour)
	add("norm", 2*time.Hour)
	add("wistock", 90*time.Minute)
	add("52labs", time.Hour)
	add("visiquate", 30*time.Minute)
	add("ticket500", time.Minute)

	r.evictLocked()

	if len(r.inst) != ttydWarmHosts {
		t.Fatalf("instances = %d, want %d", len(r.inst), ttydWarmHosts)
	}
	if !stopped["ocai"] {
		t.Error("the least-recently-active host was not retired")
	}
	if stopped["local"] {
		t.Error("the DEFAULT host was retired")
	}
	if _, ok := r.inst["ocai"]; ok {
		t.Error("retired host still resident")
	}
	for _, host := range []string{"local", "norm", "wistock", "52labs", "visiquate", "ticket500"} {
		if _, ok := r.inst[host]; !ok {
			t.Errorf("%s was evicted but is inside the warm window", host)
		}
	}
}

// A host that drops out of rotation is retired on idle, so a long session pays
// only for the terminals still in use. A watched host is exempt however long its
// terminal sits idle — with several tabs open, "least recently active" stops
// being a proxy for "nobody is looking at it".
func TestTtydRoleRetiresIdleInstances(t *testing.T) {
	setDefaultBackend(&localBackend{})
	t.Cleanup(func() { setDefaultBackend(nil) })
	r := newTestTtydRole(t)
	now := time.Now()
	stopped := map[string]bool{}
	add := func(host string, age time.Duration) {
		r.inst[host] = &ttydInstance{
			sock:       "/tmp/" + host + ".sock",
			cancel:     func() { stopped[host] = true },
			lastActive: now.Add(-age),
		}
		r.bySlug[hostSlug(host)] = host
	}
	add("local", 10*time.Hour) // the default host, idle far past the window
	add("ocai", ttydIdle+time.Minute)
	add("norm", ttydIdle-time.Minute)

	r.retireIdleLocked(now)

	if stopped["local"] {
		t.Error("the DEFAULT host's terminal was retired for being idle")
	}
	if !stopped["ocai"] {
		t.Error("an instance idle past ttydIdle was kept")
	}
	if _, ok := r.inst["norm"]; !ok || stopped["norm"] {
		t.Error("an instance inside ttydIdle was retired")
	}
	if len(r.inst) != 2 {
		t.Errorf("instances = %d, want local + norm", len(r.inst))
	}
}

// Every host gets its own socket path AND its own proxy path segment. Two
// aliases that sanitize to the same string must NOT collide: with per-host
// routing a collision means two hosts sharing one terminal, so an altered alias
// carries a digest of the original.
func TestHostSlugIsInjective(t *testing.T) {
	if got, want := hostSlug("ticket500"), "ticket500"; got != want {
		t.Errorf("hostSlug(%q) = %q, want it unchanged", "ticket500", got)
	}
	a, b := hostSlug("weird/alias:1"), hostSlug("weird_alias_1")
	if a == b {
		t.Fatalf("hostSlug collided on %q and %q: both %q", "weird/alias:1", "weird_alias_1", a)
	}
	for _, s := range []string{a, b} {
		if strings.ContainsAny(s, "/:. ") {
			t.Errorf("hostSlug = %q, want it safe in a URL path and a filename", s)
		}
	}
}

func TestTtydRoleSockPathIsPerHost(t *testing.T) {
	r := newTestTtydRole(t)
	local, remote := r.sockPath("local"), r.sockPath("ticket500")
	if local == remote {
		t.Fatalf("sockPath collided: %q", local)
	}
	for _, p := range []string{local, remote} {
		if !strings.Contains(p, "lasso-ttyd-") {
			t.Errorf("sockPath = %q, want the role's filename stem", p)
		}
	}
	shell := newTtydRole(context.Background(), "shell", "/shell")
	if shell.sockPath("local") == local {
		t.Error("the two roles share a socket path for one host")
	}
}

// A nil role (spawn-ttyd=false) answers the proxy without panicking.
func TestTtydRoleNilSafe(t *testing.T) {
	var r *ttydRole
	if got := r.sockForSlug("local"); got != "" {
		t.Errorf("sockForSlug on a nil role = %q, want empty", got)
	}
	if r.resident("local") {
		t.Error("a nil role reported a resident instance")
	}
	if err := r.ensure("local", "sh", nil); err != nil {
		t.Errorf("ensure on a nil role = %v, want nil", err)
	}
}
