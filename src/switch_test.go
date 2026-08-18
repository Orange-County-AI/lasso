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

// A resident instance is REUSED, not respawned: that is what makes switching
// back to a host free. If activate ever reached startTtyd it would fail (empty
// PATH), and stopping the old instance would call its cancel.
func TestTtydRoleReusesResidentInstance(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r := newTestTtydRole(t)
	stopped := false
	r.inst["ticket500"] = &ttydInstance{
		sock:       "/tmp/lasso-ttyd-test-ticket500.sock",
		cancel:     func() { stopped = true },
		lastActive: time.Now().Add(-time.Hour),
	}
	r.active = "local"
	r.inst["local"] = &ttydInstance{sock: "/tmp/lasso-ttyd-test-local.sock", cancel: func() {}, lastActive: time.Now()}

	if err := r.activate("ticket500", "herdr --remote ticket500", nil); err != nil {
		t.Fatalf("activate a resident host: %v", err)
	}
	if stopped {
		t.Error("the resident instance was stopped — a switch must not respawn it")
	}
	if r.active != "ticket500" {
		t.Errorf("active = %q, want ticket500", r.active)
	}
	if got, want := r.activeSock(), "/tmp/lasso-ttyd-test-ticket500.sock"; got != want {
		t.Errorf("activeSock = %q, want %q", got, want)
	}
	if len(r.inst) != 2 {
		t.Errorf("instances = %d, want the local one kept warm too", len(r.inst))
	}
}

// A spawn that fails registers nothing, so the proxy keeps reporting "no ttyd"
// instead of dialing a path nothing is listening on.
func TestTtydRoleFailedSpawnRegistersNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no ttyd binary
	r := newTestTtydRole(t)
	if err := r.activate("local", "sh", nil); err == nil {
		t.Fatal("activate succeeded without a ttyd binary")
	}
	if len(r.inst) != 0 {
		t.Errorf("instances = %d, want 0 after a failed spawn", len(r.inst))
	}
	if got := r.activeSock(); got != "" {
		t.Errorf("activeSock = %q, want empty", got)
	}
}

// Eviction retires the least-recently-active host and never the active one.
func TestTtydRoleEvictsLeastRecentlyActive(t *testing.T) {
	r := newTestTtydRole(t)
	now := time.Now()
	stopped := map[string]bool{}
	add := func(host string, age time.Duration) {
		r.inst[host] = &ttydInstance{
			sock:       "/tmp/" + host + ".sock",
			cancel:     func() { stopped[host] = true },
			lastActive: now.Add(-age),
		}
	}
	// One host over the ceiling. The active host is the oldest on purpose: it
	// must survive regardless, since it is the socket the proxy dials.
	add("local", 10*time.Hour)
	add("ocai", 3*time.Hour)
	add("norm", 2*time.Hour)
	add("wistock", 90*time.Minute)
	add("52labs", time.Hour)
	add("visiquate", 30*time.Minute)
	add("ticket500", time.Minute)
	r.active = "local"

	r.evictLocked()

	if len(r.inst) != ttydWarmHosts {
		t.Fatalf("instances = %d, want %d", len(r.inst), ttydWarmHosts)
	}
	if !stopped["ocai"] {
		t.Error("the least-recently-active host was not retired")
	}
	if stopped["local"] {
		t.Error("the ACTIVE host was retired")
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
// only for the terminals still in use. The active one is exempt however long it
// sits idle.
func TestTtydRoleRetiresIdleInstances(t *testing.T) {
	r := newTestTtydRole(t)
	now := time.Now()
	stopped := map[string]bool{}
	add := func(host string, age time.Duration) {
		r.inst[host] = &ttydInstance{
			sock:       "/tmp/" + host + ".sock",
			cancel:     func() { stopped[host] = true },
			lastActive: now.Add(-age),
		}
	}
	add("local", 10*time.Hour) // active, idle far past the window
	add("ocai", ttydIdle+time.Minute)
	add("norm", ttydIdle-time.Minute)
	r.active = "local"

	r.retireIdleLocked(now)

	if stopped["local"] {
		t.Error("the ACTIVE terminal was retired for being idle")
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

// Every host gets its own socket path — the property that lets a switch skip the
// old single-path teardown entirely. Aliases are sanitized into the filename.
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
	if got, want := r.sockPath("weird/alias:1"), r.sockPath("weird_alias_1"); got != want {
		t.Errorf("sockPath(%q) = %q, want the sanitized %q", "weird/alias:1", got, want)
	}
	shell := newTtydRole(context.Background(), "shell", "/shell")
	if shell.sockPath("local") == local {
		t.Error("the two roles share a socket path for one host")
	}
}

// A nil role (spawn-ttyd=false) answers the proxy without panicking.
func TestTtydRoleActiveSockNilSafe(t *testing.T) {
	var r *ttydRole
	if got := r.activeSock(); got != "" {
		t.Errorf("activeSock on a nil role = %q, want empty", got)
	}
}
