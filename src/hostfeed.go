package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// A hostFeed is one host's live state — its pane.list poll, its herdr event
// subscription, its layout revision, and the SSE clients watching it.
//
// There used to be exactly one of these, unnamed, inside the hub: lasso polled
// "the active host" and pushed the result to every open tab. That is what made
// per-tab hosts impossible, and splitting it is the substance of this change.
// Each host now gets its own poller and its own client set, so a tab on norm and
// a tab on titan see their own machine and neither one's herdr events invalidate
// the other's cache.
//
// A feed is created on demand by the first tab to ask for that host and torn
// down feedIdle after the last one leaves, so an unwatched host costs nothing —
// which matters because the poll it runs (pane.list) is herdr's most expensive
// method, ~0.5–1.5s on a busy session, and it would otherwise run once per alias
// in the ssh config forever.
type hostFeed struct {
	h    *hub
	host string

	mu      sync.RWMutex
	be      Backend // re-pointed when the host's pooled connection is redialed
	cur     Active
	rev     int    // pane-list layout revision (bumped when lastSig changes)
	lastSig string // last seen layout signature
	clients map[chan Active]struct{}

	trigger chan struct{}

	// subMu guards the event subscription's cancel, which is replaced whenever
	// the host's connection is redialed under us.
	subMu     sync.Mutex
	subCancel context.CancelFunc

	// idleTimer stops the feed once no client has watched it for feedIdle. Held
	// under the hub's lock, not the feed's.
	idleTimer *time.Timer
	cancel    context.CancelFunc
}

// feedIdle is how long a host's poller outlives its last watcher. Long enough to
// ride out a page reload or a quick hop away and back (which would otherwise pay
// a fresh pane.list and a re-subscription every time), short enough that a host
// visited once stops being polled well before its SSH master is idle-reaped.
const feedIdle = 90 * time.Second

// eventDebounce is the quiet window a feed waits after the first herdr event
// before refreshing, so a burst of events yields one refresh of the settled
// state. Kept well under human perception so a single focus change still feels
// immediate.
const eventDebounce = 120 * time.Millisecond

func newHostFeed(h *hub, be Backend) *hostFeed {
	return &hostFeed{
		h:    h,
		host: be.Name(),
		be:   be,
		// Seed HerdrUp=true so a tab connecting before the first poll doesn't
		// briefly flash the "herdr disconnected" state.
		cur:     Active{HerdrUp: true, Host: be.Name(), HostSlug: hostSlug(be.Name()), CwdHost: be.Name()},
		clients: map[chan Active]struct{}{},
		trigger: make(chan struct{}, 1),
	}
}

func (f *hostFeed) backend() Backend {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.be
}

// repoint swaps in a freshly dialed connection for this host and restarts the
// event subscription on its new socket. Called when the host pool redials a
// master that died under us (laptop sleep, sshd restart); without it the feed
// would keep reading a socket that no longer exists.
func (f *hostFeed) repoint(be Backend) {
	f.mu.Lock()
	f.be = be
	f.mu.Unlock()
	f.startSub()
	f.kick()
}

// kick forces a near-immediate refresh (non-blocking).
func (f *hostFeed) kick() {
	select {
	case f.trigger <- struct{}{}:
	default:
	}
}

func (f *hostFeed) snapshot() Active {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.cur
}

// startSub (re)starts this host's herdr event subscription under a fresh child
// of the feed's context, cancelling any prior one.
func (f *hostFeed) startSub() {
	f.subMu.Lock()
	defer f.subMu.Unlock()
	if f.cancel == nil { // feed not running yet; run() will start the sub
		return
	}
	if f.subCancel != nil {
		f.subCancel()
	}
	ctx, cancel := context.WithCancel(f.h.rootCtx)
	f.subCancel = cancel
	go subscribeEvents(ctx, f.backend, f.trigger)
}

// push fans a state frame out to this host's watchers. Non-blocking per client:
// a stalled reader drops the frame rather than wedging the poller.
func (f *hostFeed) push(a Active) {
	f.mu.RLock()
	clients := make([]chan Active, 0, len(f.clients))
	for c := range f.clients {
		clients = append(clients, c)
	}
	f.mu.RUnlock()
	for _, c := range clients {
		select {
		case c <- a:
		default:
		}
	}
}

