package main

import (
	"errors"
	"testing"
	"time"
)

func TestPaneErrSurfaced(t *testing.T) {
	host := "test-grace-host"
	defer paneErrClear(host)
	t0 := time.Now()
	if paneErrSurfaced(host, t0) {
		t.Error("first failure should be suppressed")
	}
	if paneErrSurfaced(host, t0.Add(paneErrGrace/2)) {
		t.Error("failure inside the grace window should be suppressed")
	}
	if !paneErrSurfaced(host, t0.Add(paneErrGrace)) {
		t.Error("failure persisting past the grace window should surface")
	}
	// A successful poll resets the streak: the next failure is fresh again.
	paneErrClear(host)
	if paneErrSurfaced(host, t0.Add(2*paneErrGrace)) {
		t.Error("failure after a success should start a new grace window")
	}
}

func TestPaneErrText(t *testing.T) {
	cases := map[string]string{
		"read unix @->/tmp/lasso-herdr-1-blackbird-hostpool.sock: i/o timeout": "unreachable (i/o timeout)",
		"dial unix /tmp/x.sock: connect: connection refused":                   "unreachable (connection refused)",
		"herdr speaks protocol 17, lasso targets 16":                           "herdr speaks protocol 17, lasso targets 16",
	}
	for in, want := range cases {
		if got := paneErrText(errors.New(in)); got != want {
			t.Errorf("paneErrText(%q) = %q, want %q", in, got, want)
		}
	}
}
