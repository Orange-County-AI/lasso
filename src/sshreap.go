package main

// Reap only SSH control masters owned by a dead Lasso process. Luvus manages
// its own client transports; Lasso must not guess at their private files.

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const sshReapInterval = 30 * time.Second

func startSSHReaper(ctx context.Context) {
	go func() {
		reapOrphanLassoSSH(ctx, os.TempDir())
		t := time.NewTicker(sshReapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reapOrphanLassoSSH(ctx, os.TempDir())
			}
		}
	}()
}

// reapOrphanLassoSSH cleans up after a lasso that died without running its own
// teardown (SIGKILL, crash, power loss). Its per-host SSH control masters use
// ControlPersist=yes — lifetime managed by backend Close — so nothing else ever
// stops an orphaned one. All the files are PID-keyed, same as luvus's: a
// lasso-ctl-<pid>-*.sock whose pid is dead is garbage — ask its master to exit
// (dropping the sshd-side connection), then remove the socket file. The
// companion forwarded-socket files (lasso-luvus-<pid>-*) of dead pids are
// plain unix socket files; just remove them. Live pids (including our own) are
// never touched.
func reapOrphanLassoSSH(ctx context.Context, tmpDir string) (removed int) {
	deadPidFiles := func(pattern, prefix string) []string {
		var out []string
		files, _ := filepath.Glob(filepath.Join(tmpDir, pattern))
		for _, f := range files {
			pidStr, _, ok := strings.Cut(strings.TrimPrefix(filepath.Base(f), prefix), "-")
			if !ok {
				continue
			}
			pid, err := strconv.Atoi(pidStr)
			if err != nil || pid <= 0 || pid == os.Getpid() || processAlive(pid) {
				continue
			}
			out = append(out, f)
		}
		return out
	}
	for _, ctl := range deadPidFiles("lasso-ctl-*.sock", "lasso-ctl-") {
		octx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = exec.CommandContext(octx, "ssh", "-o", "ControlPath="+ctl, "-O", "exit", "orphan").Run()
		cancel()
		if os.Remove(ctl) == nil {
			removed++
		}
	}
	for _, pattern := range []string{"lasso-luvus-*"} {
		prefix := strings.TrimSuffix(pattern, "*")
		for _, f := range deadPidFiles(pattern, prefix) {
			if os.Remove(f) == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("sshreap:  cleaned %d orphaned lasso ssh socket file(s)", removed)
	}
	return removed
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// processAlive reports whether pid exists. EPERM means it exists but belongs
// to another user — treated as alive so we never touch someone else's master.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
