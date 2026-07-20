package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReapOrphanHerdrSSH(t *testing.T) {
	tmp := t.TempDir()
	mk := func(name string) string {
		dir := filepath.Join(tmp, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config"), []byte("Host *\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	deadDir := mk("herdr-ssh-999999999-0")                     // pid can't exist
	aliveDir := mk(fmt.Sprintf("herdr-ssh-%d-0", os.Getpid())) // our own pid: alive
	bogusDir := mk("herdr-ssh-bogus")                          // no pid segment
	nonNumeric := mk("herdr-ssh-12x-0")                        // unparsable pid
	unrelated := mk("some-other-dir")                          // wrong prefix entirely

	if got := reapOrphanHerdrSSH(context.Background(), tmp); got != 1 {
		t.Errorf("removed = %d, want 1", got)
	}
	if fileExists(deadDir) {
		t.Errorf("dead-owner dir %s should have been removed", deadDir)
	}
	for _, dir := range []string{aliveDir, bogusDir, nonNumeric, unrelated} {
		if !fileExists(dir) {
			t.Errorf("dir %s should have been left alone", dir)
		}
	}

	// A second pass finds nothing to do.
	if got := reapOrphanHerdrSSH(context.Background(), tmp); got != 0 {
		t.Errorf("second pass removed = %d, want 0", got)
	}
}

func TestReapOrphanLassoSSH(t *testing.T) {
	tmp := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	deadCtl := mk("lasso-ctl-999999999-citadel-grid.sock") // pid can't exist
	deadFwd := mk("lasso-herdr-999999999-citadel-grid.sock")
	deadFwdClient := mk("lasso-herdr-999999999-citadel-grid-client.sock")
	deadTerm := mk("lasso-gridterm-999999999-abcdef.sock")
	aliveCtl := mk(fmt.Sprintf("lasso-ctl-%d-citadel-grid.sock", os.Getpid())) // our own pid: alive
	bogus := mk("lasso-ctl-bogus")                                             // no pid segment
	nonNumeric := mk("lasso-ctl-12x-0.sock")                                   // unparsable pid
	unrelated := mk("lasso-something-else.sock")                               // wrong prefix

	if got := reapOrphanLassoSSH(context.Background(), tmp); got != 4 {
		t.Errorf("removed = %d, want 4", got)
	}
	for _, p := range []string{deadCtl, deadFwd, deadFwdClient, deadTerm} {
		if fileExists(p) {
			t.Errorf("dead-owner file %s should have been removed", p)
		}
	}
	for _, p := range []string{aliveCtl, bogus, nonNumeric, unrelated} {
		if !fileExists(p) {
			t.Errorf("file %s should have been left alone", p)
		}
	}

	// A second pass finds nothing to do.
	if got := reapOrphanLassoSSH(context.Background(), tmp); got != 0 {
		t.Errorf("second pass removed = %d, want 0", got)
	}
}
