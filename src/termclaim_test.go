package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// connect registers a live SSE stream for id, as serveSSE does, and unregisters
// it at the end of the test. The claim consults this: an owner whose tab is gone
// holds nothing.
func connect(t *testing.T, id, host string) func() {
	t.Helper()
	off := registerClient(&clientConn{id: id, host: host, since: time.Now()})
	t.Cleanup(off)
	return off
}

// The jank this exists for: a background tab reflows (a reload, a phone waking,
// an OS window resize) and clamps the width of the terminal the human is
// actually working in. The owner must keep the pty across any number of those.
func TestBackgroundTabCannotTakeTerminalFromOwner(t *testing.T) {
	releaseTermOwners()
	t.Cleanup(releaseTermOwners)
	connect(t, "desk", "titan")
	connect(t, "phone", "titan")

	now := time.Now()
	if got := claimTerm("titan", "desk", now); got != "desk" {
		t.Fatalf("desk did not take a free claim: %q", got)
	}
	// The phone never asks (it only reflows), so the owner stands. This is the
	// whole contract the browser gate reads.
	for i := 0; i < 3; i++ {
		if got := termOwner("titan", now.Add(time.Duration(i)*time.Second)); got != "desk" {
			t.Fatalf("owner moved without anyone asking: %q", got)
		}
	}
}

// The other half: a human acting in another tab takes it outright. "Most
// recently active client wins" is the feature, not a race to be avoided.
func TestActingClientTakesTerminal(t *testing.T) {
	releaseTermOwners()
	t.Cleanup(releaseTermOwners)
	connect(t, "desk", "titan")
	connect(t, "phone", "titan")

	now := time.Now()
	claimTerm("titan", "desk", now)
	if got := claimTerm("titan", "phone", now.Add(time.Second)); got != "phone" {
		t.Fatalf("phone did not take the claim: %q", got)
	}
	if got := termOwner("titan", now.Add(2*time.Second)); got != "phone" {
		t.Fatalf("owner = %q, want phone", got)
	}
}

// A free claim is grantable — the deliberate difference from the sidebar, which
// refuses an unheld lock. A lone tab on a quiet host must be able to size its
// own terminal, or herdr sits at the pty default forever.
func TestFreeTerminalClaimIsGrantable(t *testing.T) {
	releaseTermOwners()
	t.Cleanup(releaseTermOwners)
	connect(t, "solo", "titan")

	if got := termOwner("titan", time.Now()); got != "" {
		t.Fatalf("nobody has claimed anything, owner = %q", got)
	}
	if got := claimTerm("titan", "solo", time.Now()); got != "solo" {
		t.Fatalf("solo did not take the free claim: %q", got)
	}
}

// An owner that walks away frees the pty at once rather than making the next
// human wait out the lease. This is why the claim consults the client registry
// at all instead of being a bare timestamp.
func TestClosedTabFreesTerminalClaim(t *testing.T) {
	releaseTermOwners()
	t.Cleanup(releaseTermOwners)
	off := connect(t, "desk", "titan")
	connect(t, "phone", "titan")

	now := time.Now()
	claimTerm("titan", "desk", now)
	if got := termOwner("titan", now); got != "desk" {
		t.Fatalf("owner = %q, want desk", got)
	}
	off() // the desktop tab closes; its SSE stream ends
	if got := termOwner("titan", now); got != "" {
		t.Fatalf("a closed tab still holds the claim: %q", got)
	}
}

// The lease is the backstop for a tab that stopped reporting without
// disconnecting (a wedged renderer, a laptop asleep with its socket still open).
func TestTerminalClaimExpires(t *testing.T) {
	releaseTermOwners()
	t.Cleanup(releaseTermOwners)
	connect(t, "desk", "titan")

	now := time.Now()
	claimTerm("titan", "desk", now)
	if got := termOwner("titan", now.Add(termLease-time.Second)); got != "desk" {
		t.Fatalf("claim expired inside its lease: %q", got)
	}
	if got := termOwner("titan", now.Add(termLease+time.Second)); got != "" {
		t.Fatalf("claim outlived its lease: %q", got)
	}
}

