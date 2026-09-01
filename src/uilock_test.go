package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// postUIState drives the real handler, which is where the claim and the merge
// meet — the arbitration is only correct if a refusal still lets the rest of
// the patch through.
func postUIState(t *testing.T, body string) uiStateResp {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/ui-state", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	serveUIState(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/ui-state = %d: %s", w.Code, w.Body.String())
	}
	var resp uiStateResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// The bug this exists for: a client that did not touch its sidebar re-persists
// the layout it happens to be rendering (a mount, a window resize, applying
// somebody else's change) and reopens a sidebar the human just collapsed. The
// owner's collapse must survive any number of those.
func TestRogueEchoCannotReopenSidebar(t *testing.T) {
	openTestDB(t)
	releaseLayoutOwner()
	t.Cleanup(releaseLayoutOwner)

	// The human collapses it in tab A.
	got := postUIState(t, `{"sidebar_collapsed":true,"client_id":"A","user_intent":true}`)
	if !got.SidebarCollapsed || got.LayoutDenied {
		t.Fatalf("owner's collapse not stored: %+v", got)
	}

	// Tab B's panel group insists it is open, repeatedly, with nobody behind it.
	for i := 0; i < 3; i++ {
		got = postUIState(t, `{"sidebar_collapsed":false,"sidebar_pct":40,"client_id":"B","user_intent":false}`)
		if !got.LayoutDenied {
			t.Fatalf("echo %d was not refused", i)
		}
		if !got.SidebarCollapsed {
			t.Fatalf("echo %d reopened the sidebar: %+v", i, got)
		}
	}

	// And the stored state agrees — a refusal must not have been cosmetic.
	stored, err := getUIState()
	if err != nil {
		t.Fatalf("getUIState: %v", err)
	}
	if !stored.SidebarCollapsed {
		t.Fatal("stored state reopened by a refused write")
	}
}

// The point of the lock is that a human always wins it. A rogue client holding
// the lock must not be able to keep the human who takes over from collapsing.
func TestUserIntentTakesTheLock(t *testing.T) {
	openTestDB(t)
	releaseLayoutOwner()
	t.Cleanup(releaseLayoutOwner)

	postUIState(t, `{"sidebar_pct":40,"client_id":"A","user_intent":true}`)
	got := postUIState(t, `{"sidebar_collapsed":true,"client_id":"B","user_intent":true}`)
	if got.LayoutDenied {
		t.Fatal("a human's change was refused")
	}
	if !got.SidebarCollapsed {
		t.Fatalf("B's collapse did not land: %+v", got)
	}
	// B now owns it, so A's unattended echo loses.
	got = postUIState(t, `{"sidebar_collapsed":false,"client_id":"A","user_intent":false}`)
	if !got.LayoutDenied || got.SidebarCollapsed != true {
		t.Fatalf("previous owner's echo won after losing the lock: %+v", got)
	}
}

// A refusal is scoped to the sidebar. Everything else in the patch is a change
// the human made in THIS client and must still land.
func TestRefusalDropsOnlyTheLayout(t *testing.T) {
	openTestDB(t)
	releaseLayoutOwner()
	t.Cleanup(releaseLayoutOwner)

	postUIState(t, `{"sidebar_collapsed":true,"client_id":"A","user_intent":true}`)
	got := postUIState(t, `{"sidebar_collapsed":false,"usage_compact":true,"client_id":"B","user_intent":false}`)
	if !got.LayoutDenied {
		t.Fatal("B's layout write was not refused")
	}
	if !got.SidebarCollapsed {
		t.Fatal("refused write moved the sidebar")
	}
	if !got.UsageCompact {
		t.Fatal("refusal swallowed an unrelated preference")
	}
}

