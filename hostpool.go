package main

import (
	"fmt"
	"sync"
	"time"
)

// The host backend pool keeps one live SSH control-master connection per remote
// host, lazily created on first use and idle-reaped. It's how lasso drives tmux /
// files / git on ANY host (not just the active one): the status poller, the grid
// aggregator, and the MCP tools all resolve a host name to a Backend through here.
//
// The active host (curBackend(), set by serveHostSwitch) is a pooled backend too,
// so file/diff handlers and per-host tmux share the same connection. The reaper
// never closes the active host's entry.

// hostPoolIdle is how long a remote backend may sit unused before the reaper
// tears down its control master. Generous because a viewport attached to a
// remote host relies on the master staying up between user actions.
const hostPoolIdle = 3 * time.Minute

var hostPool struct {
	mu      sync.Mutex
	entries map[string]*hostPoolEntry // keyed by ssh alias
	reaper  bool
}

type hostPoolEntry struct {
	backend  *remoteBackend
	lastUsed time.Time
}

// isLocalHost reports whether a host name refers to the machine lasso runs on:
// "", "local", or this box's short hostname (so the local host's own alias maps
// to the fast local backend rather than ssh-to-self).
func isLocalHost(host string) bool {
	switch host {
	case "", "local", localHostname():
		return true
	}
	return false
}

// resolveBackend maps a tool/handler's optional `host` argument to a Backend.
// Empty / "local" / this machine's hostname → the local backend; any other name
// is treated as an ssh-config alias and resolved to a pooled remote backend.
func resolveBackend(host string) (Backend, error) { return hostBackend(host) }

// hostBackend returns the Backend for host, creating + caching a remote
// connection on first use. Touches lastUsed so the reaper keeps an in-use host
// alive.
func hostBackend(host string) (Backend, error) {
	if isLocalHost(host) {
		return localBE, nil
	}
	hostPool.mu.Lock()
	defer hostPool.mu.Unlock()
	if hostPool.entries == nil {
		hostPool.entries = map[string]*hostPoolEntry{}
	}
	if e := hostPool.entries[host]; e != nil && e.backend != nil {
		e.lastUsed = nowFunc()
		return e.backend, nil
	}
	be, err := newRemoteBackend(srvCtx, host)
	if err != nil {
		return nil, err
	}
	hostPool.entries[host] = &hostPoolEntry{backend: be, lastUsed: nowFunc()}
	if !hostPool.reaper {
		hostPool.reaper = true
		go hostPoolReaper()
	}
	return be, nil
}

// dropHostBackend tears down and forgets a host's pooled connection (e.g. after a
// connection error so the next use reconnects fresh).
func dropHostBackend(host string) {
	hostPool.mu.Lock()
	e := hostPool.entries[host]
	delete(hostPool.entries, host)
	hostPool.mu.Unlock()
	if e != nil && e.backend != nil {
		_ = e.backend.Close()
	}
}

// hostPoolReaper closes remote backends idle longer than hostPoolIdle, except the
// active host (its connection must persist while it's selected).
func hostPoolReaper() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-srvCtx.Done():
			return
		case <-t.C:
		}
		active := curBackend().Name()
		var dead []*remoteBackend
		hostPool.mu.Lock()
		for host, e := range hostPool.entries {
			if host == active {
				continue
			}
			if time.Since(e.lastUsed) > hostPoolIdle {
				dead = append(dead, e.backend)
				delete(hostPool.entries, host)
			}
		}
		hostPool.mu.Unlock()
		for _, b := range dead {
			_ = b.Close()
		}
	}
}

// hostBackendOrErr is hostBackend with the error folded into a descriptive
// message, for call sites that just want a Backend or a reason it's unreachable.
func hostBackendOrErr(host string) (Backend, error) {
	be, err := hostBackend(host)
	if err != nil {
		return nil, fmt.Errorf("host %q unreachable: %w", host, err)
	}
	return be, nil
}

// nowFunc is time.Now, indirected only so the pool stays testable.
func nowFunc() time.Time { return time.Now() }
