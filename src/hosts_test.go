package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// resetHostStore clears the discovery store so each test starts cold.
func resetHostStore(t *testing.T) {
	t.Helper()
	hostStore.mu.Lock()
	hostStore.entries = nil
	hostStore.order = nil
	hostStore.sweep = nil
	hostStore.at = time.Time{}
	hostStore.mu.Unlock()
}

// drainSweep waits for any in-flight sweep to finish. Tests must call this
// before restoring probeHostFn or clearing the store: a sweep runs detached
// from the reader that triggered it, so an unblocked probe goroutine would
// otherwise publish into the NEXT test's store and make it flaky.
func drainSweep(t *testing.T) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		hostStore.mu.Lock()
		ch := hostStore.sweep
		hostStore.mu.Unlock()
		if ch == nil {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Error("in-flight sweep did not finish; store may leak into the next test")
			return
		}
	}
}

// fakeSSH puts a stub `ssh` on PATH for the duration of the test, so probeHost
// can be driven through its real exec path with scripted behaviour.
func fakeSSH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestProbeHostClassification pins down how probeHost reports each outcome. The
// timeout case is the regression that matters: a probe killed by its own
// deadline comes back as an *exec.ExitError whose ExitCode() is -1, which is
// neither "not an ExitError" nor 255 — so it used to fall through to the
// "remote ran the command" branch and be reported as Reachable with
// "luvus not installed". The switcher then offered to INSTALL luvus on a host
// lasso had never reached. The non-timeout subtests are the negative controls:
// they prove this test can still tell reachable from unreachable, i.e. that a
// pass isn't just "everything looks like a timeout".
func TestProbeHostClassification(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		timeout    time.Duration
		wantState  string
		wantReach  bool
		wantRun    bool
		maxElapsed time.Duration
	}{
		{
			name:       "timeout is not a verdict",
			script:     "sleep 30",
			timeout:    300 * time.Millisecond,
			wantState:  hostTimedOut,
			wantReach:  false, // and crucially NOT Reachable=true
			maxElapsed: 5 * time.Second,
		},
		{
			name:       "ssh connect failure is unreachable",
			script:     "echo 'ssh: connect to host x port 22: Connection timed out' >&2; exit 255",
			timeout:    5 * time.Second,
			wantState:  "",
			wantReach:  false,
			maxElapsed: 5 * time.Second,
		},
		{
			name:       "remote ran but has no luvus is reachable",
			script:     "echo 'sh: luvus: command not found' >&2; exit 127",
			timeout:    5 * time.Second,
			wantState:  "",
			wantReach:  true,
			maxElapsed: 5 * time.Second,
		},
		{
			name:       "healthy host reports running",
			script:     "printf '%s\\n' " + shellQuote(probeReplyHostInfo) + " " + shellQuote(probeReplySessionRunning) + " " + shellQuote(probeReplyCapsOK()),
			timeout:    5 * time.Second,
			wantState:  "",
			wantReach:  true,
			wantRun:    true,
			maxElapsed: 5 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeSSH(t, tc.script)
			old := hostProbeTimeout
			hostProbeTimeout = tc.timeout
			t.Cleanup(func() { hostProbeTimeout = old })

			start := time.Now()
			hi := probeHost(context.Background(), "somehost")
			elapsed := time.Since(start)

			if elapsed > tc.maxElapsed {
				t.Errorf("probe took %v, want under %v", elapsed, tc.maxElapsed)
			}
			if hi.State != tc.wantState {
				t.Errorf("State = %q, want %q (err=%q)", hi.State, tc.wantState, hi.Err)
			}
			if hi.Reachable != tc.wantReach {
				t.Errorf("Reachable = %v, want %v (err=%q)", hi.Reachable, tc.wantReach, hi.Err)
			}
			if hi.Running != tc.wantRun {
				t.Errorf("Running = %v, want %v", hi.Running, tc.wantRun)
			}
		})
	}
}

