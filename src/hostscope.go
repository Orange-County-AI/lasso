package main

import "fmt"

// Host scope — which hosts an agent driving lasso may see and act on.
//
// The rule: an agent can only see and communicate with agents on its own host,
// or on a host that has an alias in the ssh config lasso reads and can
// therefore be driven through lasso. Nothing else is addressable.
//
// That set is NOT the same as the set of hosts in lasso's agents db. The db is
// a historical record: every agent lasso ever created, tagged with the host it
// ran on. The reachable set is the local box plus the concrete aliases in
// ~/.ssh/config (exactly what list_hosts enumerates). The two drift apart the
// moment an alias is renamed, removed, or was only ever known to some other
// lasso — and the cross-host paths that resolve targets from the db
// (message_agent's recipients, whoami/close_agent with no host) used to span
// the db's hosts. So an agent on a machine this lasso can no longer connect to
// stayed visible and addressable while every call against it was doomed: the
// listing promised a peer that no send, read, or close could ever reach.
//
// Membership below therefore comes from the ssh config, never from the db.

// addressableHosts is the set of host names lasso may resolve an agent on: the
// local box plus every concrete alias in the ssh config.
//
// Reachability deliberately does NOT enter into it. An alias whose machine is
// asleep is still the right target — the caller gets an honest "host
// unreachable" from the backend a moment later, which beats the agent silently
// vanishing from the listing every time its box takes a nap. Only a host with
// no alias at all is invisible, because that is a permanent property of the
// config rather than a transient property of the network.
func addressableHosts() map[string]bool {
	set := map[string]bool{"local": true}
	for _, alias := range sshConfigHostsFn() {
		set[alias] = true
	}
	return set
}

// hostAddressable reports whether one host is in that set. The empty host means
// the local box (the tools' default), so it always is.
func hostAddressable(host string) bool {
	if isLocalHost(host) {
		return true
	}
	return addressableHosts()[host]
}

// requireAddressableHost is the guard a host-scoped call runs before answering
// from the db, so a host lasso cannot connect to is refused with the reason
// rather than half-answered from records nothing can act on.
func requireAddressableHost(host string) error {
	if hostAddressable(host) {
		return nil
	}
	return fmt.Errorf("host %q is not addressable from this lasso: an agent can only see and message agents on its own host, or on a host with an alias in lasso's ssh config — call list_hosts for the ones it can drive", host)
}

// addressableAgents drops the records whose host is not addressable, so a
// cross-host resolution can only ever land on an agent lasso could actually
// reach. Order is preserved (listAllAgents returns oldest-first, which the
// callers rely on).
func addressableAgents(all []hostAgent) []hostAgent {
	set := addressableHosts()
	out := make([]hostAgent, 0, len(all))
	for _, ha := range all {
		if isLocalHost(ha.Host) || set[ha.Host] {
			out = append(out, ha)
		}
	}
	return out
}
