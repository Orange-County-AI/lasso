package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// permDenied is herdr 0.9.1's real refusal when it can't write its own install
// directory, as captured over `ssh -tt` (PTY line endings and all) — the failure
// every host with a system-wide /usr/local/bin herdr hits.
const permDenied = "checking stable channel for updates...\r\n" +
	"downloading 0.9.1...\r\n" +
	"update failed: install directory not writable: /usr/local/bin (Permission denied (os error 13)). Try running with appropriate permissions.\r\n" +
	"Shared connection to 10.183.0.21 closed.\r\n"

// runFakeExit produces a genuine *exec.ExitError with the given code, which is
// the only way to exercise sshTransportFailed's classification honestly —
// ExitError's status is not constructible by hand.
func runFakeExit(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != code {
		t.Fatalf("could not build an ExitError with code %d: %v", code, err)
	}
	return err
}

// seedUpdatableHost puts one reachable, herdr-running host in the store, which
// is what serveHostUpdate's guard requires before it will shell out at all.
func seedUpdatableHost(t *testing.T, alias string) {
	t.Helper()
	resetHostStore(t)
	hostStore.mu.Lock()
	hostStore.entries = map[string]HostInfo{
		alias: {Alias: alias, Reachable: true, Running: true, Version: "0.9.0", Protocol: 21},
	}
	hostStore.order = []string{alias}
	hostStore.mu.Unlock()
	t.Cleanup(func() { resetHostStore(t) })
}

// postUpdate drives the handler and decodes its body.
func postUpdate(t *testing.T, alias string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/host-update",
		strings.NewReader(`{"host":"`+alias+`"}`))
	w := httptest.NewRecorder()
	serveHostUpdate(w, r)
	var body map[string]any
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, body
}

// stubUpdate installs a fake remote runner and records every attempt's elevate
// flag, restoring the real one afterwards.
func stubUpdate(t *testing.T, fn func(elevate bool) (string, error)) *[]bool {
	t.Helper()
	var attempts []bool
	prev := runHostUpdateFn
	runHostUpdateFn = func(_ context.Context, _ string, elevate bool) (string, error) {
		attempts = append(attempts, elevate)
		return fn(elevate)
	}
	t.Cleanup(func() { runHostUpdateFn = prev })
	return &attempts
}

// A permission refusal must be retried under sudo — that failure is the whole
// reason the button could never work on a host with a root-owned install dir.
func TestHostUpdateRetriesUnderSudo(t *testing.T) {
	seedUpdatableHost(t, "ocai")
	// A REAL exit-1, so the retry is gated exactly as it is in production: only
	// a failure of herdr itself escalates, never one of ssh (exit 255).
	herdrExit1 := runFakeExit(t, 1)
	attempts := stubUpdate(t, func(elevate bool) (string, error) {
		if !elevate {
			return permDenied, herdrExit1
		}
		return "installed 0.9.1\r\n", nil
	})

	code, body := postUpdate(t, "ocai")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if got := *attempts; len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("attempts = %v, want [false true]", got)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true (the sudo attempt succeeded)", body["ok"])
	}
	if body["elevated"] != true {
		t.Errorf("elevated = %v, want true so the UI can say it took sudo", body["elevated"])
	}
	// Both logs ride along: the first attempt is what explains the second.
	out, _ := body["output"].(string)
	if !strings.Contains(out, "not writable") || !strings.Contains(out, "installed 0.9.1") {
		t.Errorf("output dropped an attempt: %q", out)
	}
}

// Any OTHER failure must not escalate. An update runs on the human's say-so, so
// sudo is warranted when the unprivileged attempt was refused for permissions
// and is privilege taken for nothing otherwise.
func TestHostUpdateDoesNotEscalateOtherFailures(t *testing.T) {
	seedUpdatableHost(t, "ocai")
	herdrExit1 := runFakeExit(t, 1)
	attempts := stubUpdate(t, func(bool) (string, error) {
		return "update failed: failed to fetch manifest: connection refused\r\n", herdrExit1
	})

	_, body := postUpdate(t, "ocai")
	if got := *attempts; len(got) != 1 || got[0] {
		t.Fatalf("attempts = %v, want a single unprivileged attempt", got)
	}
	if body["ok"] != false {
		t.Errorf("ok = %v, want false", body["ok"])
	}
	if body["elevated"] != false {
		t.Errorf("elevated = %v, want false", body["elevated"])
	}
}

// ssh's own refusal reads as "Permission denied (publickey)", which is not a
// failure sudo can fix — retrying it buys a second dead dial and then reports
// the second failure's message in place of the first's.
func TestHostUpdateDoesNotEscalateSSHFailure(t *testing.T) {
	seedUpdatableHost(t, "ocai")
	// exec's own shape for "ssh exited 255", which is how a transport failure
	// reaches the handler.
	sshRefused := runFakeExit(t, 255)
	attempts := stubUpdate(t, func(bool) (string, error) {
		return "dev@ocai: Permission denied (publickey).\r\n", sshRefused
	})

	_, body := postUpdate(t, "ocai")
	if got := *attempts; len(got) != 1 {
		t.Fatalf("attempts = %v, want a single attempt; ssh never ran herdr", got)
	}
	if body["elevated"] != false {
		t.Errorf("elevated = %v, want false", body["elevated"])
	}
}

