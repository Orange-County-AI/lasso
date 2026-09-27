package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParsePluginSource(t *testing.T) {
	ok := []struct {
		src, ref string
		want     ghSource
	}{
		{"owner/repo", "", ghSource{Owner: "owner", Repo: "repo"}},
		{"owner/repo.git", "", ghSource{Owner: "owner", Repo: "repo"}},
		{"owner/repo/plugins/hello", "v1.2", ghSource{Owner: "owner", Repo: "repo", Subdir: "plugins/hello", Ref: "v1.2"}},
		{"github.com/o-1/r_2", "", ghSource{Owner: "o-1", Repo: "r_2"}},
		{"https://github.com/owner/repo", "", ghSource{Owner: "owner", Repo: "repo"}},
		{"https://github.com/owner/repo.git", "", ghSource{Owner: "owner", Repo: "repo"}},
		{"https://github.com/owner/repo/tree/main/sub/dir", "", ghSource{Owner: "owner", Repo: "repo", Subdir: "sub/dir", Ref: "main"}},
		{"https://github.com/owner/repo/tree/abc1234", "abc1234", ghSource{Owner: "owner", Repo: "repo", Ref: "abc1234"}},
		{"owner/repo/", "", ghSource{Owner: "owner", Repo: "repo"}},
	}
	for _, c := range ok {
		got, err := parsePluginSource(c.src, c.ref)
		if err != nil || got != c.want {
			t.Errorf("parse(%q, %q) = %+v, %v; want %+v", c.src, c.ref, got, err, c.want)
		}
	}
	bad := []struct{ src, ref string }{
		{"", ""},
		{"owner", ""},
		{"../x", ""},
		{"owner/..", ""},
		{"a/b/../c", ""},
		{"a/b/.hidden", ""},
		{"https://gitlab.com/owner/repo", ""},
		{"http://github.com/owner/repo", ""},
		{"https://github.com/owner/repo/blob/main/x", ""},
		{"https://github.com/owner/repo?x=1", ""},
		{"git@github.com:owner/repo.git", ""},
		{"file:///tmp/repo", ""},
		{"/abs/path", ""},
		{"owner/re po", ""},
		{"owner/repo", "-upload-pack=x"},
		{"owner/repo", "a..b"},
		{"https://github.com/owner/repo/tree/main", "other"},
	}
	for _, c := range bad {
		if g, err := parsePluginSource(c.src, c.ref); err == nil {
			t.Errorf("parse(%q, %q) = %+v; want an error", c.src, c.ref, g)
		}
	}
}

// gitRun runs git in dir for test fixtures.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fakeGitHub is a set of local bare repositories standing in for GitHub,
// wired in through the clone-URL test seam.
type fakeGitHub struct {
	t     *testing.T
	root  string
	works map[string]string // repo -> working tree
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := &fakeGitHub{t: t, root: t.TempDir(), works: map[string]string{}}
	oldURL, oldFile := pluginCloneURL, pluginGitAllowFile
	pluginCloneURL = func(owner, repo string) string {
		return "file://" + filepath.Join(f.root, "bare", owner, repo+".git")
	}
	pluginGitAllowFile = true
	t.Cleanup(func() { pluginCloneURL, pluginGitAllowFile = oldURL, oldFile })
	return f
}

