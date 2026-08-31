package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func blockedTestPane(host, paneID, status string) hostPane {
	return hostPane{
		Host:           host,
		HostLabel:      host,
		PaneID:         paneID,
		WorkspaceID:    "ws-" + paneID,
		WorkspaceLabel: "Fix the push flow",
		Agent:          "claude",
		AgentStatus:    status,
		HasAgent:       true,
	}
}

func TestObserveNotifiesOnTheTransitionIntoBlocked(t *testing.T) {
	w := &blockedWatcher{}
	now := time.Now()

	if got := w.observe([]hostPane{blockedTestPane("local", "p1", "working")}, now); len(got) != 0 {
		t.Fatalf("a working agent must say nothing: %+v", got)
	}
	got := w.observe([]hostPane{blockedTestPane("local", "p1", "blocked")}, now.Add(10*time.Second))
	if len(got) != 1 {
		t.Fatalf("want 1 notification on the transition, got %d", len(got))
	}
	if got[0].Kind != notifAgentBlocked {
		t.Errorf("kind = %q", got[0].Kind)
	}
	if got[0].Title != "Fix the push flow" {
		t.Errorf("title = %q, want the workspace label", got[0].Title)
	}
	if !strings.Contains(got[0].Body, "claude") || !strings.Contains(got[0].Body, "local") {
		t.Errorf("body = %q, want the harness and the host named", got[0].Body)
	}
	if got[0].Host != "local" {
		t.Errorf("host = %q", got[0].Host)
	}
	if got[0].Tag == "" {
		t.Error("a tag is required so a re-notify replaces rather than stacks")
	}

	// Still blocked is not news — an agent parked on an approval for an hour is
	// one notification, not one per poll.
	for i := range 4 {
		at := now.Add(time.Duration(20+i*10) * time.Second)
		if extra := w.observe([]hostPane{blockedTestPane("local", "p1", "blocked")}, at); len(extra) != 0 {
			t.Fatalf("steady blocked re-notified at poll %d: %+v", i, extra)
		}
	}
}

func TestObserveRateLimitsAFlappingAgent(t *testing.T) {
	w := &blockedWatcher{}
	now := time.Now()
	if got := w.observe([]hostPane{blockedTestPane("local", "p1", "blocked")}, now); len(got) != 1 {
		t.Fatalf("first block: %d notifications", len(got))
	}
	// Approve, and the agent immediately asks for the next thing. That is one
	// buzz per notifyRenotify window, not one per approval.
	w.observe([]hostPane{blockedTestPane("local", "p1", "working")}, now.Add(time.Second))
	if got := w.observe([]hostPane{blockedTestPane("local", "p1", "blocked")}, now.Add(2*time.Second)); len(got) != 0 {
		t.Fatalf("re-block inside the window notified again: %+v", got)
	}
	// Past the window it is news again.
	after := now.Add(notifyRenotify + time.Second)
	w.observe([]hostPane{blockedTestPane("local", "p1", "working")}, after)
	if got := w.observe([]hostPane{blockedTestPane("local", "p1", "blocked")}, after.Add(time.Second)); len(got) != 1 {
		t.Fatalf("re-block after the window: %d notifications, want 1", len(got))
	}
}

// A blocked agent lasso has never seen before IS news, even on the first poll.
// Baselining silently would mean an agent that blocked while lasso restarted —
// or while notifications were being switched on — is never announced at all.
func TestObserveNotifiesAnAlreadyBlockedAgentOnTheFirstPoll(t *testing.T) {
	w := &blockedWatcher{}
	if got := w.observe([]hostPane{blockedTestPane("titan", "p9", "blocked")}, time.Now()); len(got) != 1 {
		t.Fatalf("want 1 notification, got %d", len(got))
	}
}

func TestObserveIgnoresPanesWithoutAnAgent(t *testing.T) {
	w := &blockedWatcher{}
	shell := hostPane{Host: "local", PaneID: "p1", AgentStatus: "blocked"} // HasAgent false
	if got := w.observe([]hostPane{shell}, time.Now()); len(got) != 0 {
		t.Fatalf("a bare shell must never notify: %+v", got)
	}
}

