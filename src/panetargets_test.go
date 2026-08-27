package main

import "testing"

// targetHosts flattens targets to their host keys, in order.
func targetHosts(ts []paneTarget) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.host)
	}
	return out
}

func hasHost(ts []paneTarget, host string) bool {
	for _, t := range ts {
		if t.host == host {
			return true
		}
	}
	return false
}

// A pooled connection to a host whose probe SETTLED unreachable must not be
// queried: that entry is what made one asleep box cost paneHostTimeout of dead
// air on every /api/all-panes.
func TestPaneTargetsSkipsSettledUnreachablePooledHost(t *testing.T) {
	discovered := []HostInfo{
		{Alias: "norm", Reachable: true, Running: true, Compatible: true},
		// Settled negative: probe completed, verdict is "gone".
		{Alias: "gigachad", Reachable: false, Running: false, Compatible: false},
	}
	got := paneTargets("titan", discovered, []string{"gigachad"})

	if hasHost(got, "gigachad") {
		t.Errorf("settled-unreachable pooled host was queried: %v", targetHosts(got))
	}
	if !hasHost(got, "norm") {
		t.Errorf("reachable host missing: %v", targetHosts(got))
	}
	if !hasHost(got, "local") {
		t.Errorf("local must always be included: %v", targetHosts(got))
	}
}

// The pool fallback still has to work for its actual purpose: a probe that has
// NOT settled (still in flight, or timed out) is weaker evidence than a live
// pooled socket, so the host is queried anyway.
func TestPaneTargetsKeepsPooledHostWhenProbeUnsettled(t *testing.T) {
	for _, state := range []string{hostProbing, hostTimedOut} {
		discovered := []HostInfo{
			{Alias: "norm", Reachable: false, Running: false, Compatible: false, State: state},
		}
		got := paneTargets("titan", discovered, []string{"norm"})
		if !hasHost(got, "norm") {
			t.Errorf("state %q: pooled host with unsettled probe should still be queried, got %v",
				state, targetHosts(got))
		}
	}
}

// A pooled host discovery has never seen at all has no verdict to defer to, so
// the live connection is all the evidence there is.
func TestPaneTargetsKeepsPooledHostUnknownToDiscovery(t *testing.T) {
	got := paneTargets("titan", nil, []string{"adhoc"})
	if !hasHost(got, "adhoc") {
		t.Errorf("pooled host unknown to discovery should be queried, got %v", targetHosts(got))
	}
}

// A reachable host that is also pooled must appear exactly once.
func TestPaneTargetsNoDuplicates(t *testing.T) {
	discovered := []HostInfo{{Alias: "norm", Reachable: true, Running: true, Compatible: true}}
	got := paneTargets("titan", discovered, []string{"norm", "norm"})
	n := 0
	for _, h := range targetHosts(got) {
		if h == "norm" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("norm appeared %d times, want 1: %v", n, targetHosts(got))
	}
}

// The whole fleet must fit in one wave, or a stalled host postpones the hosts
// queued behind it and one dead box costs a MULTIPLE of paneHostTimeout.
func TestPaneHostConcurrencyCoversFleet(t *testing.T) {
	if paneHostConcurrency < hostProbeConcurrency {
		t.Errorf("paneHostConcurrency (%d) < hostProbeConcurrency (%d): discovery can surface "+
			"more hosts than the aggregation will query at once",
			paneHostConcurrency, hostProbeConcurrency)
	}
}