// The reply frames a probe reads back from a host's `luvus uhp proxy` calls.
// They are the real wire shapes (`type` discriminator, endpoint object, protocol
// identity) so a change in how they're parsed shows up here rather than in a
// live fleet.
const (
	probeReplyHostInfo = `{"id":"lasso","result":{"type":"host_info","version":"0.13.4","os":"linux","arch":"x86_64","config_dir":"/home/u/.luvus"}}`

	probeReplySessionRunning = `{"id":"lasso","result":{"type":"session_list","sessions":[` +
		`{"name":"default","default":true,"running":true,"endpoint":{"transport":"unix_socket","address":"/tmp/luvus.sock"},"socket_path":"/tmp/luvus.sock","session_dir":"/tmp"}]}}`

	probeReplySessionStopped = `{"id":"lasso","result":{"type":"session_list","sessions":[` +
		`{"name":"default","default":true,"running":false,"endpoint":{"transport":"unix_socket","address":"/tmp/luvus.sock"},"socket_path":"/tmp/luvus.sock","session_dir":"/tmp"}]}}`

	// A Windows host: running, but its endpoint is a named pipe, which lasso can
	// neither dial nor forward over ssh.
	probeReplySessionPipe = `{"id":"lasso","result":{"type":"session_list","sessions":[` +
		`{"name":"default","default":true,"running":true,"endpoint":{"transport":"named_pipe","address":"\\\\.\\pipe\\luvus"},"socket_path":"\\\\.\\pipe\\luvus"}]}}`
)

// probeReplyCaps renders a uhp.capabilities frame for a server offering exactly
// methods, under the named protocol identity.
func probeReplyCaps(name string, major int, methods []string) string {
	ms, err := json.Marshal(methods)
	if err != nil {
		panic(err)
	}
	return `{"id":"lasso","result":{"type":"uhp_capabilities","protocol":{"name":"` + name +
		`","major":` + strconv.Itoa(major) + `,"minor":0},"methods":` + string(ms) + `}}`
}

// probeReplyCapsOK is a server that offers everything lasso needs. It is built
// from requiredLuvusMethods rather than a frozen list, so adding a requirement
// cannot leave this fixture silently describing a server lasso would refuse.
func probeReplyCapsOK() string {
	return probeReplyCaps(luvusUHPName, 1, requiredLuvusMethods)
}

// TestApplyProbeRepliesVerdicts covers what an /api/hosts row says about a host
// lasso DID reach. Only a running server whose capabilities satisfy lasso's
// contract may be reported usable — a host listed as usable that then refuses
// every call is the failure this guards — and each refusal has to name its own
// cause, since that is the whole difference between "install it", "start it" and
// "update it" in the switcher.
func TestApplyProbeRepliesVerdicts(t *testing.T) {
	missing := requiredLuvusMethods[0]
	capsMissingOne := probeReplyCaps(luvusUHPName, 1, requiredLuvusMethods[1:])
	capsNextMajor := probeReplyCaps(luvusUHPName, 2, requiredLuvusMethods)

	cases := []struct {
		name       string
		frames     []string
		running    bool
		compatible bool
		socket     string
		protocol   string
		errHas     string
	}{
		{
			name:       "healthy host is usable",
			frames:     []string{probeReplyHostInfo, probeReplySessionRunning, probeReplyCapsOK()},
			running:    true,
			compatible: true,
			socket:     "/tmp/luvus.sock",
			protocol:   "luvus-uhp/1",
		},
		{
			name:    "stopped server is not usable",
			frames:  []string{probeReplyHostInfo, probeReplySessionStopped},
			running: false,
			socket:  "/tmp/luvus.sock",
			errHas:  "not running",
		},
		{
			name:     "a missing required method is refused by name",
			frames:   []string{probeReplyHostInfo, probeReplySessionRunning, capsMissingOne},
			running:  true,
			socket:   "/tmp/luvus.sock",
			protocol: "luvus-uhp/1",
			errHas:   missing,
		},
		{
			name:     "a newer protocol major is refused",
			frames:   []string{probeReplyHostInfo, probeReplySessionRunning, capsNextMajor},
			running:  true,
			socket:   "/tmp/luvus.sock",
			protocol: "luvus-uhp/2",
			errHas:   "protocol",
		},
		{
			name:    "an endpoint lasso cannot forward is refused",
			frames:  []string{probeReplyHostInfo, probeReplySessionPipe},
			running: true,
			errHas:  "named_pipe",
		},
		{
			name:   "no luvus at all",
			frames: []string{"bash: luvus: command not found"},
			errHas: "not installed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hi := HostInfo{Alias: "somehost", Reachable: true}
			applyProbeReplies(&hi, []byte(strings.Join(tc.frames, "\n")+"\n"))
			if hi.Running != tc.running {
				t.Errorf("Running = %v, want %v (err=%q)", hi.Running, tc.running, hi.Err)
			}
			if hi.Compatible != tc.compatible {
				t.Errorf("Compatible = %v, want %v (err=%q)", hi.Compatible, tc.compatible, hi.Err)
			}
			if hi.Socket != tc.socket {
				t.Errorf("Socket = %q, want %q", hi.Socket, tc.socket)
			}
			if hi.Protocol != tc.protocol {
				t.Errorf("Protocol = %q, want %q", hi.Protocol, tc.protocol)
			}
			if tc.errHas == "" {
				if hi.Err != "" {
					t.Errorf("Err = %q, want none", hi.Err)
				}
			} else if !strings.Contains(hi.Err, tc.errHas) {
				t.Errorf("Err = %q, want it to mention %q", hi.Err, tc.errHas)
			}
		})
	}
}

