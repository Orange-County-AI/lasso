package main

import (
	"strings"
	"testing"
)

func TestLocalBackendTmuxArgv(t *testing.T) {
	argv := (&localBackend{}).TmuxArgv([]string{"has-session", "-t", "lasso_x"})
	if argv[0] != "tmux" {
		t.Fatalf("argv[0] = %q, want tmux", argv[0])
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-S "+lassoTmuxSock()) || !strings.Contains(joined, "-f /dev/null") {
		t.Errorf("local tmux argv missing socket/-f flags: %v", argv)
	}
	if !strings.HasSuffix(joined, "has-session -t lasso_x") {
		t.Errorf("local tmux argv missing command: %v", argv)
	}
}

func TestRemoteBackendTmuxArgv(t *testing.T) {
	b := &remoteBackend{alias: "myhost", ctlPath: "/tmp/ctl.sock", home: "/home/bob"}
	argv := b.TmuxArgv([]string{"new-session", "-d", ";", "set", "-g", "status", "off"})
	if argv[0] != "ssh" {
		t.Fatalf("argv[0] = %q, want ssh", argv[0])
	}
	if argv[len(argv)-2] != "myhost" {
		t.Errorf("ssh target = %q, want myhost", argv[len(argv)-2])
	}
	cmd := argv[len(argv)-1]
	// The remote socket resolves against the cached home, and the ";" separator is
	// shell-quoted so the remote shell hands tmux a literal separator token.
	if !strings.Contains(cmd, "tmux -S '/home/bob/.lasso/tmux.sock' -f /dev/null") {
		t.Errorf("remote tmux command missing/socket wrong: %q", cmd)
	}
	if !strings.Contains(cmd, "';'") {
		t.Errorf("remote tmux command should shell-quote the ';' separator: %q", cmd)
	}
}

func TestRemoteBackendTmuxAttachArgv(t *testing.T) {
	b := &remoteBackend{alias: "myhost", ctlPath: "/tmp/ctl.sock", home: "/home/bob"}
	argv := b.TmuxAttachArgv("lasso_abc")
	if argv[0] != "ssh" || argv[1] != "-tt" {
		t.Fatalf("attach argv must start `ssh -tt`: %v", argv)
	}
	if !strings.Contains(argv[len(argv)-1], "attach -t 'lasso_abc'") {
		t.Errorf("attach command wrong: %q", argv[len(argv)-1])
	}
}

func TestStatusKeyHostScoping(t *testing.T) {
	if k := statusKey("", "t1"); k != "t1" {
		t.Errorf("local statusKey = %q, want t1", k)
	}
	if k := statusKey("local", "t1"); k != "t1" {
		t.Errorf(`statusKey("local",…) = %q, want t1`, k)
	}
	if k := statusKey("myhost", "t1"); k != "myhost|t1" {
		t.Errorf("remote statusKey = %q, want myhost|t1", k)
	}
}

func TestIsLocalHost(t *testing.T) {
	for _, h := range []string{"", "local", localHostname()} {
		if !isLocalHost(h) {
			t.Errorf("isLocalHost(%q) = false, want true", h)
		}
	}
	if isLocalHost("some-remote") {
		t.Error("isLocalHost(some-remote) = true, want false")
	}
}
