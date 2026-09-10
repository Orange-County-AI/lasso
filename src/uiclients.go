package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Connected-client registry
// ---------------------------------------------------------------------------
//
// Nothing used to know which browsers were attached. client_id existed, but only
// on a WRITE (the sidebar-layout claim in uilock.go reads it off a POST body),
// so a tab sitting idle on a phone in another room was invisible — which is
// exactly the tab that causes trouble, and exactly the one a human asking "what
// else is connected?" wants named.
//
// The SSE stream is the honest signal: it is the one thing every open tab holds
// for its whole life, opened on mount and re-opened when the tab moves host. So
// registration hangs off it rather than off a heartbeat of its own.
//
// Keyed by CONNECTION, not by client id, because a tab that moves host closes
// its old EventSource and opens a new one — and the browser gives no ordering
// guarantee between the two, so keying by id would let the new registration land
// first and the stale unregister then delete the live client. Rows are grouped
// by id on the way out instead.

// clientConn is one live SSE stream.
type clientConn struct {
	id        string // the tab's client_id, "" for a client too old to send one
	host      string // resolved host this stream is watching
	userAgent string
	addr      string
	since     time.Time
}

var (
	clientsMu   sync.Mutex
	clientConns = map[*clientConn]struct{}{}
)

// registerClient records a live SSE stream and returns its unregister.
func registerClient(c *clientConn) func() {
	clientsMu.Lock()
	clientConns[c] = struct{}{}
	clientsMu.Unlock()
	return func() {
		clientsMu.Lock()
		delete(clientConns, c)
		clientsMu.Unlock()
	}
}

// clientConnected reports whether any live stream belongs to id. It is what lets
// the terminal claim (uilock.go) free a lock the moment its owner's tab goes
// away, instead of making the next human wait out the lease.
//
// An empty id is never "connected": it is the anonymous bucket, shared by every
// pre-handshake client at once, so treating it as present would hand one lock to
// all of them.
func clientConnected(id string) bool {
	if id == "" {
		return false
	}
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for c := range clientConns {
		if c.id == id {
			return true
		}
	}
	return false
}

// clientRow is one identified client in the /api/clients answer.
type clientRow struct {
	ID           string `json:"id"`
	Host         string `json:"host"`
	UserAgent    string `json:"user_agent,omitempty"`
	Addr         string `json:"addr,omitempty"`
	Streams      int    `json:"streams"` // >1 only during a host move's overlap
	ConnectedAt  string `json:"connected_at"`
	OwnsTerminal bool   `json:"owns_terminal"` // holds this host's terminal-size claim
	Self         bool   `json:"self,omitempty"`
}

type clientsResp struct {
	Clients []clientRow `json:"clients"`
	// Unidentified is how many streams came from a client too old to send a
	// client_id. They cannot be told apart from each other, so they are counted
	// rather than listed — and they are counted rather than hidden, because a
	// stale tab that predates this handshake is a likely cause of the jank
	// someone opened this list to explain.
	Unidentified int `json:"unidentified"`
}

// serveClients lists the browsers currently attached to this lasso.
func serveClients(w http.ResponseWriter, r *http.Request) {
	self := r.URL.Query().Get("client")
	now := time.Now()

	clientsMu.Lock()
	byID := map[string]*clientRow{}
	unident := 0
	for c := range clientConns {
		if c.id == "" {
			unident++
			continue
		}
		row := byID[c.id]
		if row == nil {
			row = &clientRow{
				ID:          c.id,
				Host:        c.host,
				UserAgent:   c.userAgent,
				Addr:        c.addr,
				ConnectedAt: c.since.UTC().Format(time.RFC3339),
				Self:        c.id == self,
			}
			byID[c.id] = row
		}
		row.Streams++
		// During a host move a client briefly holds two streams. Report the
		// newer one's host and connect time: that is the host it is moving TO,
		// which is what the row is about.
		if c.since.After(mustTime(row.ConnectedAt)) {
			row.Host = c.host
			row.ConnectedAt = c.since.UTC().Format(time.RFC3339)
		}
	}
	clientsMu.Unlock()

	out := clientsResp{Clients: make([]clientRow, 0, len(byID)), Unidentified: unident}
	for _, row := range byID {
		row.OwnsTerminal = termOwner(row.Host, now) == row.ID
		out.Clients = append(out.Clients, *row)
	}
	// Newest last, so the list reads as an arrival order rather than shuffling
	// on every poll (Go randomizes map iteration).
	sort.Slice(out.Clients, func(i, j int) bool {
		if out.Clients[i].ConnectedAt != out.Clients[j].ConnectedAt {
			return out.Clients[i].ConnectedAt < out.Clients[j].ConnectedAt
		}
		return out.Clients[i].ID < out.Clients[j].ID
	})
	writeJSON(w, out)
}

// mustTime parses a timestamp this file just formatted; an unparseable one sorts
// first, which only affects which of two overlapping streams labels a row.
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// serveTermClaim takes (or renews) the caller's right to resize this host's
// shared terminal. See uilock.go for why the pty needs an owner at all.
//
// A POST is the whole protocol: the browser asks only when a human actually
// acted in that tab, so there is no intent flag to send and nothing to refuse.
// The answer is who owns it afterwards, which is normally the caller — it is
// returned rather than assumed so a client with no id learns it did not get one,
// and so the taker can act on the new owner without waiting for the SSE frame.
func serveTermClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	be, err := namedHostBackend(requestHost(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	host := be.Name()
	var body struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	before := termOwner(host, time.Now())
	claimTerm(host, body.ClientID, time.Now())
	// Report the EFFECTIVE owner, not what was just recorded. A claim by a
	// client holding no SSE stream does not stand (clientConnected), and
	// answering "you own it" while every state frame says "" would have the
	// caller adopt an owner nobody else agrees with. "" reads as free, which
	// still lets a tab whose stream is mid-reconnect resize — it just doesn't
	// claim to be an owner it is not.
	owner := termOwner(host, time.Now())
	// Only a CHANGE of hands is broadcast. A renewal is the common case (the
	// owner keeps typing), and pushing a frame per keystroke to every watcher
	// would cost more than the jank this exists to fix.
	if owner != before && srvHub != nil {
		srvHub.pushTermOwner(host)
	}
	writeJSON(w, map[string]any{"host": host, "term_owner": owner})
}