// commit writes files (a "->" prefix makes a symlink to the rest) into
// owner/repo, commits, pushes to its bare repo, and returns the commit.
func (f *fakeGitHub) commit(repo string, files map[string]string) string {
	t := f.t
	t.Helper()
	work, ok := f.works[repo]
	bare := filepath.Join(f.root, "bare", "owner", repo+".git")
	if !ok {
		work = filepath.Join(f.root, "work", repo)
		_ = os.MkdirAll(work, 0o755)
		gitRun(t, work, "init", "-q")
		_ = os.MkdirAll(filepath.Dir(bare), 0o755)
		gitRun(t, f.root, "init", "-q", "--bare", bare)
		gitRun(t, work, "remote", "add", "origin", bare)
		f.works[repo] = work
	}
	for rel, body := range files {
		p := filepath.Join(work, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.Remove(p)
		if target, isLink := strings.CutPrefix(body, "->"); isLink {
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "c", "--allow-empty")
	gitRun(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	return gitRun(t, work, "rev-parse", "HEAD")
}

func manifestJSON(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func helloManifest(version string, tabs ...string) string {
	var ts []any
	for _, id := range tabs {
		ts = append(ts, map[string]any{"id": id, "label": id, "entry": "ui/" + id + ".html"})
	}
	return manifestJSON(map[string]any{"name": "hello", "version": version, "description": "d", "tabs": ts})
}

// apiCall drives the manager's HTTP handler.
func apiCall(t *testing.T, m *pluginManager, method, path string, body any, out any) int {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	m.serveAPI(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	if rec.Code != http.StatusOK {
		t.Logf("%s %s -> %d %s", method, path, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return rec.Code
}

func stagingEntries(t *testing.T, m *pluginManager) []string {
	t.Helper()
	ents, _ := os.ReadDir(m.stagingRoot())
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestPluginInstallPreviewConfirm(t *testing.T) {
	gh := newFakeGitHub(t)
	m, dir := testPluginManager(t)
	c1 := gh.commit("hello", map[string]string{"plugin.json": helloManifest("0.1.0", "main"), "ui/main.html": "<p>hi</p>"})

	var p pluginPreview
	if code := apiCall(t, m, "POST", "/api/plugins/install/preview", map[string]string{"source": "owner/hello"}, &p); code != 200 {
		t.Fatalf("preview = %d", code)
	}
	if p.Name != "hello" || p.Commit != c1 || p.Source != "owner/hello" || p.Token == "" || len(p.Permissions.Tabs) != 1 || p.Fingerprint == "" {
		t.Fatalf("preview = %+v", p)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello")); err == nil {
		t.Fatal("a preview must not install anything")
	}
	if st := stagingEntries(t, m); len(st) != 1 {
		t.Fatalf("staging = %v; want one stage", st)
	}

	// A fingerprint other than the preview's is a 409, and the stage is gone.
	if code := apiCall(t, m, "POST", "/api/plugins/install/confirm", map[string]any{"token": p.Token, "fingerprint": "beef", "enable": true}, nil); code != http.StatusConflict {
		t.Fatalf("confirm with a stale fingerprint = %d; want 409", code)
	}
	if code := apiCall(t, m, "POST", "/api/plugins/install/confirm", map[string]any{"token": p.Token, "fingerprint": p.Fingerprint, "enable": true}, nil); code != http.StatusNotFound {
		t.Fatalf("a used token = %d; want 404", code)
	}

	// Cancel discards a stage.
	apiCall(t, m, "POST", "/api/plugins/install/preview", map[string]string{"source": "owner/hello"}, &p)
	apiCall(t, m, "POST", "/api/plugins/install/cancel", map[string]string{"token": p.Token}, nil)
	if st := stagingEntries(t, m); len(st) != 0 {
		t.Fatalf("staging after cancel = %v", st)
	}

	// A good confirm installs, records, approves exactly the preview's fingerprint.
	apiCall(t, m, "POST", "/api/plugins/install/preview", map[string]string{"source": "https://github.com/owner/hello"}, &p)
	var l pluginsPayload
	if code := apiCall(t, m, "POST", "/api/plugins/install/confirm", map[string]any{"token": p.Token, "fingerprint": p.Fingerprint, "enable": true}, &l); code != 200 {
		t.Fatalf("confirm = %d", code)
	}
	got := pluginByName(t, l, "hello")
	if got.State != pluginStateEnabled || got.Source.Kind != pluginSourceGitHub || got.Source.Commit != c1 || got.Source.Source != "owner/hello" || got.Source.URL == "" {
		t.Fatalf("installed = %+v", got)
	}
	if got.DataDir != filepath.Join(filepath.Dir(dir), "plugin-data", "hello") {
		t.Errorf("data_dir = %q", got.DataDir)
	}
	if loadPluginGrants()["hello"].Approved != p.Fingerprint {
		t.Error("the approval is not the preview's fingerprint")
	}
	if _, err := os.Stat(filepath.Join(dir, "hello", ".git")); err == nil {
		t.Error(".git was installed")
	}
	if st := stagingEntries(t, m); len(st) != 0 {
		t.Fatalf("staging after confirm = %v", st)
	}

	// Installing it again is refused, pointing at update.
	code := apiCall(t, m, "POST", "/api/plugins/install/preview", map[string]string{"source": "owner/hello"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("second install = %d", code)
	}
	if _, err := m.installPreview(context.Background(), "owner/hello", ""); err == nil || !strings.Contains(err.Error(), "use update") {
		t.Fatalf("second install err = %v", err)
	}
}

func TestPluginInstallDisabledAndByRef(t *testing.T) {
	gh := newFakeGitHub(t)
	m, _ := testPluginManager(t)
	c1 := gh.commit("hello", map[string]string{"plugin.json": helloManifest("0.1.0", "main")})
	gh.commit("hello", map[string]string{"plugin.json": helloManifest("0.2.0", "main")})
	gitRun(t, gh.works["hello"], "push", "-q", "origin", c1+":refs/tags/v1")

	for _, ref := range []string{"v1", c1} {
		p, err := m.installPreview(context.Background(), "owner/hello", ref)
		if err != nil {
			t.Fatalf("ref %s: %v", ref, err)
		}
		if p.Commit != c1 || p.Version != "0.1.0" || p.Ref != ref {
			t.Fatalf("ref %s: preview = %+v", ref, p)
		}
		m.cancelStage(p.Token)
	}
	p, _ := m.installPreview(context.Background(), "owner/hello", "v1")
	if err := m.installConfirm(p.Token, p.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	got := pluginByName(t, m.listing(), "hello")
	if got.State != pluginStateDisabled || got.Source.Ref != "v1" {
		t.Fatalf("install only = %+v", got)
	}
}

func TestPluginInstallRefusals(t *testing.T) {
	gh := newFakeGitHub(t)
	m, dir := testPluginManager(t)
	ctx := context.Background()

	// Subdirectory install, and hand-placed collision.
	gh.commit("mono", map[string]string{"plugins/hello/plugin.json": helloManifest("1", "main"), "README": "x"})
	writePlugin(t, dir, "hello", map[string]any{"name": "hello"}, nil)
	m.rescan()
	if _, err := m.installPreview(ctx, "owner/mono/plugins/hello", ""); err == nil || !strings.Contains(err.Error(), "hand-placed") {
		t.Fatalf("collision with a hand-placed plugin: %v", err)
	}
	_ = os.RemoveAll(filepath.Join(dir, "hello"))
	if p, err := m.installPreview(ctx, "owner/mono/plugins/hello", ""); err != nil || p.Source != "owner/mono/plugins/hello" {
		t.Fatalf("subdir install: %+v %v", p, err)
	}
	if _, err := m.installPreview(ctx, "owner/mono/plugins/nope", ""); err == nil {
		t.Fatal("a missing subdirectory was accepted")
	}
	// No manifest at the root.
	if _, err := m.installPreview(ctx, "owner/mono", ""); err == nil || !strings.Contains(err.Error(), "no plugin.json") {
		t.Fatalf("no manifest: %v", err)
	}

	// Size caps.
	gh.commit("big", map[string]string{"plugin.json": manifestJSON(map[string]any{"name": "big"}), "a": "1", "b": "2", "c": strings.Repeat("x", 4096)})
	oldFiles, oldBytes := pluginInstallMaxFiles, pluginInstallMaxBytes
	t.Cleanup(func() { pluginInstallMaxFiles, pluginInstallMaxBytes = oldFiles, oldBytes })
	pluginInstallMaxFiles = 3
	if _, err := m.installPreview(ctx, "owner/big", ""); err == nil || !strings.Contains(err.Error(), "more than 3 files") {
		t.Fatalf("file cap: %v", err)
	}
	pluginInstallMaxFiles, pluginInstallMaxBytes = 5000, 1024
	if _, err := m.installPreview(ctx, "owner/big", ""); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("byte cap: %v", err)
	}
	pluginInstallMaxBytes = oldBytes

	// Symlinks out of the plugin, absolute or climbing.
	gh.commit("evil", map[string]string{"plugins/evil/plugin.json": manifestJSON(map[string]any{"name": "evil"}), "plugins/evil/ui": "->../../secret", "secret": "s"})
	if _, err := m.installPreview(ctx, "owner/evil/plugins/evil", ""); err == nil || !strings.Contains(err.Error(), "points outside") {
		t.Fatalf("climbing symlink: %v", err)
	}
	gh.commit("evil2", map[string]string{"plugin.json": manifestJSON(map[string]any{"name": "evil2"}), "passwd": "->/etc/passwd"})
	if _, err := m.installPreview(ctx, "owner/evil2", ""); err == nil || !strings.Contains(err.Error(), "points outside") {
		t.Fatalf("absolute symlink: %v", err)
	}
	// An inside symlink is fine.
	gh.commit("okl", map[string]string{"plugin.json": manifestJSON(map[string]any{"name": "okl"}), "a.txt": "x", "b.txt": "->a.txt"})
	if _, err := m.installPreview(ctx, "owner/okl", ""); err != nil {
		t.Fatalf("inside symlink refused: %v", err)
	}
	// Every refusal cleaned its staging up; only the live previews remain.
	m.stageMu.Lock()
	live := len(m.staged)
	m.stageMu.Unlock()
	if st := stagingEntries(t, m); len(st) != live {
		t.Fatalf("staging = %v with %d live previews", st, live)
	}
}

func TestPluginStagingSweep(t *testing.T) {
	gh := newFakeGitHub(t)
	m, _ := testPluginManager(t)
	gh.commit("hello", map[string]string{"plugin.json": helloManifest("1")})
	p, err := m.installPreview(context.Background(), "owner/hello", "")
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(m.stagingRoot(), "stray")
	_ = os.MkdirAll(stray, 0o700)

	// A periodic sweep leaves a fresh stray (a clone in progress) and the live stage.
	m.sweepStaging(false)
	if st := stagingEntries(t, m); len(st) != 2 {
		t.Fatalf("staging after a fresh sweep = %v", st)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(stray, old, old)
	m.sweepStaging(false)
	if st := stagingEntries(t, m); !slices.Equal(st, []string{p.Token}) {
		t.Fatalf("staging after sweeping an old stray = %v", st)
	}
	// Expiry: the stage goes, and its token no longer confirms.
	oldTTL := pluginStageTTL
	pluginStageTTL = time.Millisecond
	t.Cleanup(func() { pluginStageTTL = oldTTL })
	time.Sleep(5 * time.Millisecond)
	m.sweepStaging(false)
	if st := stagingEntries(t, m); len(st) != 0 {
		t.Fatalf("staging after expiry = %v", st)
	}
	if err := m.installConfirm(p.Token, p.Fingerprint, true); !errors.Is(err, errNoStage) {
		t.Fatalf("expired confirm = %v", err)
	}
	// Boot sweep removes everything nobody owns.
	pluginStageTTL = oldTTL
	_ = os.MkdirAll(stray, 0o700)
	m.sweepStaging(true)
	if st := stagingEntries(t, m); len(st) != 0 {
		t.Fatalf("staging after the boot sweep = %v", st)
	}
}

func TestPluginLinkUnlink(t *testing.T) {
	m, dir := testPluginManager(t)
	ext := filepath.Join(t.TempDir(), "my-checkout")
	writePlugin(t, filepath.Dir(ext), "my-checkout", helloManifest("1", "main"), map[string]string{"ui/main.html": "x"})

	if _, err := m.link("relative/path", false); err == nil {
		t.Fatal("a relative path was linked")
	}
	inside := writePlugin(t, dir, "inside", map[string]any{"name": "inside"}, nil)
	if _, err := m.link(inside, false); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("a path inside plugins/ was linked: %v", err)
	}

	var l pluginsPayload
	if code := apiCall(t, m, "POST", "/api/plugins/link", map[string]any{"path": ext, "enable": true}, &l); code != 200 {
		t.Fatalf("link = %d", code)
	}
	p := pluginByName(t, l, "hello")
	if p.Source.Kind != pluginSourceLinked || p.Source.Path != ext || p.Dir != ext || p.State != pluginStateEnabled {
		t.Fatalf("linked = %+v", p)
	}
	// Served from the linked path.
	rec := httptest.NewRecorder()
	m.serveFiles(rec, httptest.NewRequest("GET", "/plugins/hello/ui/main.html", nil))
	if rec.Code != 200 || rec.Body.String() != "x" {
		t.Fatalf("linked file = %d %q", rec.Code, rec.Body.String())
	}
	// A second link of the same name is refused.
	if _, err := m.link(ext, false); err == nil || !strings.Contains(err.Error(), "linked") {
		t.Fatalf("double link: %v", err)
	}
	// Uninstall refuses a linked plugin.
	if err := m.uninstall("hello", false); err == nil || !strings.Contains(err.Error(), "unlink") {
		t.Fatalf("uninstall of a linked plugin: %v", err)
	}

	// The path vanishing lists the plugin invalid, with the reason.
	moved := ext + ".moved"
	_ = os.Rename(ext, moved)
	m.rescan()
	if p := pluginByName(t, m.listing(), "hello"); p.State != pluginStateInvalid || !strings.Contains(p.Error, "no longer exists") {
		t.Fatalf("vanished link = %+v", p)
	}
	_ = os.Rename(moved, ext)

	if code := apiCall(t, m, "POST", "/api/plugins/hello/unlink", nil, &l); code != 200 {
		t.Fatalf("unlink = %d", code)
	}
	if findPluginPayload(l, "hello") != nil {
		t.Fatal("still listed after unlink")
	}
	if _, err := os.Stat(filepath.Join(ext, "plugin.json")); err != nil {
		t.Fatal("unlink touched the files")
	}
	if _, ok := loadPluginGrants()["hello"]; ok {
		t.Fatal("unlink left the approval behind")
	}
	// Unlink of a hand-placed plugin is refused.
	m.rescan()
	if err := m.unlink("inside"); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("unlink of a local plugin: %v", err)
	}
}

func TestPluginUninstall(t *testing.T) {
	gh := newFakeGitHub(t)
	m, dir := testPluginManager(t)
	writePlugin(t, dir, "handmade", map[string]any{"name": "handmade"}, nil)
	m.rescan()
	if err := m.uninstall("handmade", false); err == nil || !strings.Contains(err.Error(), "delete the directory yourself") {
		t.Fatalf("uninstall of a hand-placed plugin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "handmade")); err != nil {
		t.Fatal("a refused uninstall removed the directory")
	}

	gh.commit("hello", map[string]string{"plugin.json": helloManifest("1")})
	p, _ := m.installPreview(context.Background(), "owner/hello", "")
	if err := m.installConfirm(p.Token, p.Fingerprint, true); err != nil {
		t.Fatal(err)
	}
	data := m.dataDir("hello")
	_ = os.MkdirAll(data, 0o700)
	_ = os.WriteFile(filepath.Join(data, "state"), []byte("keep"), 0o600)

	// A github record whose directory is a symlink is refused.
	final := filepath.Join(dir, "hello")
	real := filepath.Join(t.TempDir(), "real")
	_ = os.Rename(final, real)
	_ = os.Symlink(real, final)
	if err := m.uninstall("hello", false); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("uninstall through a symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "plugin.json")); err != nil {
		t.Fatal("the symlink's target was touched")
	}
	_ = os.Remove(final)
	_ = os.Rename(real, final)

	var l pluginsPayload
	if code := apiCall(t, m, "POST", "/api/plugins/hello/uninstall", map[string]bool{"purge_data": false}, &l); code != 200 {
		t.Fatalf("uninstall = %d", code)
	}
	if findPluginPayload(l, "hello") != nil {
		t.Fatal("still listed")
	}
	if _, err := os.Stat(final); err == nil {
		t.Fatal("directory still there")
	}
	if _, err := os.Stat(filepath.Join(data, "state")); err != nil {
		t.Fatal("the data dir was removed without purge_data")
	}
	if _, ok := loadPluginSources()["hello"]; ok {
		t.Fatal("record kept")
	}
	if _, ok := loadPluginGrants()["hello"]; ok {
		t.Fatal("approval kept")
	}

	// With purge_data the data goes too.
	p, _ = m.installPreview(context.Background(), "owner/hello", "")
	_ = m.installConfirm(p.Token, p.Fingerprint, false)
	if err := m.uninstall("hello", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(data); err == nil {
		t.Fatal("purge_data left the data dir")
	}
}

func TestPluginUpdate(t *testing.T) {
	gh := newFakeGitHub(t)
	m, dir := testPluginManager(t)
	ctx := context.Background()
	c1 := gh.commit("hello", map[string]string{"plugin.json": helloManifest("0.1.0", "main")})
	p, _ := m.installPreview(ctx, "owner/hello", "")
	if err := m.installConfirm(p.Token, p.Fingerprint, true); err != nil {
		t.Fatal(err)
	}

	// Cosmetic change: same permissions, approval carries over.
	c2 := gh.commit("hello", map[string]string{"plugin.json": helloManifest("0.2.0", "main")})
	var up pluginPreview
	if code := apiCall(t, m, "POST", "/api/plugins/hello/update/preview", nil, &up); code != 200 {
		t.Fatalf("update preview = %d", code)
	}
	if up.CurrentCommit != c1 || up.Commit != c2 || up.ChangesPermissions == nil || *up.ChangesPermissions {
		t.Fatalf("update preview = %+v", up)
	}
	var l pluginsPayload
	if code := apiCall(t, m, "POST", "/api/plugins/hello/update/confirm", map[string]string{"token": up.Token, "fingerprint": up.Fingerprint}, &l); code != 200 {
		t.Fatalf("update confirm = %d", code)
	}
	if got := pluginByName(t, l, "hello"); got.State != pluginStateEnabled || got.Version != "0.2.0" || got.Source.Commit != c2 {
		t.Fatalf("after a cosmetic update = %+v", got)
	}

	// A new tab changes the permissions: needs approval afterwards.
	c3 := gh.commit("hello", map[string]string{"plugin.json": helloManifest("0.3.0", "main", "extra")})
	up, err := m.updatePreview(ctx, "hello")
	if err != nil || up.ChangesPermissions == nil || !*up.ChangesPermissions {
		t.Fatalf("update preview = %+v %v", up, err)
	}
	// install/cancel discards an update preview too.
	apiCall(t, m, "POST", "/api/plugins/install/cancel", map[string]string{"token": up.Token}, nil)
	if err := m.updateConfirm("hello", up.Token, up.Fingerprint); !errors.Is(err, errNoStage) {
		t.Fatalf("confirm after cancel = %v", err)
	}
	up, _ = m.updatePreview(ctx, "hello")

	// A failed swap puts the old version back.
	calls := 0
	pluginRename = func(from, to string) error {
		calls++
		if calls == 2 {
			return errors.New("injected")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { pluginRename = os.Rename })
	if err := m.updateConfirm("hello", up.Token, up.Fingerprint); err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("failed swap = %v", err)
	}
	pluginRename = os.Rename
	m.rescan()
	if got := pluginByName(t, m.listing(), "hello"); got.Version != "0.2.0" || got.State != pluginStateEnabled || got.Source.Commit != c2 {
		t.Fatalf("after the rollback = %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hello", "plugin.json")); !strings.Contains(string(b), "0.2.0") {
		t.Fatalf("rolled-back manifest = %s", b)
	}

	up, _ = m.updatePreview(ctx, "hello")
	if err := m.updateConfirm("hello", up.Token, "stale"); !errors.Is(err, errPluginChanged) {
		t.Fatalf("stale update fingerprint = %v", err)
	}
	up, _ = m.updatePreview(ctx, "hello")
	if err := m.updateConfirm("hello", up.Token, up.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if got := pluginByName(t, m.listing(), "hello"); got.State != pluginStateNeedsApproval || got.Source.Commit != c3 {
		t.Fatalf("after a permission-changing update = %+v", got)
	}

	// update refuses a hand-placed plugin.
	writePlugin(t, dir, "handmade", map[string]any{"name": "handmade"}, nil)
	m.rescan()
	if _, err := m.updatePreview(ctx, "handmade"); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("update of a hand-placed plugin: %v", err)
	}
}

func TestPluginRuntimeRequirements(t *testing.T) {
	oldV, oldOS := lassoSemver, pluginGOOS
	t.Cleanup(func() { lassoSemver, pluginGOOS = oldV, oldOS })
	lassoSemver, pluginGOOS = "3.6.0", "linux"
	cases := []struct {
		m       map[string]any
		wantErr string
	}{
		{map[string]any{"min_lasso_version": "3.6.0"}, ""},
		{map[string]any{"min_lasso_version": "3.5.9"}, ""},
		{map[string]any{"min_lasso_version": "3.7.0"}, "needs lasso >= 3.7.0 (this is 3.6.0)"},
		{map[string]any{"min_lasso_version": "banana"}, "not a version"},
		{map[string]any{"platforms": []string{"linux"}}, ""},
		{map[string]any{"platforms": []string{"macos"}}, "runs only on macos"},
		{map[string]any{"platforms": []string{"darwin", "linux"}}, ""},
		{map[string]any{"platforms": []string{"windows"}}, "not one of"},
	}
	for i, c := range cases {
		c.m["name"] = "hello"
		dir := t.TempDir()
		pd := writePlugin(t, dir, "hello", c.m, nil)
		_, err := loadPluginManifest(pd, "hello")
		if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("case %d %v: err = %v, want %q", i, c.m, err, c.wantErr)
		}
	}
	// A dev build passes whatever the manifest asks.
	lassoSemver = "3.6.0-dev"
	pd := writePlugin(t, t.TempDir(), "hello", map[string]any{"name": "hello", "min_lasso_version": "9.0.0"}, nil)
	if _, err := loadPluginManifest(pd, "hello"); err != nil {
		t.Errorf("dev build refused: %v", err)
	}
	// Neither field is part of the fingerprint.
	a := &pluginManifest{Name: "x"}
	b := &pluginManifest{Name: "x", MinLassoVersion: "1.0.0", Platforms: []string{"linux"}}
	if a.fingerprint() != b.fingerprint() {
		t.Error("min_lasso_version/platforms changed the fingerprint")
	}
}

func TestPluginLogRing(t *testing.T) {
	r := &lineRing{max: 3}
	fmt.Fprint(r, "one\ntwo\nthr")
	if got := r.last(10); !slices.Equal(got, []string{"one", "two", "thr"}) {
		t.Fatalf("with a partial = %v", got)
	}
	fmt.Fprint(r, "ee\nfour\r\nfive\n")
	if got := r.last(10); !slices.Equal(got, []string{"three", "four", "five"}) {
		t.Fatalf("bounded = %v", got)
	}
	if got := r.last(2); !slices.Equal(got, []string{"four", "five"}) {
		t.Fatalf("last(2) = %v", got)
	}

	// A trusted plugin's log is the ring; a sandboxed one without msb says why.
	t.Setenv("LASSO_MSB", "off")
	m, dir := testPluginManager(t)
	// No MCP server: nothing produces output, and the note says so rather than
	// blaming msb or trust.
	writePlugin(t, dir, "quiet", map[string]any{"name": "quiet"}, nil)
	writePlugin(t, dir, "hello", map[string]any{"name": "hello",
		"mcp": map[string]any{"image": "python:3.12-slim", "command": []string{"python3", "s.py"}}}, nil)
	m.rescan()
	var out pluginLogPayload
	if code := apiCall(t, m, "GET", "/api/plugins/quiet/log", nil, &out); code != 200 || out.Sandboxed || !strings.Contains(out.Note, "no MCP server") {
		t.Fatalf("no-mcp log = %d %+v", code, out)
	}
	if code := apiCall(t, m, "GET", "/api/plugins/hello/log?lines=5", nil, &out); code != 200 || !out.Sandboxed || len(out.Lines) != 0 || !strings.Contains(out.Note, "LASSO_MSB=off") {
		t.Fatalf("sandboxed log = %d %+v", code, out)
	}
	_ = m.setTrusted("hello", true)
	m.mu.Lock()
	fmt.Fprint(m.logRingLocked("hello"), "a\nb\nc\n")
	m.mu.Unlock()
	if code := apiCall(t, m, "GET", "/api/plugins/hello/log?lines=2", nil, &out); code != 200 || out.Sandboxed || !slices.Equal(out.Lines, []string{"b", "c"}) {
		t.Fatalf("trusted log = %d %+v", code, out)
	}
	if code := apiCall(t, m, "GET", "/api/plugins/nope/log", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown plugin log = %d", code)
	}
}

func TestNewLogLines(t *testing.T) {
	cases := []struct{ prev, cur, want []string }{
		{nil, []string{"a"}, []string{"a"}},
		{[]string{"a", "b"}, []string{"a", "b", "c"}, []string{"c"}},
		{[]string{"a", "b"}, []string{"b", "c", "d"}, []string{"c", "d"}},
		{[]string{"a", "b"}, []string{"a", "b"}, []string{}},
	}
	for _, c := range cases {
		if got := newLogLines(c.prev, c.cur); !slices.Equal(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("newLogLines(%v, %v) = %v; want %v", c.prev, c.cur, got, c.want)
		}
	}
}

// Production refuses the file transport: the seam is the only way a test's
// file:// clone works.
func TestPluginCloneRefusesFileProtocol(t *testing.T) {
	gh := newFakeGitHub(t)
	m, _ := testPluginManager(t)
	gh.commit("hello", map[string]string{"plugin.json": helloManifest("1")})
	pluginGitAllowFile = false
	if _, err := m.installPreview(context.Background(), "owner/hello", ""); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("file:// clone without the seam: %v", err)
	}
}
