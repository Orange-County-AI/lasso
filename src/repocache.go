package main

import (
	"log"
	"sync"
	"time"
)

// Repo and branch lookups are the slowest reads in the app: a remote repo list
// costs two sqlite-over-ssh round trips plus an SFTP directory walk, and every
// repo's branch list costs three `git` invocations over ssh. Both were fetched
// on demand, so the first New Agent dialog open — and every repo selection
// inside it — stalled on the network.
//
// This file caches both, and warms them in the background across every usable
// host so the dialog is already populated by the time it opens.

const (
	// TTLs are comfortably longer than warmInterval so a warmed entry never
	// expires between cycles — on-demand reads are then near-always cache hits.
	repoCacheTTL   = 5 * time.Minute
	branchCacheTTL = 5 * time.Minute

	// warmInterval is how often the background warmer refreshes every host. It
	// exceeds gridBackendIdle, so pooled ssh connections are still reaped between
	// cycles rather than pinned open forever.
	warmInterval = 2 * time.Minute

	// warmHostConcurrency bounds hosts warmed at once; warmRepoConcurrency bounds
	// concurrent branch fetches within one host. Both keep the warmer from
	// stampeding a box with ssh processes.
	warmHostConcurrency = 4
	warmRepoConcurrency = 4
)

// ttlCache is a keyed cache with per-key single-flight: concurrent misses on the
// same key wait on one fetch instead of each firing their own ssh round trip.
// Errors are deliberately NOT cached — an unreachable host should recover on the
// next request rather than serving a stale failure for the whole TTL.
type ttlCache[T any] struct {
	ttl     time.Duration
	mu      sync.Mutex // guards entries
	entries map[string]*ttlEntry[T]
}

type ttlEntry[T any] struct {
	mu  sync.Mutex // held across the fetch → single-flight per key
	at  time.Time
	val T
}

func newTTLCache[T any](ttl time.Duration) *ttlCache[T] {
	return &ttlCache[T]{ttl: ttl, entries: map[string]*ttlEntry[T]{}}
}

// entry returns key's entry, creating it if absent.
func (c *ttlCache[T]) entry(key string) *ttlEntry[T] {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		e = &ttlEntry[T]{}
		c.entries[key] = e
	}
	return e
}

// get returns key's cached value, calling fetch on a miss (or when force is set,
// which the ?refresh=1 query param uses to bypass a warm entry).
func (c *ttlCache[T]) get(key string, force bool, fetch func() (T, error)) (T, error) {
	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	if !force && !e.at.IsZero() && time.Since(e.at) < c.ttl {
		return e.val, nil
	}
	v, err := fetch()
	if err != nil {
		var zero T
		return zero, err
	}
	e.val, e.at = v, time.Now()
	return v, nil
}

// invalidate drops key so the next get refetches.
func (c *ttlCache[T]) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// ---------------------------------------------------------------------------
// the two caches
// ---------------------------------------------------------------------------

type repoListing struct {
	root  string
	repos []repoEntry
}

type branchListing struct {
	local  []string
	remote []string
	def    string
}

var (
	repoCache   = newTTLCache[repoListing](repoCacheTTL)
	branchCache = newTTLCache[branchListing](branchCacheTTL)
)

// branchKey namespaces a repo's branches by host — the same path on two hosts is
// two different repos.
func branchKey(host, path string) string { return host + "\x00" + path }

// cachedHostReposList is the cached form of hostReposList.
func cachedHostReposList(host string, force bool) (string, []repoEntry, error) {
	l, err := repoCache.get(host, force, func() (repoListing, error) {
		root, repos, err := hostReposList(host)
		return repoListing{root: root, repos: repos}, err
	})
	if err != nil {
		return "", nil, err
	}
	// Hand back a copy: the cached slice is shared with every other caller.
	repos := make([]repoEntry, len(l.repos))
	copy(repos, l.repos)
	return l.root, repos, nil
}

// cachedBranchList is the cached form of branchList. be must be a backend for
// host; path must be already expanded (as the repo list hands it out), so the
// key matches what the warmer stored.
func cachedBranchList(host string, be Backend, path string, force bool) (local, remote []string, def string) {
	l, err := branchCache.get(branchKey(host, path), force, func() (branchListing, error) {
		lo, re, d := branchList(be, path)
		return branchListing{local: lo, remote: re, def: d}, nil
	})
	if err != nil {
		return nil, nil, ""
	}
	return l.local, l.remote, l.def
}

// invalidateRepoCache drops host's cached repo list — call after anything that
// changes what the picker should show (repos_root, per-repo config, the
// remembered base branch).
func invalidateRepoCache(host string) {
	repoCache.invalidate(hostCacheKey(host))
}

// invalidateBranchCache drops one repo's cached branches — call after creating a
// branch on it so the new branch shows up without waiting for the TTL.
func invalidateBranchCache(host, path string) {
	branchCache.invalidate(branchKey(hostCacheKey(host), path))
}

// hostCacheKey normalizes the empty/"local" aliases onto one key so a write
// through one spelling invalidates the entry a read made through the other.
func hostCacheKey(host string) string {
	if isLocalHost(host) {
		return "local"
	}
	return host
}

// ---------------------------------------------------------------------------
// background warmer
// ---------------------------------------------------------------------------

var warmerOnce sync.Once

// startCacheWarmer launches (once) the goroutine that eagerly fetches repos and
// branches for every usable host, refreshing on warmInterval. It runs one cycle
// immediately so the caches are populated before the first dialog open.
func startCacheWarmer() {
	warmerOnce.Do(func() {
		go func() {
			t := time.NewTicker(warmInterval)
			defer t.Stop()
			for {
				warmAllHosts()
				select {
				case <-srvCtx.Done():
					return
				case <-t.C:
				}
			}
		}()
	})
}

// warmAllHosts warms the local host plus every discovered host we're allowed to
// drive, bounded to warmHostConcurrency at a time.
func warmAllHosts() {
	hosts := []string{"local"}
	for _, h := range discoverHosts(srvCtx, false) {
		if h.Reachable && h.Running && h.Compatible && h.Alias != "local" {
			hosts = append(hosts, h.Alias)
		}
	}

	sem := make(chan struct{}, warmHostConcurrency)
	var wg sync.WaitGroup
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			warmHost(host)
		}(host)
	}
	wg.Wait()
}

// warmHost refetches host's repo list, then every repo's branches. Failures are
// logged and skipped — a host that's briefly down just leaves its entries cold,
// and the on-demand path retries.
func warmHost(host string) {
	key := hostCacheKey(host)
	_, repos, err := cachedHostReposList(key, true)
	if err != nil {
		log.Printf("warm: repos on %s: %v", key, err)
		return
	}
	be, err := gridHostBackend(key)
	if err != nil {
		log.Printf("warm: backend for %s: %v", key, err)
		return
	}

	sem := make(chan struct{}, warmRepoConcurrency)
	var wg sync.WaitGroup
	for _, re := range repos {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cachedBranchList(key, be, path, true)
		}(re.Path)
	}
	wg.Wait()
}