// A pane whose agent exits leaves a bare shell. When an agent lands in that pane
// again and is blocked, that is a real transition and must notify — the slot
// must not still be holding the previous agent's "blocked".
func TestObserveTreatsAReusedPaneAsFresh(t *testing.T) {
	w := &blockedWatcher{}
	now := time.Now()
	if got := w.observe([]hostPane{blockedTestPane("local", "p1", "blocked")}, now); len(got) != 1 {
		t.Fatalf("first block: %d", len(got))
	}
	shell := hostPane{Host: "local", PaneID: "p1"}
	w.observe([]hostPane{shell}, now.Add(notifyRenotify+time.Second))
	got := w.observe([]hostPane{blockedTestPane("local", "p1", "blocked")}, now.Add(notifyRenotify+2*time.Second))
	if len(got) != 1 {
		t.Fatalf("a new agent blocking in a reused pane: %d notifications, want 1", len(got))
	}
}

// herdr-mirror makes one blocked agent appear twice in the fleet listing: once
// as the remote host's own pane, once as the local pane streaming it. One agent
// is one notification, attributed to the machine the work is on.
func TestObserveDedupesMirroredPanes(t *testing.T) {
	direct := blockedTestPane("norm", "remote-1", "blocked")
	mirror := blockedTestPane("titan", "local-7", "blocked")
	mirror.MirrorHost, mirror.MirrorPane = "norm", "remote-1"
	mirror.MirrorLabel, mirror.WorkspaceLabel = "Fix the push flow", "norm: Fix the push flow"

	for _, order := range [][]hostPane{{direct, mirror}, {mirror, direct}} {
		w := &blockedWatcher{}
		got := w.observe(order, time.Now())
		if len(got) != 1 {
			t.Fatalf("want 1 notification, got %d", len(got))
		}
		if got[0].Host != "norm" {
			t.Errorf("host = %q, want the machine the agent is on", got[0].Host)
		}
	}
}

// A mirror of a host lasso cannot reach directly is the only view of that
// agent, so it must still notify — attributed to the remote host.
func TestObserveNotifiesForAMirrorOnlyAgent(t *testing.T) {
	mirror := blockedTestPane("titan", "local-7", "blocked")
	mirror.MirrorHost, mirror.MirrorPane = "blackbird", "remote-2"
	mirror.WorkspaceLabel = "blackbird: Ship the thing"
	mirror.MirrorLabel = "Ship the thing"
	got := (&blockedWatcher{}).observe([]hostPane{mirror}, time.Now())
	if len(got) != 1 {
		t.Fatalf("want 1 notification, got %d", len(got))
	}
	if got[0].Host != "blackbird" || !strings.Contains(got[0].Body, "blackbird") {
		t.Errorf("notification = %+v, want it attributed to blackbird", got[0])
	}
}

// The aggregation serves a failed host's last-good panes, so an unreachable
// host re-states panes the watcher already recorded. That must not read as a
// fresh block.
func TestObserveIsQuietOnRepeatedStalePanes(t *testing.T) {
	w := &blockedWatcher{}
	now := time.Now()
	panes := []hostPane{blockedTestPane("norm", "p1", "blocked")}
	if got := w.observe(panes, now); len(got) != 1 {
		t.Fatalf("first sighting: %d", len(got))
	}
	for i := range 3 {
		if got := w.observe(panes, now.Add(time.Duration(i+1)*notifyPollEvery)); len(got) != 0 {
			t.Fatalf("stale listing notified again at poll %d: %+v", i, got)
		}
	}
}

func TestObserveForgetsPanesThatStayGone(t *testing.T) {
	w := &blockedWatcher{}
	now := time.Now()
	w.observe([]hostPane{blockedTestPane("local", "p1", "working")}, now)
	w.observe([]hostPane{blockedTestPane("local", "p2", "working")}, now.Add(notifyForget+time.Minute))
	w.mu.Lock()
	_, stillThere := w.seen["local\x00p1"]
	n := len(w.seen)
	w.mu.Unlock()
	if stillThere || n != 1 {
		t.Errorf("closed pane not forgotten: %d entries remembered", n)
	}
}

