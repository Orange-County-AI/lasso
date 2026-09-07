package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReapOrphanLassoSSH(t *testing.T) {
	tmp := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(tmp, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	deadCtl := mk("lasso-ctl-999999999-citadel.sock") // pid can't exist
	deadFwd := mk("lasso-luvus-999999999-citadel.sock")
	deadFwdClient := mk("lasso-luvus-999999999-citadel-client.sock")
	aliveCtl := mk(fmt.Sprintf("lasso-ctl-%d-citadel.sock", os.Getpid())) // our own pid: alive
	bogus := mk("lasso-ctl-bogus")                                        // no pid segment
	nonNumeric := mk("lasso-ctl-12x-0.sock")                              // unparsable pid
	unrelated := mk("lasso-something-else.sock")                          // wrong prefix

	if got := reapOrphanLassoSSH(context.Background(), tmp); got != 3 {
		t.Errorf("removed = %d, want 3", got)
	}
	for _, p := range []string{deadCtl, deadFwd, deadFwdClient} {
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
