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
