package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTtydRestartKeepsLiveSocketOnStopTimeout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ttyd.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	manager := newTtydManager(context.Background(), sock, "/terminal")
	manager.cancel = func() {}
	manager.waitTimeout = 20 * time.Millisecond

	// If restart incorrectly reaches startTtyd, it will unlink sock before
	// failing to find the ttyd executable.
	t.Setenv("PATH", t.TempDir())
	err = manager.restart("sh", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for ttyd socket") {
		t.Fatalf("restart error = %v, want socket timeout", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("live socket was removed: %v", err)
	}
}