// pushCurrent re-sends the feed's current state after a GLOBAL revision (theme,
// UI prefs) moved. Those live on the hub, not on any host, so the bump has to be
// stamped into every feed's snapshot and pushed — there is no host poll that
// would otherwise carry it.
func (f *hostFeed) pushCurrent() {
	f.mu.Lock()
	f.cur.ThemeRev, f.cur.UIStateRev = f.h.revs()
	cur := f.cur
	f.mu.Unlock()
	f.push(cur)
}

// run polls this host until ctx ends. It is the old hub.run loop, scoped to one
// host: the theme re-resolution that used to share it moved to the hub, since
// herdr's config.toml is read locally and re-reading it once per watched host
// would have multiplied that work by the number of open tabs' hosts.
func (f *hostFeed) run(ctx context.Context) {
	f.subMu.Lock()
	f.cancel = func() {}
	f.subMu.Unlock()
	f.startSub()

	ticker := time.NewTicker(*pollEvery)
	defer ticker.Stop()

	f.refresh()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			f.subMu.Lock()
			if f.subCancel != nil {
				f.subCancel()
				f.subCancel = nil
			}
			f.subMu.Unlock()
			return
		case <-f.trigger:
			// An event means THIS host's pane state changed; refetch rather than
			// serve its cache. Other hosts' caches are untouched.
			invalidatePaneList(f.host)
			if debounce == nil {
				debounce = time.After(eventDebounce)
			}
		case <-debounce:
			debounce = nil
			f.refresh()
		case <-ticker.C:
			f.refresh()
		}
	}
}

func (f *hostFeed) refresh() {
	be := f.backend()
	a, sig, err := fetchActive(be)
	if err != nil {
		// herdr's socket is unreachable (closed in the terminal, or the ssh
		// master died). Keep the last-known state but mark it stale, and notify
		// watchers once on the up->down transition so the sidebar can show a
		// disconnected cue.
		f.mu.Lock()
		var down Active
		send := f.cur.HerdrUp
		if send {
			f.cur.HerdrUp = false
			down = f.cur
		}
		f.mu.Unlock()
		if send {
			f.push(down)
		}
		return
	}
	a.HerdrUp = true
	themeRev, uiStateRev := f.h.revs()
	f.mu.Lock()
	if sig != f.lastSig {
		f.lastSig = sig
		f.rev++
	}
	a.PanesRev = f.rev
	a.ThemeRev = themeRev
	a.UIStateRev = uiStateRev
	a.Host = f.host
	a.HostSlug = hostSlug(f.host)
	// activeCwd fills CwdHost only when a resolver knows the host; an empty one
	// (no focused pane, or a resolver that can't tell) defaults to this feed's
	// host so the browser always has a concrete host to browse.
	if a.CwdHost == "" {
		a.CwdHost = a.Host
	}
	changed := a != f.cur
	f.cur = a
	f.mu.Unlock()
	if changed {
		f.push(a)
	}
}

// ---------------------------------------------------------------------------
// hub side: the feed registry
// ---------------------------------------------------------------------------

// feed returns host's feed, starting it if this is the first interest in that
// host. An empty host means the default one. The returned feed is guaranteed to
// be running; watch/unwatch decide how long it stays that way.
func (h *hub) feed(host string) (*hostFeed, error) {
	be, err := namedHostBackend(host)
	if err != nil {
		return nil, err
	}
	name := be.Name()
	h.mu.Lock()
	if h.feeds == nil {
		h.feeds = map[string]*hostFeed{}
	}
	if f := h.feeds[name]; f != nil {
		// A feed whose connection was redialed under it still points at the old
		// backend; the pool hands out the live one, so adopt it here too.
		if f.backend() != be {
			h.mu.Unlock()
			f.repoint(be)
			return f, nil
		}
		h.mu.Unlock()
		return f, nil
	}
	f := newHostFeed(h, be)
	ctx, cancel := context.WithCancel(h.rootCtx)
	f.cancel = cancel
	h.feeds[name] = f
	h.mu.Unlock()
	log.Printf("feed:     watching %s", name)
	go f.run(ctx)
	// Unwatched from birth: a caller that only wanted a snapshot (GET
	// /api/active) never watches, and the idle timer collects the feed rather
	// than leaving it polling forever. The default host is exempt (see
	// scheduleFeedIdle).
	h.scheduleFeedIdle(f)
	return f, nil
}

