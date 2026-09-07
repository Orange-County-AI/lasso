package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// luvusEchoSock starts a one-shot luvus-shaped server on a unix socket that waits
// delay before answering, so a test can pin down which read deadline applied.
func luvusEchoSock(t *testing.T, delay time.Duration) string {
	t.Helper()
	// Not t.TempDir(): its path blows past the ~104-byte sockaddr_un limit on
	// macOS, so the bind fails with EINVAL.
	dir, err := os.MkdirTemp("/tmp", "lasso-t")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
					return
				}
				time.Sleep(delay)
				conn.Write([]byte(`{"id":"ui","result":{"ok":true}}` + "\n"))
			}()
		}
	}()
	return sock
}

// Opening a workspace can outlast cheap reads while the runtime starts a PTY.
func TestLuvusCallSockSlowMethodOutlastsReadDefault(t *testing.T) {
	sock := luvusEchoSock(t, luvusReadTimeout+500*time.Millisecond)

	res, err := luvusCallSock(sock, "workspace.open", map[string]any{"path": "/tmp/project"})
	if err != nil {
		t.Fatalf("workspace.open past the read default: %v", err)
	}
	var got struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(res, &got); err != nil || !got.OK {
		t.Fatalf("result = %s (err %v), want ok:true", res, err)
	}

	// The same delay on a cheap read still trips the default deadline.
	if _, err := luvusCallSock(sock, "pane.read", map[string]any{}); err == nil {
		t.Fatal("pane.read past the read default: got nil error, want timeout")
	}
}