// TestApplyProbeRepliesReportsRemoteVersion: the switcher offers "update" off
// the version it reads here, so a healthy row has to carry the host's own Luvus
// version rather than an empty string.
func TestApplyProbeRepliesReportsRemoteVersion(t *testing.T) {
	hi := HostInfo{Alias: "somehost", Reachable: true}
	applyProbeReplies(&hi, []byte(strings.Join([]string{
		probeReplyHostInfo, probeReplySessionRunning, probeReplyCapsOK(),
	}, "\n")))
	if hi.Version != "0.13.4" {
		t.Errorf("Version = %q, want %q", hi.Version, "0.13.4")
	}
}

// TestProbeHostBoundedWhenChildHoldsStdout locks in the WaitDelay guard.
// cmd.Output() waits for every process holding the inherited stdout pipe, not
// just the one the context killed — and ssh forks a ControlMaster that outlives
// the client (ControlPersist), so without WaitDelay a probe can hang far past
// its deadline no matter what the context says. Measured before the fix: a
// killed command whose background child held stdout returned only when that
// CHILD exited, 60s after a 2s deadline.
func TestProbeHostBoundedWhenChildHoldsStdout(t *testing.T) {
	// The backgrounded sleep inherits stdout and outlives the killed script.
	fakeSSH(t, "sleep 30 &\nsleep 30")
	old := hostProbeTimeout
	hostProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { hostProbeTimeout = old })

	start := time.Now()
	hi := probeHost(context.Background(), "somehost")
	elapsed := time.Since(start)

	// Deadline + WaitDelay, with slack. Without WaitDelay this is ~30s.
	if limit := probeWaitDelay + 3*time.Second; elapsed > limit {
		t.Errorf("probe took %v, want under %v — Output() is waiting on a lingering child", elapsed, limit)
	}
	if hi.State != hostTimedOut {
		t.Errorf("State = %q, want %q", hi.State, hostTimedOut)
	}
}

