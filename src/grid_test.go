package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestHerdrClientSock(t *testing.T) {
	cases := map[string]string{
		"/tmp/lasso-herdr-1-gigachad.sock": "/tmp/lasso-herdr-1-gigachad-client.sock",
		"/home/u/.config/herdr/herdr.sock": "/home/u/.config/herdr/herdr-client.sock",
	}
	for in, want := range cases {
		if got := herdrClientSock(in); got != want {
			t.Errorf("herdrClientSock(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGridAttachEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"HERDR_SOCKET_PATH=/local/herdr.sock",         // nested lasso: must not leak through
		"HERDR_CLIENT_SOCKET_PATH=/local/client.sock", // ditto
		"HOME=/home/u",
	}
	env := gridAttachEnv(base, "/fwd/herdr.sock", "/fwd/herdr-client.sock")

	var sock, client []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "HERDR_SOCKET_PATH=") {
			sock = append(sock, kv)
		}
		if strings.HasPrefix(kv, "HERDR_CLIENT_SOCKET_PATH=") {
			client = append(client, kv)
		}
	}
	if len(sock) != 1 || sock[0] != "HERDR_SOCKET_PATH=/fwd/herdr.sock" {
		t.Errorf("HERDR_SOCKET_PATH entries = %v, want exactly the forwarded one", sock)
	}
	if len(client) != 1 || client[0] != "HERDR_CLIENT_SOCKET_PATH=/fwd/herdr-client.sock" {
		t.Errorf("HERDR_CLIENT_SOCKET_PATH entries = %v, want exactly the forwarded one", client)
	}
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/u"} {
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Errorf("env lost %q: %v", want, env)
		}
	}
}

func TestGridErrSurfaced(t *testing.T) {
	host := "test-grace-host"
	defer gridErrClear(host)
	t0 := time.Now()
	if gridErrSurfaced(host, t0) {
		t.Error("first failure should be suppressed")
	}
	if gridErrSurfaced(host, t0.Add(gridErrGrace/2)) {
		t.Error("failure inside the grace window should be suppressed")
	}
	if !gridErrSurfaced(host, t0.Add(gridErrGrace)) {
		t.Error("failure persisting past the grace window should surface")
	}
	// A successful poll resets the streak: the next failure is fresh again.
	gridErrClear(host)
	if gridErrSurfaced(host, t0.Add(2*gridErrGrace)) {
		t.Error("failure after a success should start a new grace window")
	}
}

func TestGridErrText(t *testing.T) {
	cases := map[string]string{
		"read unix @->/tmp/lasso-herdr-1-blackbird-grid.sock: i/o timeout": "unreachable (i/o timeout)",
		"dial unix /tmp/x.sock: connect: connection refused":               "unreachable (connection refused)",
		"herdr speaks protocol 17, lasso targets 16":                       "herdr speaks protocol 17, lasso targets 16",
	}
	for in, want := range cases {
		if got := gridErrText(errors.New(in)); got != want {
			t.Errorf("gridErrText(%q) = %q, want %q", in, got, want)
		}
	}
}

// seedGridTerm installs a fake attach for one pane, as if a ttyd had been
// spawned for it, and reports whether its cancel (the ttyd kill) ran.
func seedGridTerm(t *testing.T, host, terminalID, token string, lastUsed time.Time) *bool {
	t.Helper()
	killed := false
	e := &gridTermEntry{
		token:    token,
		base:     "/grid-term/" + token + "/",
		cancel:   func() { killed = true },
		lastUsed: lastUsed,
	}
	gridTerms.mu.Lock()
	if gridTerms.byKey == nil {
		gridTerms.byKey = map[string]*gridTermEntry{}
		gridTerms.byToken = map[string]*gridTermEntry{}
	}
	gridTerms.byKey[host+"|"+terminalID] = e
	gridTerms.byToken[token] = e
	gridTerms.mu.Unlock()
	t.Cleanup(func() { releaseGridTerm(host, terminalID, "", false) })
	return &killed
}

func TestTouchGridTermTokenScoped(t *testing.T) {
	const host, term = "titan", "t1"
	seedGridTerm(t, host, term, "tokenB", time.Now().Add(-time.Minute))

	// A cell still streaming from the attach that owned this pane BEFORE it was
	// re-attached (another viewer's releaseAll, a host switch) must be told it's
	// dead — its /grid-term/tokenA/ requests 404, so answering "alive" left it
	// parked on ttyd's 404 page forever.
	if touchGridTerm(host, term, "tokenA") {
		t.Error("a superseded token should read as dead")
	}
	if touchGridTerm(host, term, "tokenB") != true {
		t.Error("the current token should read as alive")
	}
	if !touchGridTerm(host, term, "") {
		t.Error("an empty token should fall back to key-only liveness")
	}
	// The live touch bumped the idle timer.
	gridTerms.mu.Lock()
	age := time.Since(gridTerms.byKey[host+"|"+term].lastUsed)
	gridTerms.mu.Unlock()
	if age > time.Second {
		t.Errorf("lastUsed not bumped by a live touch (age %v)", age)
	}
	if touchGridTerm(host, "nosuch", "") {
		t.Error("an unknown pane should read as dead")
	}
}

func TestReleaseGridTermIfIdle(t *testing.T) {
	const host, term = "titan", "t2"

	// A grace-deferred unmount release must spare an attach some other viewer is
	// still keepaliving — killing it is what dropped a live cell onto a 404.
	killed := seedGridTerm(t, host, term, "tok", time.Now())
	releaseGridTerm(host, term, "tok", true)
	if *killed {
		t.Error("if-idle release killed an attach that is still being touched")
	}

	// Once nobody has touched it for longer than the keepalive window, the same
	// release goes through.
	gridTerms.mu.Lock()
	gridTerms.byKey[host+"|"+term].lastUsed = time.Now().Add(-2 * gridTermActive)
	gridTerms.mu.Unlock()
	releaseGridTerm(host, term, "tok", true)
	if !*killed {
		t.Error("if-idle release spared an attach nobody is using")
	}

	// Unconditional releases (tab hidden, leaving the Grid) ignore recency: no
	// thin attach may survive to clamp a pane viewed full-size in Herdr.
	killed = seedGridTerm(t, host, term, "tok2", time.Now())
	releaseGridTerm(host, term, "tok2", false)
	if !*killed {
		t.Error("unconditional release should kill a freshly-touched attach")
	}
}