// Claims are per host because the pty is: ttyd runs one instance per host, so
// two tabs on two machines are not in contention at all.
func TestTerminalClaimIsPerHost(t *testing.T) {
	releaseTermOwners()
	t.Cleanup(releaseTermOwners)
	connect(t, "desk", "titan")
	connect(t, "phone", "norm")

	now := time.Now()
	claimTerm("titan", "desk", now)
	claimTerm("norm", "phone", now)
	if got := termOwner("titan", now); got != "desk" {
		t.Fatalf("titan owner = %q, want desk", got)
	}
	if got := termOwner("norm", now); got != "phone" {
		t.Fatalf("norm owner = %q, want phone", got)
	}
}

// A client too old to send an id gets no claim. It does not consult one either
// (it predates the gate), so this only has to avoid handing one lock to the
// whole anonymous bucket at once.
func TestAnonymousClientGetsNoTerminalClaim(t *testing.T) {
	releaseTermOwners()
	t.Cleanup(releaseTermOwners)
	connect(t, "desk", "titan")

	now := time.Now()
	claimTerm("titan", "desk", now)
	if got := claimTerm("titan", "", now.Add(time.Second)); got != "desk" {
		t.Fatalf("an anonymous claim disturbed the owner: %q", got)
	}
	releaseTermOwners()
	if got := claimTerm("titan", "", now); got != "" {
		t.Fatalf("an anonymous client took a free claim: %q", got)
	}
}

// ---------------------------------------------------------------------------
// the client registry
// ---------------------------------------------------------------------------

func getClients(t *testing.T, self string) clientsResp {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/clients?client="+self, nil)
	w := httptest.NewRecorder()
	serveClients(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/clients = %d: %s", w.Code, w.Body.String())
	}
	var resp clientsResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestClientsListsConnectedTabs(t *testing.T) {
	releaseTermOwners()
	t.Cleanup(releaseTermOwners)
	connect(t, "desk", "titan")
	connect(t, "phone", "norm")
	claimTerm("titan", "desk", time.Now())

	got := getClients(t, "desk")
	if len(got.Clients) != 2 {
		t.Fatalf("got %d clients, want 2: %+v", len(got.Clients), got.Clients)
	}
	byID := map[string]clientRow{}
	for _, c := range got.Clients {
		byID[c.ID] = c
	}
	if !byID["desk"].Self {
		t.Errorf("the caller's own row is not marked self: %+v", byID["desk"])
	}
	if byID["phone"].Self {
		t.Errorf("another tab's row is marked self: %+v", byID["phone"])
	}
	if !byID["desk"].OwnsTerminal {
		t.Errorf("the terminal owner's row does not say so: %+v", byID["desk"])
	}
	// The phone is on norm, whose claim nobody holds — it must not inherit
	// titan's answer.
	if byID["phone"].OwnsTerminal {
		t.Errorf("a client on another host reads as owning the terminal: %+v", byID["phone"])
	}
}

// A tab moving host briefly holds two streams (the new EventSource opens before
// the old one's close lands). That is ONE client, not two — keying the registry
// by connection is what makes the grouping able to say so.
func TestHostMoveIsOneClient(t *testing.T) {
	connect(t, "desk", "titan")
	connect(t, "desk", "norm")

	got := getClients(t, "desk")
	if len(got.Clients) != 1 {
		t.Fatalf("a host move split one client into %d rows: %+v", len(got.Clients), got.Clients)
	}
	if got.Clients[0].Streams != 2 {
		t.Errorf("streams = %d, want 2", got.Clients[0].Streams)
	}
}

// Pre-handshake clients are counted rather than listed: they cannot be told
// apart, and hiding them would omit a likely cause of the jank someone opened
// this list to explain.
func TestUnidentifiedClientsAreCounted(t *testing.T) {
	connect(t, "", "titan")
	connect(t, "", "titan")
	connect(t, "desk", "titan")

	got := getClients(t, "desk")
	if got.Unidentified != 2 {
		t.Errorf("unidentified = %d, want 2", got.Unidentified)
	}
	if len(got.Clients) != 1 {
		t.Errorf("got %d listed clients, want 1: %+v", len(got.Clients), got.Clients)
	}
}