// TestDiscoverHostsServesReadyHostsWhileOneIsSlow is the headline behaviour: a
// single slow host must not withhold every other host. Under the old
// implementation discoverHosts did wg.Wait() and returned one blob, so this
// call would have blocked for as long as the slow probe ran and returned
// nothing until then.
func TestDiscoverHostsServesReadyHostsWhileOneIsSlow(t *testing.T) {
	resetHostStore(t)

	release := make(chan struct{})
	probed := make(chan struct{}, 4)

	oldProbe, oldHosts := probeHostFn, sshConfigHostsFn
	sshConfigHostsFn = func() []string { return []string{"fast-a", "fast-b", "slowpoke"} }
	probeHostFn = func(_ context.Context, alias string) HostInfo {
		if alias == "slowpoke" {
			<-release // stands in for a sleeping laptop
		}
		probed <- struct{}{}
		return HostInfo{Alias: alias, Reachable: true, Running: true, Compatible: true}
	}
	t.Cleanup(func() {
		close(release)
		drainSweep(t)
		probeHostFn, sshConfigHostsFn = oldProbe, oldHosts
		resetHostStore(t)
	})

	start := time.Now()
	hosts, probing := discoverHostsState(context.Background(), false)
	elapsed := time.Since(start)

	// It returned on the grace, not on the slow host.
	if limit := hostProbeGrace + 3*time.Second; elapsed > limit {
		t.Fatalf("discoverHosts took %v, want ~%v — it is still waiting for the slowest host", elapsed, hostProbeGrace)
	}
	if !probing {
		t.Error("probing = false, want true — a probe is still outstanding")
	}
	if len(hosts) != 3 {
		t.Fatalf("got %d hosts, want 3 (every configured alias should be listed)", len(hosts))
	}

	byAlias := map[string]HostInfo{}
	for _, h := range hosts {
		byAlias[h.Alias] = h
	}
	// The healthy hosts are usable NOW, mid-sweep.
	for _, a := range []string{"fast-a", "fast-b"} {
		h := byAlias[a]
		if !(h.Reachable && h.Running && h.Compatible) {
			t.Errorf("%s: not usable yet (%+v) — resolved hosts must be delivered immediately", a, h)
		}
		if h.State != "" {
			t.Errorf("%s: State = %q, want empty (its probe completed)", a, h.State)
		}
	}
	// The slow host is pending, NOT reported as broken.
	slow := byAlias["slowpoke"]
	if slow.State != hostProbing {
		t.Errorf("slowpoke: State = %q, want %q", slow.State, hostProbing)
	}
	if slow.Err != "" {
		t.Errorf("slowpoke: Err = %q, want empty — a host still being probed has not failed", slow.Err)
	}

	// Correct eventually: once it answers, it shows up healthy.
	release <- struct{}{}
	deadline := time.After(5 * time.Second)
	for {
		if h, ok := findHost("slowpoke"); ok && h.State == "" && h.Reachable {
			return
		}
		select {
		case <-deadline:
			h, _ := findHost("slowpoke")
			t.Fatalf("slowpoke never became healthy: %+v", h)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestDiscoverHostsDoesNotBlockOnRefreshWhenWarm: once the store holds real
// results, a re-probe kicked in the background must not make readers wait for
// it. The pane aggregation and the repo warmer poll discovery constantly; taxing
// them with a grace period for data they already have would be the old
// all-or-nothing stall in miniature.
func TestDiscoverHostsDoesNotBlockOnRefreshWhenWarm(t *testing.T) {
	resetHostStore(t)

	block := make(chan struct{})
	slow := false
	oldProbe, oldHosts := probeHostFn, sshConfigHostsFn
	sshConfigHostsFn = func() []string { return []string{"h1"} }
	probeHostFn = func(_ context.Context, alias string) HostInfo {
		if slow {
			<-block
		}
		return HostInfo{Alias: alias, Reachable: true, Running: true, Compatible: true}
	}
	t.Cleanup(func() {
		close(block)
		drainSweep(t)
		probeHostFn, sshConfigHostsFn = oldProbe, oldHosts
		resetHostStore(t)
	})

	// First sweep settles the store.
	if hosts := discoverHosts(context.Background(), false); len(hosts) != 1 {
		t.Fatalf("first sweep returned %d hosts, want 1", len(hosts))
	}

	// Now every probe hangs, and the cache is marked stale so the next read
	// kicks a fresh sweep. The read must still come back at once.
	slow = true
	invalidateHostCache()

	start := time.Now()
	hosts := discoverHosts(context.Background(), false)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("read took %v while a background sweep was stuck; want immediate", elapsed)
	}
	if len(hosts) != 1 || !hosts[0].Reachable {
		t.Errorf("got %+v, want the previously known good row served from the store", hosts)
	}
}

// TestDiscoverHostsSharesOneSweep checks that concurrent readers don't each
// fork their own round of ssh probes.
func TestDiscoverHostsSharesOneSweep(t *testing.T) {
	resetHostStore(t)

	var calls int64
	counted := make(chan string, 32)
	oldProbe, oldHosts := probeHostFn, sshConfigHostsFn
	sshConfigHostsFn = func() []string { return []string{"h1", "h2"} }
	probeHostFn = func(_ context.Context, alias string) HostInfo {
		counted <- alias
		time.Sleep(50 * time.Millisecond)
		return HostInfo{Alias: alias, Reachable: true, Running: true, Compatible: true}
	}
	t.Cleanup(func() {
		drainSweep(t)
		probeHostFn, sshConfigHostsFn = oldProbe, oldHosts
		resetHostStore(t)
	})

	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			discoverHosts(context.Background(), false)
		}()
	}
	for i := 0; i < 5; i++ {
		<-done
	}
	close(counted)
	for range counted {
		calls++
	}
	if calls != 2 {
		t.Errorf("probed %d times across 5 concurrent readers, want 2 (one sweep of two hosts)", calls)
	}
}
