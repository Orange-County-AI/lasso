package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resetPanesCache clears the shared aggregation cache so a test starts cold.
func resetPanesCache(t *testing.T) {
	t.Helper()
	panesCache.mu.Lock()
	panesCache.at, panesCache.data, panesCache.inflight = time.Time{}, panesPayload{}, nil
	panesCache.mu.Unlock()
}

// A slow refresh must not queue every other caller behind it. Before,
// serveAllPanes held panesCache.mu across the fetch, so a fetch that outran the
// TTL made each waiter start another refresher and /api/all-panes stopped
// answering until lasso restarted.
func TestPanesSnapshotSharesOneRefresh(t *testing.T) {
	resetPanesCache(t)
	orig := panesFetch
	t.Cleanup(func() { panesFetch = orig; resetPanesCache(t) })

	var calls atomic.Int32
	release := make(chan struct{})
	panesFetch = func(context.Context) panesPayload {
		calls.Add(1)
		<-release
		return panesPayload{Panes: []hostPane{{Host: "local", PaneID: "p1"}}}
	}

	const callers = 5
	var wg sync.WaitGroup
	got := make([]panesPayload, callers)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = panesSnapshot(context.Background())
		}(i)
	}

	// Give every caller time to arrive and find the refresh already claimed.
	time.Sleep(200 * time.Millisecond)
	close(release)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("callers did not return after the refresh landed")
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1 — the refresh is not shared", n)
	}
	for i, p := range got {
		if len(p.Panes) != 1 {
			t.Errorf("caller %d got %d panes, want the refreshed 1", i, len(p.Panes))
		}
	}
}

// A caller that gives up must not take the endpoint with it: it returns
// promptly, and the refresh it was waiting on still lands for everyone else.
func TestPanesSnapshotHonorsCallerCancellation(t *testing.T) {
	resetPanesCache(t)
	orig := panesFetch
	t.Cleanup(func() { panesFetch = orig; resetPanesCache(t) })

	release := make(chan struct{})
	panesFetch = func(context.Context) panesPayload {
		<-release
		return panesPayload{Panes: []hostPane{{Host: "local", PaneID: "p1"}}}
	}

	refreshed := make(chan struct{})
	go func() { defer close(refreshed); panesSnapshot(context.Background()) }()
	// Let it claim the refresh and block in the fetch.
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan panesPayload, 1)
	go func() { done <- panesSnapshot(ctx) }()

	select {
	case p := <-done:
		if p.Panes == nil {
			t.Error("gave up with a nil pane slice; want an empty array so the JSON isn't null")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled caller stayed blocked on the shared refresh")
	}
	close(release)
	<-refreshed // let the refresh land before cleanup restores panesFetch
}

// The context bound on an ssh command is only real if Wait can't outlive it.
// exec's output copiers read until EOF, and a ProxyCommand the child spawned
// keeps the pipe open after the child is killed — so without the process-group
// kill and WaitDelay, CombinedOutput blocks forever past the deadline.
func TestBoundedCmdReturnsDespiteLingeringGrandchild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// `sleep 60 &` is the stand-in ProxyCommand: a grandchild holding the
	// inherited stdout/stderr pipe while the direct child blocks.
	cmd := boundedCmd(ctx, "sh", "-c", "sleep 60 & sleep 60")
	start := time.Now()
	done := make(chan struct{})
	go func() { _, _ = cmd.CombinedOutput(); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("CombinedOutput never returned after the context expired")
	}
	// Returning inside the WaitDelay backstop means the pipes closed on their
	// own — i.e. the whole process group died, grandchild included.
	if el := time.Since(start); el >= sshWaitDelay {
		t.Errorf("took %v to return; the grandchild outlived the kill and only WaitDelay ended it", el)
	}
}