// A patch that never mentions the sidebar is not arbitrated at all — otherwise
// toggling the usage footer from a second tab would depend on who holds a lock
// that has nothing to do with it.
func TestNonLayoutPatchIsNotArbitrated(t *testing.T) {
	openTestDB(t)
	releaseLayoutOwner()
	t.Cleanup(releaseLayoutOwner)

	postUIState(t, `{"sidebar_collapsed":true,"client_id":"A","user_intent":true}`)
	got := postUIState(t, `{"files_click_navigates":false,"client_id":"B","user_intent":false}`)
	if got.LayoutDenied {
		t.Fatal("a patch that never named the layout was arbitrated")
	}
	if got.FilesClickNavigates {
		t.Fatal("unrelated preference did not land")
	}
	if !got.SidebarCollapsed {
		t.Fatal("unrelated patch disturbed the layout")
	}
}

func TestClaimLayout(t *testing.T) {
	t0 := time.Now()
	cases := []struct {
		name    string
		owner   layoutClaim
		client  string
		intent  bool
		now     time.Time
		want    bool
		wantOwn string
	}{
		{
			name: "a human's change takes a lock nobody holds",
			now:  t0, client: "A", intent: true, want: true, wantOwn: "A",
		},
		{
			name:  "a human's change takes it from a live owner",
			owner: layoutClaim{"A", t0}, client: "B", intent: true, now: t0.Add(time.Second),
			want: true, wantOwn: "B",
		},
		{
			name:  "the owner settles its own change within the lease",
			owner: layoutClaim{"A", t0}, client: "A", now: t0.Add(time.Second),
			want: true, wantOwn: "A",
		},
		{
			name:  "an unattended echo from a non-owner is refused",
			owner: layoutClaim{"A", t0}, client: "B", now: t0.Add(time.Second),
			want: false, wantOwn: "A",
		},
		{
			// The regression that matters: an idle lease must not promote an
			// unattended echo into an authority. "Nobody has touched a tab in a
			// while" is precisely the state the rogue client writes in.
			name:  "a free lock does not let an unattended echo move it",
			owner: layoutClaim{"A", t0}, client: "B", now: t0.Add(layoutLease + time.Second),
			want: false, wantOwn: "A",
		},
		{
			// Same rule applied to the holder: an idle tab's panel group is not
			// evidence of what anyone wants either.
			name:  "a stale owner's own echo is refused",
			owner: layoutClaim{"A", t0}, client: "A", now: t0.Add(layoutLease + time.Second),
			want: false, wantOwn: "A",
		},
		{
			name:  "a human on the stale owner's tab reclaims it",
			owner: layoutClaim{"A", t0}, client: "A", intent: true, now: t0.Add(layoutLease * 10),
			want: true, wantOwn: "A",
		},
		{
			// A tab built before this handshake sends no id, so its writes are
			// indistinguishable from the unattended ones. It never moves the
			// sidebar for anyone else, whatever it claims.
			name:  "an unidentified client never moves it",
			owner: layoutClaim{}, client: "", intent: true, now: t0,
			want: false, wantOwn: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			layoutOwner = c.owner
			t.Cleanup(releaseLayoutOwner)
			if got := claimLayout(c.client, c.intent, c.now); got != c.want {
				t.Errorf("claimLayout = %v, want %v", got, c.want)
			}
			if layoutOwner.client != c.wantOwn {
				t.Errorf("owner = %q, want %q", layoutOwner.client, c.wantOwn)
			}
			if c.want && !layoutOwner.at.Equal(c.now) {
				t.Errorf("claim not stamped at the write: %v, want %v", layoutOwner.at, c.now)
			}
		})
	}
}

// A patch restating the layout it already agrees with is asking for nothing, so
// it must not come back refused — a refusal makes the client snap its panel to
// a value it is already rendering, and would fire on every routine echo.
func TestUnchangedLayoutIsNotRefused(t *testing.T) {
	openTestDB(t)
	releaseLayoutOwner()
	t.Cleanup(releaseLayoutOwner)

	postUIState(t, `{"sidebar_collapsed":true,"sidebar_pct":40,"client_id":"A","user_intent":true}`)
	got := postUIState(t, `{"sidebar_collapsed":true,"sidebar_pct":40,"client_id":"B","user_intent":false}`)
	if got.LayoutDenied {
		t.Fatal("an echo that changes nothing was refused")
	}
}