func TestBlockedNotificationFallsBackThroughTheAvailableNames(t *testing.T) {
	p := hostPane{
		Host:          "titan",
		HostLabel:     "titan",
		PaneID:        "p1",
		TerminalTitle: "Reticulating splines",
		Agent:         "codex",
		AgentStatus:   "blocked",
		HasAgent:      true,
	}
	n := blockedNotification(p, "titan\x00p1")
	if n.Title != "Reticulating splines" {
		t.Errorf("title = %q, want the terminal title when there is no workspace label", n.Title)
	}
	// The terminal title is already the headline, so it must not be repeated in
	// the body.
	if strings.Count(n.Body, "Reticulating splines") != 0 {
		t.Errorf("body repeats the title: %q", n.Body)
	}
	if !strings.Contains(n.Body, "codex") {
		t.Errorf("body = %q, want the harness named", n.Body)
	}

	// A named workspace plus a distinct task line: both are useful, so the task
	// rides in the body.
	p.WorkspaceLabel = "Push notifications"
	n = blockedNotification(p, "titan\x00p1")
	if n.Title != "Push notifications" || !strings.Contains(n.Body, "Reticulating splines") {
		t.Errorf("notification = %+v", n)
	}

	// Nothing to name it by at all still produces a usable notification.
	bare := hostPane{Host: "local", PaneID: "p2", HasAgent: true, AgentStatus: "blocked"}
	n = blockedNotification(bare, "local\x00p2")
	if n.Title == "" || n.Body == "" {
		t.Errorf("unnamed agent produced %+v", n)
	}
}

func TestClipNotifyTextCutsOnRuneBoundaries(t *testing.T) {
	if got := clipNotifyText("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 40)
	got := clipNotifyText(long, 10)
	if r := []rune(got); len(r) != 10 || r[9] != '…' {
		t.Errorf("clip = %q (%d runes)", got, len([]rune(got)))
	}
	if !strings.HasPrefix(got, "ééé") {
		t.Errorf("multi-byte runes were cut mid-character: %q", got)
	}
}

// fakeTransport captures what the pipeline delivers.
type fakeTransport struct {
	mu   sync.Mutex
	got  []notification
	live bool
}

func (f *fakeTransport) name() string { return "fake" }
func (f *fakeTransport) active() bool { return f.live }
func (f *fakeTransport) deliver(_ context.Context, n notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, n)
	return nil
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

// The loop itself: a blocked agent in the fleet aggregation has to come out the
// other end as a delivered notification. observe() and the transport are tested
// separately; this is the wiring between them, including the notifyEnabled gate
// that decides whether a host is polled at all.
func TestBlockedWatcherDeliversThroughTheTransport(t *testing.T) {
	notifyPollEvery = 5 * time.Millisecond
	t.Cleanup(func() { notifyPollEvery = 10 * time.Second })

	fake := &fakeTransport{}
	useNotifTransport(t, fake)

	var polls atomic.Int32
	savedFetch := panesFetch
	panesFetch = func(context.Context) panesPayload {
		polls.Add(1)
		return panesPayload{Panes: []hostPane{blockedTestPane("local", "p1", "blocked")}}
	}
	t.Cleanup(func() { panesFetch = savedFetch })
	invalidatePanesCache()
	t.Cleanup(invalidatePanesCache)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go startBlockedWatcher(ctx)

	// Nothing is listening yet, so no host may be enumerated.
	time.Sleep(50 * time.Millisecond)
	if n := polls.Load(); n != 0 {
		t.Fatalf("polled %d times with no active transport", n)
	}

	fake.live = true
	deadline := time.Now().Add(5 * time.Second)
	for fake.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.count() == 0 {
		t.Fatal("a blocked agent produced no notification")
	}
	fake.mu.Lock()
	got := fake.got[0]
	fake.mu.Unlock()
	if got.Kind != notifAgentBlocked || got.Host != "local" || got.Title == "" {
		t.Errorf("delivered %+v", got)
	}
}

// useNotifTransport swaps the notification fan-out for the given transports for
// the duration of the test, so nothing reaches a real push service and a test
// can see exactly what was published. No arguments = nothing is subscribed.
func useNotifTransport(t *testing.T, ts ...notifTransport) {
	t.Helper()
	notifTransportsMu.Lock()
	saved := notifTransports
	notifTransports = ts
	notifTransportsMu.Unlock()
	t.Cleanup(func() {
		notifTransportsMu.Lock()
		notifTransports = saved
		notifTransportsMu.Unlock()
	})
}