// The reported error is herdr's own sentence, not "exit status 1" — the bare
// exec error is what the UI used to render as a "failed" chip that explained
// nothing.
func TestHostUpdateReportsHerdrsReason(t *testing.T) {
	seedUpdatableHost(t, "ocai")
	herdrExit1 := runFakeExit(t, 1)
	stubUpdate(t, func(elevate bool) (string, error) {
		if !elevate {
			return permDenied, herdrExit1
		}
		return "sudo: a password is required\r\n", herdrExit1
	})

	_, body := postUpdate(t, "ocai")
	msg, _ := body["error"].(string)
	if msg != "sudo: a password is required" {
		t.Errorf("error = %q, want sudo's own refusal", msg)
	}
}

// A host lasso has never probed as reachable must not make us shell out.
func TestHostUpdateRefusesUnknownHost(t *testing.T) {
	resetHostStore(t)
	t.Cleanup(func() { resetHostStore(t) })
	attempts := stubUpdate(t, func(bool) (string, error) { return "", nil })
	code, _ := postUpdate(t, "nowhere")
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	if len(*attempts) != 0 {
		t.Errorf("shelled out to an unprobed host: %v", *attempts)
	}
}

func TestUpdateFailureReason(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want string
	}{
		{
			// ssh's own sign-off lands AFTER the remote's message, so taking the
			// literal last line would report ssh closing the connection.
			name: "skips ssh sign-off",
			out:  permDenied,
			err:  errors.New("exit status 1"),
			want: "update failed: install directory not writable: /usr/local/bin (Permission denied (os error 13)). Try running with appropriate permissions.",
		},
		{
			name: "falls back to the exec error when nothing was said",
			out:  "\r\nConnection to host closed.\r\n",
			err:  errors.New("exit status 255"),
			want: "exit status 255",
		},
		{
			name: "truncates a wall of text",
			out:  strings.Repeat("x", updateFailureMsgMax+50),
			err:  errors.New("exit status 1"),
			want: strings.Repeat("x", updateFailureMsgMax) + "…",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := updateFailureReason(c.out, c.err); got != c.want {
				t.Errorf("updateFailureReason() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNeedsElevation(t *testing.T) {
	if !needsElevation(permDenied) {
		t.Error("herdr's own permission refusal must earn the sudo retry")
	}
	if needsElevation("update failed: failed to fetch manifest: connection refused") {
		t.Error("a network failure must not earn a privileged retry")
	}
}

// --handoff is what makes an update visible at all: without it herdr replaces
// the binary and leaves the old server running, so the version lasso reads (the
// SERVER's) never moves and the row offers the same button forever.
func TestHerdrUpdateCmdHandsOff(t *testing.T) {
	plain := herdrUpdateCmd(false)
	if !strings.Contains(plain, "--handoff") {
		t.Errorf("unprivileged cmd = %q, want --handoff", plain)
	}
	sudo := herdrUpdateCmd(true)
	if !strings.Contains(sudo, "--handoff") {
		t.Errorf("elevated cmd = %q, want --handoff", sudo)
	}
	// -n so a host without passwordless sudo fails fast instead of eating the
	// prompt answers we feed and hanging on a PTY until the timeout.
	if !strings.Contains(sudo, "sudo -n ") {
		t.Errorf("elevated cmd = %q, want a non-interactive sudo", sudo)
	}
	// command -v resolves herdr in the login shell we already put the user-local
	// dirs on PATH for, rather than whatever sudo's secure_path finds.
	if !strings.Contains(sudo, `"$(command -v herdr)"`) {
		t.Errorf("elevated cmd = %q, want the same herdr the plain attempt runs", sudo)
	}
}

// herdr reports "the binary on disk is newer than the server I am running"
// separately from the version, and that is the one signal telling "behind,
// press update" apart from "updated already, needs a restart" — two states
// identical in Version alone, needing opposite things.
func TestHerdrStatusInfoCarriesStale(t *testing.T) {
	// ocai's real status after `herdr update` installed 0.9.1 while its 0.9.0
	// server kept running.
	raw := []byte(`{"status":"running","running":true,"version":"0.9.0",` +
		`"protocol":22,"socket":"/dev/shm/herdr/herdr.sock","server_binary_stale":true}`)
	hi := herdrStatusInfo(HostInfo{Alias: "ocai", Reachable: true}, raw, 22)
	if !hi.Stale {
		t.Error("Stale = false; a host whose server is running a superseded binary must say so")
	}
	// It is orthogonal to compatibility: this host is perfectly usable, it just
	// is not running what is installed on it.
	if !hi.Compatible || !hi.Running {
		t.Errorf("compatible=%v running=%v; a stale server is still a usable one", hi.Compatible, hi.Running)
	}
	if hi.Version != "0.9.0" {
		t.Errorf("Version = %q, want the RUNNING server's version", hi.Version)
	}
}

// The ordinary case must not claim staleness, or every row would wear the hint.
func TestHerdrStatusInfoFreshServer(t *testing.T) {
	raw := []byte(`{"running":true,"version":"0.9.1","protocol":22}`)
	hi := herdrStatusInfo(HostInfo{Alias: "norm", Reachable: true}, raw, 22)
	if hi.Stale {
		t.Error("Stale = true for a host that reported nothing of the sort")
	}
}
