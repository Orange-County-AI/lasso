package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// fakeMirrorBackend serves HomeDir/ReadDir/ReadFile out of a temp dir; every
// other Backend method panics, so a test that strays off the file path says so.
type fakeMirrorBackend struct {
	Backend
	home string
}

func (f *fakeMirrorBackend) Name() string                      { return "fake" }
func (f *fakeMirrorBackend) HomeDir() (string, error)          { return f.home, nil }
func (f *fakeMirrorBackend) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

func (f *fakeMirrorBackend) ReadDir(p string) ([]fileEntry, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]fileEntry, 0, len(ents))
	for _, e := range ents {
		out = append(out, fileEntry{Name: e.Name(), Dir: e.IsDir()})
	}
	return out, nil
}

// mirrorFixture writes a state directory holding the given <host>-map.json
// bodies and returns a backend rooted at its home.
func mirrorFixture(t *testing.T, files map[string]string) *fakeMirrorBackend {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, mirrorStateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), fs.FileMode(0o644)); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeMirrorBackend{home: home}
}

// The shape herdr-mirror's daemon actually writes, trimmed to the fields lasso
// reads (captured from titan's ocai-map.json).
const ocaiMap = `{
  "workspaces": {"w5": {"localId": "w8C", "lastRemoteLabel": "clem"}},
  "tabs": {"w5:t1": {"localId": "w8C:t2", "lastRemoteLabel": "1"}},
  "panes": {"w5:p1": {"localId": "w8C:p2", "seq": 42, "reported": "omp"}},
  "prev_remote_ids": ["w5", "w5:p1", "w5:t1"],
  "ratios": {}
}`

func TestReadMirrorMapsAttributesPanes(t *testing.T) {
	b := mirrorFixture(t, map[string]string{
		"ocai-map.json": ocaiMap,
		// A second host, and one whose local workspace is long gone (the
		// retired norm-darren file titan still carries).
		"gigachad-map.json": `{"workspaces":{"w2":{"localId":"w8B","lastRemoteLabel":"Apps"}},
			"panes":{"w2:p1":{"localId":"w8B:p2"},"w2:p3":{"localId":"w8B:p3"}}}`,
		"norm-darren-map.json": `{"workspaces":{"w1":{"localId":"w8E","lastRemoteLabel":"darren"}},
			"panes":{"w1:p1":{"localId":"w8E:p2"}}}`,
		// Not a map file, and a broken one — neither may take the others down.
		"daemon.log":   "noise",
		"ocai.ctl":     "noise",
		"bad-map.json": "{not json",
	})
	set := readMirrorMaps(b)

	got, ok := set.lookup("w8C", "w8C:p2")
	if !ok {
		t.Fatalf("lookup(w8C:p2) not attributed")
	}
	want := mirrorRef{Host: "ocai", Workspace: "w5", Pane: "w5:p1", Label: "clem"}
	if got != want {
		t.Errorf("lookup(w8C:p2) = %+v, want %+v", got, want)
	}
	if r, _ := set.lookup("w8B", "w8B:p3"); r.Host != "gigachad" || r.Pane != "w2:p3" || r.Label != "Apps" {
		t.Errorf("lookup(w8B:p3) = %+v, want gigachad w2:p3 Apps", r)
	}
	// A pane the daemon has mapped its workspace but not yet itself still reads
	// as a mirror of the right host — with no remote pane id.
	if r, ok := set.lookup("w8B", "w8B:p9"); !ok || r.Host != "gigachad" || r.Pane != "" {
		t.Errorf("lookup(unmapped pane in mapped ws) = (%+v, %v), want gigachad with no pane id", r, ok)
	}
	// A local pane belongs to nobody.
	if r, ok := set.lookup("w5Z", "w5Z:p1"); ok {
		t.Errorf("lookup(local pane) = %+v, want no attribution", r)
	}
	// A stale host's map is inert rather than harmful: it attributes only ids
	// that no longer exist.
	if r, ok := set.lookup("w8E", "w8E:p2"); !ok || r.Host != "norm-darren" {
		t.Errorf("stale map = (%+v, %v), want it to parse but match nothing live", r, ok)
	}
}

func TestReadMirrorMapsNoStateDir(t *testing.T) {
	b := &fakeMirrorBackend{home: t.TempDir()}
	if set := readMirrorMaps(b); !set.empty() {
		t.Errorf("readMirrorMaps(no herdr-mirror) = %+v, want empty", set)
	}
}

func TestAnyMirrorSentinelGate(t *testing.T) {
	local := []pane{
		{PaneID: "w1:p1", Cwd: "/home/stephan/projects/lasso"},
		{PaneID: "w2:p1", ForegroundCwd: "/home/stephan"},
	}
	if anyMirrorSentinel(local) {
		t.Errorf("anyMirrorSentinel(local panes) = true, want false")
	}
	withMirror := append(local, pane{
		PaneID: "w8C:p2",
		Cwd:    "/home/stephan/.local/state/herdr-mirror/.mirror-pane",
	})
	if !anyMirrorSentinel(withMirror) {
		t.Errorf("anyMirrorSentinel(mirror pane) = false, want true")
	}
}

// The sentinel is a gate, never a classifier: a real local pane can sit in the
// streamer directory (on titan, the workspace herdr-mirror was installed from),
// and only the daemon's map may condemn a pane as a mirror.
func TestSentinelCwdAloneDoesNotAttribute(t *testing.T) {
	b := mirrorFixture(t, map[string]string{"ocai-map.json": ocaiMap})
	set := readMirrorMaps(b)
	if r, ok := set.lookup("w7Z", "w7Z:p1"); ok {
		t.Errorf("local pane parked in the state dir attributed to %+v, want none", r)
	}
}