// watch registers a client channel on host's feed and returns the unsubscribe.
func (h *hub) watch(host string) (*hostFeed, chan Active, func(), error) {
	f, err := h.feed(host)
	if err != nil {
		return nil, nil, nil, err
	}
	ch := make(chan Active, 4)
	h.mu.Lock()
	if f.idleTimer != nil {
		f.idleTimer.Stop()
		f.idleTimer = nil
	}
	h.mu.Unlock()
	f.mu.Lock()
	f.clients[ch] = struct{}{}
	f.mu.Unlock()
	return f, ch, func() {
		f.mu.Lock()
		delete(f.clients, ch)
		last := len(f.clients) == 0
		f.mu.Unlock()
		if last {
			h.scheduleFeedIdle(f)
		}
	}, nil
}

// scheduleFeedIdle arms (or re-arms) the timer that stops an unwatched feed. The
// timer is cancelled the moment a client returns, so a reload or a quick hop
// away and back keeps the poller warm.
//
// The DEFAULT host is exempt and polls for the life of the server. It is what a
// fresh tab lands on, so hub.run warms it at boot precisely so first paint is
// not cold — letting the idle timer collect it a minute and a half later would
// undo that, and it is also what lasso polled unconditionally before any of this
// was per host.
func (h *hub) scheduleFeedIdle(f *hostFeed) {
	if b := defaultBackend(); b != nil && b.Name() == f.host {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if f.idleTimer != nil {
		f.idleTimer.Stop()
	}
	f.idleTimer = time.AfterFunc(feedIdle, func() { h.stopFeedIfIdle(f) })
}

func (h *hub) stopFeedIfIdle(f *hostFeed) {
	if b := defaultBackend(); b != nil && b.Name() == f.host {
		return // the default host polls for the life of the server
	}
	f.mu.RLock()
	watched := len(f.clients) > 0
	f.mu.RUnlock()
	if watched {
		return
	}
	h.mu.Lock()
	if h.feeds[f.host] != f {
		h.mu.Unlock()
		return
	}
	delete(h.feeds, f.host)
	cancel := f.cancel
	if f.idleTimer != nil {
		f.idleTimer.Stop()
		f.idleTimer = nil
	}
	h.mu.Unlock()
	log.Printf("feed:     stopped watching %s (idle)", f.host)
	if cancel != nil {
		cancel()
	}
}

// feedHosts snapshots the hosts with a running feed. hostInUse consults it so a
// watched host's connection and terminals are never reaped out from under the
// tab watching them.
func (h *hub) feedHosts() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.feeds))
	for host := range h.feeds {
		out = append(out, host)
	}
	return out
}

// eachFeed runs fn against every running feed.
func (h *hub) eachFeed(fn func(*hostFeed)) {
	h.mu.RLock()
	feeds := make([]*hostFeed, 0, len(h.feeds))
	for _, f := range h.feeds {
		feeds = append(feeds, f)
	}
	h.mu.RUnlock()
	for _, f := range feeds {
		fn(f)
	}
}

// hostInUse reports whether anything still depends on host's connection and
// terminals: it is the default host, a tab is watching it, or a terminal for it
// is resident. It replaces the old "is this the active host?" test, which had
// exactly one answer and so could not protect a second tab's host.
func hostInUse(host string) bool {
	if b := defaultBackend(); b != nil && b.Name() == host {
		return true
	}
	if srvHub != nil {
		for _, h := range srvHub.feedHosts() {
			if h == host {
				return true
			}
		}
	}
	return terminals.herdr.resident(host) || terminals.shell.resident(host)
}
