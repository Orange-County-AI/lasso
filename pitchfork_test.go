package main

import (
	"strings"
	"testing"
)

// block renders the daemon block the way ensureLassoDaemon does, for a given name.
func testBlock(name string) string {
	return daemonBlock(name, "/x/lasso serve --tailscale", "/home/u", "http://127.0.0.1:8090/", "/shims:/usr/bin")
}

func TestUpsertDaemonBlock_AppendAndIdempotent(t *testing.T) {
	name := "lasso"
	blk := testBlock(name)

	// Empty config → the block is appended, with exactly one [daemons.lasso].
	got := upsertDaemonBlock("", name, blk)
	if strings.Count(got, "[daemons."+name+"]") != 1 {
		t.Fatalf("want exactly one [daemons.%s]; got:\n%s", name, got)
	}
	if !strings.Contains(got, beginMarker(name)) || !strings.Contains(got, endMarker(name)) {
		t.Fatalf("markers missing:\n%s", got)
	}

	// Idempotent: feeding the output back in is a no-op.
	if again := upsertDaemonBlock(got, name, blk); again != got {
		t.Errorf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", got, again)
	}
}

func TestUpsertDaemonBlock_PreservesOthers(t *testing.T) {
	name := "lasso"
	existing := "# top comment\n[daemons.other]\nrun = \"./other\"\n"
	got := upsertDaemonBlock(existing, name, testBlock(name))
	if !strings.Contains(got, "[daemons.other]") || !strings.Contains(got, "# top comment") {
		t.Errorf("clobbered unrelated content:\n%s", got)
	}
	if strings.Count(got, "[daemons."+name+"]") != 1 {
		t.Errorf("want one [daemons.%s]:\n%s", name, got)
	}
}

func TestUpsertDaemonBlock_ReplacesMarked(t *testing.T) {
	name := "lasso"
	first := upsertDaemonBlock("[daemons.other]\nrun=\"x\"\n", name, daemonBlock(name, "/x/lasso serve", "/h", "http://127.0.0.1:8090/", "/shims:/usr/bin"))
	// Re-run with --tailscale in the run line → the marked block is replaced.
	second := upsertDaemonBlock(first, name, testBlock(name))
	if strings.Count(second, "[daemons."+name+"]") != 1 {
		t.Fatalf("want one [daemons.%s] after replace:\n%s", name, second)
	}
	if !strings.Contains(second, "serve --tailscale") {
		t.Errorf("replacement didn't take:\n%s", second)
	}
	if strings.Contains(second, "run = \"/x/lasso serve\"\n") {
		t.Errorf("old run line lingered:\n%s", second)
	}
}

func TestUpsertDaemonBlock_StripsUnmarkedHandWritten(t *testing.T) {
	name := "lasso"
	// A citadel-style hand-written block (no markers) with a comment lead-in and a
	// .ready subtable, alongside another daemon.
	existing := `# global daemons

# lasso — old build-from-main wrapper
[daemons.lasso]
run = "/home/u/.local/bin/lasso-serve"
dir = "/home/u/projects/lasso"
boot_start = true
retry = true

[daemons.lasso.ready]
http = "http://127.0.0.1:8090/"

[daemons.keep]
run = "./keep"
`
	got := upsertDaemonBlock(existing, name, testBlock(name))
	if strings.Count(got, "[daemons.lasso]") != 1 {
		t.Fatalf("want exactly one [daemons.lasso] (no duplicate):\n%s", got)
	}
	if strings.Contains(got, "lasso-serve") {
		t.Errorf("old hand-written run line survived:\n%s", got)
	}
	if !strings.Contains(got, "[daemons.keep]") {
		t.Errorf("unrelated [daemons.keep] was dropped:\n%s", got)
	}
	if !strings.Contains(got, beginMarker(name)) {
		t.Errorf("new marked block not added:\n%s", got)
	}
	// And idempotent from here.
	if again := upsertDaemonBlock(got, name, testBlock(name)); again != got {
		t.Errorf("not idempotent after strip:\n%s", again)
	}
}
