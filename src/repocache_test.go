package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTTLCacheServesWarmEntry(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	calls := 0
	fetch := func() (int, error) { calls++; return calls, nil }

	if v, _ := c.get("k", false, fetch); v != 1 {
		t.Fatalf("first get = %d, want 1", v)
	}
	if v, _ := c.get("k", false, fetch); v != 1 {
		t.Errorf("second get = %d, want cached 1", v)
	}
	if calls != 1 {
		t.Errorf("fetch ran %d times, want 1", calls)
	}
	// Separate keys don't share an entry.
	if v, _ := c.get("other", false, fetch); v != 2 {
		t.Errorf("other key = %d, want fresh 2", v)
	}
}

func TestTTLCacheForceAndInvalidate(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	calls := 0
	fetch := func() (int, error) { calls++; return calls, nil }

	c.get("k", false, fetch)
	if v, _ := c.get("k", true, fetch); v != 2 {
		t.Errorf("forced get = %d, want refetched 2", v)
	}
	c.invalidate("k")
	if v, _ := c.get("k", false, fetch); v != 3 {
		t.Errorf("get after invalidate = %d, want refetched 3", v)
	}
}

func TestTTLCacheExpiry(t *testing.T) {
	c := newTTLCache[int](10 * time.Millisecond)
	calls := 0
	fetch := func() (int, error) { calls++; return calls, nil }

	c.get("k", false, fetch)
	time.Sleep(20 * time.Millisecond)
	if v, _ := c.get("k", false, fetch); v != 2 {
		t.Errorf("get after TTL = %d, want refetched 2", v)
	}
}

// Errors must not be cached: an unreachable host has to recover on the next
// request rather than serving the failure for a whole TTL.
func TestTTLCacheDoesNotCacheErrors(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	boom := errors.New("host down")

	if _, err := c.get("k", false, func() (int, error) { return 0, boom }); err != boom {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	v, err := c.get("k", false, func() (int, error) { return 7, nil })
	if err != nil || v != 7 {
		t.Errorf("retry after error = (%d, %v), want (7, nil)", v, err)
	}
}

// Concurrent misses on one key collapse into a single fetch, so N viewers
// opening the dialog at once cost one ssh round trip, not N.
func TestTTLCacheSingleFlight(t *testing.T) {
	c := newTTLCache[int](time.Minute)
	var mu sync.Mutex
	calls := 0

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.get("k", false, func() (int, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				return 1, nil
			})
		}()
	}
	wg.Wait()

	if calls != 1 {
		t.Errorf("fetch ran %d times under concurrency, want 1", calls)
	}
}

func TestHostCacheKeyNormalizesLocal(t *testing.T) {
	for _, alias := range []string{"", "local"} {
		if got := hostCacheKey(alias); got != "local" {
			t.Errorf("hostCacheKey(%q) = %q, want local", alias, got)
		}
	}
	if got := hostCacheKey("citadel"); got != "citadel" {
		t.Errorf("hostCacheKey(citadel) = %q", got)
	}
}
