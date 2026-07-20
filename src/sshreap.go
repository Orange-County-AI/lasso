package main

// Reaper for orphaned herdr SSH control masters.
//
// The left terminal runs `herdr --remote <host>` under ttyd, which spawns one
// herdr client per connected browser and kills it on disconnect. Each client
// opens a private OpenSSH control master (<tmp>/herdr-ssh-<pid>-<n>/ctl) that
// herdr only shuts down on clean exit — a killed client orphans the master,
// which keeps its TCP connection to the remote sshd alive indefinitely
// (herdr's generated ssh config adds ServerAlive keepalives, so it never goes
// stale). Every browser (re)connect to the terminal of a remote active host
// leaks one, and enough of them saturate the remote sshd into resetting new
// handshakes (kex_exchange_identification: Connection reset by peer).
//
// herdr is third-party and pinned, so lasso cleans up after it: a herdr-ssh
// dir whose owning pid is gone is garbage by definition — a live herdr only
// ever uses its own pid's dir — so ask the orphaned master to exit and remove
// the dir. Dirs whose pid is alive (any user's) are never touched.

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

const herdrSSHReapInterval = 30 * time.Second

// startHerdrSSHReaper reaps once at startup (clearing anything accumulated
// while lasso was down) and then on an interval until ctx is cancelled.
func startHerdrSSHReaper(ctx context.Context) {
	go func() {
		reapOrphanHerdrSSH(ctx, os.TempDir())
		reapOrphanLassoSSH(ctx, os.TempDir())
		t := time.NewTicker(herdrSSHReapInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reapOrphanHerdrSSH(ctx, os.TempDir())
				reapOrphanLassoSSH(ctx, os.TempDir())
			}
		}
	}()
}

// reapOrphanLassoSSH cleans up after a lasso that died without running its own
// teardown (SIGKILL, crash, power loss). Its per-host SSH control masters use
// ControlPersist=yes — lifetime managed by backend Close — so nothing else ever
// stops an orphaned one. All the files are PID-keyed, same as herdr's: a
// lasso-ctl-<pid>-*.sock whose pid is dead is garbage — ask its master to exit
// (dropping the sshd-side connection), then remove the socket file. The
// companion forwarded-socket files (lasso-herdr-<pid>-*) and grid ttyd sockets
// (lasso-gridterm-<pid>-*) of dead pids are plain unix socket files; just
// remove them. Live pids (including our own) are never touched.
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
	for _, pattern := range []string{"lasso-herdr-*", "lasso-gridterm-*"} {
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

// reapOrphanHerdrSSH removes every herdr-ssh control dir in tmpDir whose
// owning process is dead, first asking its control master (if any) to exit so
// the sshd-side connection closes too. Returns how many dirs were removed.
func reapOrphanHerdrSSH(ctx context.Context, tmpDir string) (removed int) {
	dirs, _ := filepath.Glob(filepath.Join(tmpDir, "herdr-ssh-*"))
	for _, dir := range dirs {
		rest := strings.TrimPrefix(filepath.Base(dir), "herdr-ssh-")
		pidStr, _, ok := strings.Cut(rest, "-")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 || processAlive(pid) {
			continue
		}
		if ctl := filepath.Join(dir, "ctl"); fileExists(ctl) {
			// Ask the master to close down (this drops its TCP connection to
			// the remote sshd). The destination argument is required by ssh
			// but unused: -O talks to the master over the control socket only.
			octx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = exec.CommandContext(octx, "ssh", "-o", "ControlPath="+ctl, "-O", "exit", "orphan").Run()
			cancel()
		}
		if err := os.RemoveAll(dir); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Printf("sshreap:  cleaned %d orphaned herdr-ssh control dir(s)", removed)
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
