package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Installing plugins: from GitHub (preview → confirm), linked checkouts,
// uninstall, update, per-plugin data directories and logs. Modelled on herdr's
// `plugin install` and Luvus's `module install` — shallow clone into a staging
// directory, show every permission, install only what was shown — with lasso's
// own trust model (fingerprint approval, microVM) unchanged underneath: an
// install is a convenience for getting a directory into plugins/, never a
// grant. A freshly installed plugin is disabled unless the human confirmed
// "install and enable", and then exactly the fingerprint the preview showed is
// approved.

const (
	pluginSourcesKey = "plugin_sources"
	pluginStagingDir = ".staging" // hidden: the scanner and /plugins/ never see it

	pluginSourceGitHub = "github"
	pluginSourceLinked = "linked"
	pluginSourceLocal  = "local"

	pluginLogRingLines = 500
	pluginMaxStages    = 16
)

var (
	// Limits on what an install may bring in. Vars so tests can lower them.
	pluginInstallMaxBytes int64 = 50 << 20
	pluginInstallMaxFiles       = 5000
	pluginStageTTL              = 10 * time.Minute
	pluginCloneTimeout          = 90 * time.Second

	// pluginCloneURL is where a GitHub source is cloned from. A TEST SEAM: tests
	// point it at a local bare repository and set pluginGitAllowFile, since
	// production refuses the file protocol for anything user-supplied.
	pluginCloneURL = func(owner, repo string) string {
		return "https://github.com/" + owner + "/" + repo + ".git"
	}
	pluginGitAllowFile = false

	// pluginRename moves directories during confirm/update; a seam so tests can
	// fail the swap and check the rollback.
	pluginRename = os.Rename

	// pluginGOOS is the platform `platforms` is checked against (a seam).
	pluginGOOS = runtime.GOOS

	ghPartRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	gitRefRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	gitSHAish  = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	errNoStage = errors.New("no such preview (previews expire after 10 minutes and are used once); preview again")
)

// ---------------------------------------------------------------------------
// manifest: where the plugin can run
// ---------------------------------------------------------------------------

// validateRuntime checks min_lasso_version and platforms against this lasso.
func (m *pluginManifest) validateRuntime() error {
	if v := strings.TrimSpace(m.MinLassoVersion); v != "" {
		want, ok := parseSemver(v)
		if !ok {
			return fmt.Errorf("min_lasso_version %q is not a version (X.Y.Z)", v)
		}
		// A dev build or an unparseable running version passes: it is
		// somebody building lasso, who knows better than a manifest.
		if have, ok := parseSemver(lassoSemver); ok && !strings.Contains(lassoSemver, "-dev") {
			if semverLess(have, want) {
				return fmt.Errorf("needs lasso >= %s (this is %s)", v, lassoSemver)
			}
		}
	}
	if len(m.Platforms) > 0 {
		match := false
		for _, p := range m.Platforms {
			switch strings.ToLower(strings.TrimSpace(p)) {
			case "linux":
				match = match || pluginGOOS == "linux"
			case "darwin", "macos":
				match = match || pluginGOOS == "darwin"
			default:
				return fmt.Errorf("platforms: %q is not one of linux, darwin (macos)", p)
			}
		}
		if !match {
			return fmt.Errorf("runs only on %s (this is %s)", strings.Join(m.Platforms, ", "), pluginGOOS)
		}
	}
	return nil
}

func semverLess(a, b [3]int) bool {
	for i := range 3 {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// install records (lasso's db, beside the approvals)
// ---------------------------------------------------------------------------

// pluginSource is how one plugin got here. A plugin directory with no record
// is "local": hand-placed, which is how every plugin worked before installs.
type pluginSource struct {
	Kind        string `json:"kind"`
	Source      string `json:"source,omitempty"` // github: owner/repo[/subdir]
	Ref         string `json:"ref,omitempty"`
	Commit      string `json:"commit,omitempty"`
	InstalledAt string `json:"installed_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	Path        string `json:"path,omitempty"` // linked: the checkout
}

// pluginSourceOut is the listing's view: always a kind, and for a GitHub
// install the URL of the exact commit.
type pluginSourceOut struct {
	pluginSource
	URL string `json:"url,omitempty"`
}

func (s pluginSource) out() pluginSourceOut {
	o := pluginSourceOut{pluginSource: s}
	if o.Kind == "" {
		o.Kind = pluginSourceLocal
	}
	if o.Kind == pluginSourceGitHub {
		if g, err := parsePluginSource(s.Source, ""); err == nil {
			o.URL = g.commitURL(s.Commit)
		}
	}
	return o
}

var pluginSourcesMu sync.Mutex

func loadPluginSources() map[string]pluginSource {
	out := map[string]pluginSource{}
	if db == nil {
		return out
	}
	v, err := getSetting(pluginSourcesKey)
	if err != nil || v == "" {
		return out
	}
	_ = json.Unmarshal([]byte(v), &out)
	return out
}

// setPluginSource writes (or with nil, deletes) one plugin's record.
func setPluginSource(name string, rec *pluginSource) error {
	if db == nil {
		return errors.New("lasso's state db is not open")
	}
	pluginSourcesMu.Lock()
	defer pluginSourcesMu.Unlock()
	all := loadPluginSources()
	if rec == nil {
		delete(all, name)
	} else {
		all[name] = *rec
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return setSetting(pluginSourcesKey, string(b))
}

// forgetPlugin drops everything lasso's db holds about a plugin: its install
// record, its approval and its trust flag.
func forgetPlugin(name string) error {
	if err := setPluginSource(name, nil); err != nil {
		return err
	}
	return updatePluginGrant(name, func(g *pluginGrant) { *g = pluginGrant{} })
}

// scan is scanPlugins plus the linked checkouts the records name, each entry
// carrying its source.
func (m *pluginManager) scan() map[string]*pluginEntry {
	entries := scanPlugins(m.dir)
	recs := loadPluginSources()
	for name, e := range entries {
		if r, ok := recs[name]; ok && r.Kind == pluginSourceGitHub {
			e.Src = r
		}
	}
	names := make([]string, 0, len(recs))
	for n := range recs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		r := recs[name]
		if r.Kind != pluginSourceLinked {
			continue
		}
		if _, clash := entries[name]; clash {
			// A directory of the same name appeared in plugins/ after the link;
			// it wins (it is what the operator can see), the link waits.
			continue
		}
		e := &pluginEntry{Name: name, Dir: r.Path, Src: r}
		switch st, err := os.Stat(r.Path); {
		case !pluginNameRE.MatchString(name):
			e.Err = fmt.Sprintf("linked name %q is not a valid plugin name", name)
		case err != nil:
			e.Err = fmt.Sprintf("linked path %s: %v", r.Path, errors.Unwrap(err))
			if errors.Is(err, fs.ErrNotExist) {
				e.Err = fmt.Sprintf("linked path %s no longer exists", r.Path)
			}
		case !st.IsDir():
			e.Err = fmt.Sprintf("linked path %s is not a directory", r.Path)
		default:
			man, err := loadPluginManifest(r.Path, "")
			switch {
			case err != nil:
				e.Err = err.Error()
			case man.Name != name:
				e.Err = fmt.Sprintf("the linked manifest is now named %q, not %q; unlink and link again", man.Name, name)
			default:
				e.Man, e.FP = man, man.fingerprint()
			}
		}
		entries[name] = e
	}
	return entries
}

// dataDir is a plugin's one writable directory. Not created here: its server
// creates it on first start (0700).
func (m *pluginManager) dataDir(name string) string {
	return filepath.Join(m.dataRoot, name)
}

// logRingLocked returns (creating) a plugin's stderr ring. m.mu held.
func (m *pluginManager) logRingLocked(name string) *lineRing {
	r := m.logs[name]
	if r == nil {
		r = &lineRing{max: pluginLogRingLines}
		m.logs[name] = r
	}
	return r
}

// hold / unhold bracket a directory swap or removal (see pluginManager.held).
func (m *pluginManager) hold(name string) {
	m.mu.Lock()
	m.held[name] = true
	m.mu.Unlock()
	m.reconcile() // stops its server, synchronously
}

func (m *pluginManager) unhold(name string) {
	m.mu.Lock()
	delete(m.held, name)
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// sources
// ---------------------------------------------------------------------------

// ghSource is a parsed GitHub plugin source.
type ghSource struct {
	Owner, Repo, Subdir, Ref string
}

// String is the canonical spelling stored in the record: owner/repo[/subdir].
func (g ghSource) String() string {
	s := g.Owner + "/" + g.Repo
	if g.Subdir != "" {
		s += "/" + g.Subdir
	}
	return s
}

func (g ghSource) commitURL(commit string) string {
	if commit == "" {
		return "https://github.com/" + g.Owner + "/" + g.Repo
	}
	u := "https://github.com/" + g.Owner + "/" + g.Repo + "/tree/" + commit
	if g.Subdir != "" {
		u += "/" + g.Subdir
	}
	return u
}

// parsePluginSource accepts owner/repo, owner/repo/sub/dir, github.com/…, and
// https://github.com/owner/repo[.git][/tree/<ref>/sub/dir]. GitHub only.
func parsePluginSource(src, ref string) (ghSource, error) {
	src = strings.TrimSpace(src)
	ref = strings.TrimSpace(ref)
	if src == "" {
		return ghSource{}, errors.New("empty source (expected owner/repo[/subdir])")
	}
	var segs []string
	urlRef := ""
	switch {
	case strings.Contains(src, "://"):
		u, err := url.Parse(src)
		if err != nil {
			return ghSource{}, fmt.Errorf("source %q is not a URL", src)
		}
		if u.Scheme != "https" || (u.Host != "github.com" && u.Host != "www.github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return ghSource{}, fmt.Errorf("source %q: only https://github.com/<owner>/<repo> is supported", src)
		}
		segs = strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(segs) >= 3 {
			if segs[2] != "tree" || len(segs) < 4 {
				return ghSource{}, fmt.Errorf("source %q: expected https://github.com/<owner>/<repo>[/tree/<ref>/<subdir>]", src)
			}
			urlRef = segs[3]
			segs = append(segs[:2:2], segs[4:]...)
		}
	case strings.Contains(src, ":") || strings.Contains(src, "@"):
		return ghSource{}, fmt.Errorf("source %q: only GitHub sources (owner/repo) are supported", src)
	default:
		s := strings.TrimPrefix(src, "github.com/")
		if strings.HasPrefix(s, "/") {
			return ghSource{}, fmt.Errorf("source %q: expected owner/repo[/subdir], not a path (use link for a local checkout)", src)
		}
		segs = strings.Split(s, "/")
	}
	if len(segs) < 2 {
		return ghSource{}, fmt.Errorf("source %q: expected owner/repo[/subdir]", src)
	}
	g := ghSource{Owner: segs[0], Repo: strings.TrimSuffix(segs[1], ".git")}
	for _, part := range []string{g.Owner, g.Repo} {
		if !ghPartRE.MatchString(part) || part == "." || part == ".." {
			return ghSource{}, fmt.Errorf("source %q: %q is not a GitHub owner or repository name", src, part)
		}
	}
	if sub := strings.Trim(strings.Join(segs[2:], "/"), "/"); sub != "" {
		c, err := cleanPluginPath(sub)
		if err != nil {
			return ghSource{}, fmt.Errorf("source %q: subdirectory: %v", src, err)
		}
		g.Subdir = c
	}
	switch {
	case ref != "" && urlRef != "" && ref != urlRef:
		return ghSource{}, fmt.Errorf("source names ref %q but %q was also given", urlRef, ref)
	case ref == "":
		ref = urlRef
	}
	if ref != "" {
		if !gitRefRE.MatchString(ref) || strings.Contains(ref, "..") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") {
			return ghSource{}, fmt.Errorf("ref %q is not a branch, tag or commit name", ref)
		}
		g.Ref = ref
	}
	return g, nil
}

// ---------------------------------------------------------------------------
// cloning and staging
// ---------------------------------------------------------------------------

// pluginGit runs one git command without a shell: hooks off, the file and ext
// transports refused, no prompts, no LFS smudge, and the minimal child env.
func pluginGit(ctx context.Context, dir string, args ...string) (string, error) {
	fileProto := "never"
	if pluginGitAllowFile {
		fileProto = "always"
	}
	full := append([]string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=" + fileProto,
		"-c", "protocol.ext.allow=never",
		"-c", "advice.detachedHead=false",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = append(minimalChildEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1", "GCM_INTERACTIVE=never")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// git's progress line names the staging path, which is noise to a
		// human reading why an install failed.
		var keep []string
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if !strings.HasPrefix(l, "Cloning into ") {
				keep = append(keep, l)
			}
		}
		msg := strings.TrimSpace(strings.Join(keep, "\n"))
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s timed out", args[0])
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], clipLine(strings.ReplaceAll(msg, "\n", " | "), 600))
	}
	return strings.TrimSpace(string(out)), nil
}

// cloneGitHub shallow-clones g into dest and returns the commit it checked out.
func cloneGitHub(ctx context.Context, g ghSource, dest string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git is not installed on lasso's machine")
	}
	ctx, cancel := context.WithTimeout(ctx, pluginCloneTimeout)
	defer cancel()
	u := pluginCloneURL(g.Owner, g.Repo)
	parent := filepath.Dir(dest)
	sha := g.Ref != "" && gitSHAish.MatchString(g.Ref)
	args := []string{"clone", "--depth", "1", "--no-recurse-submodules"}
	if g.Ref != "" && !sha {
		args = append(args, "--branch", g.Ref)
	}
	args = append(args, "--single-branch", "--", u, dest)
	if _, err := pluginGit(ctx, parent, args...); err != nil {
		// With prompts off, GitHub's answer for a missing or private repository
		// is a refused credential prompt, which reads like a lasso bug.
		if msg := err.Error(); strings.Contains(msg, "could not read Username") || strings.Contains(msg, "Repository not found") {
			return "", fmt.Errorf("github.com/%s/%s was not found (or is private; only public repositories can be installed)", g.Owner, g.Repo)
		}
		return "", err
	}
	if sha {
		if _, err := pluginGit(ctx, dest, "fetch", "--depth", "1", "--no-recurse-submodules", "origin", g.Ref); err != nil {
			return "", err
		}
		if _, err := pluginGit(ctx, dest, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return "", err
		}
	}
	commit, err := pluginGit(ctx, dest, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return commit, nil
}

// checkCheckout enforces the size caps over the whole checkout and refuses a
// symlink in the plugin's own directory that points outside it (absolute, or
// climbing out) — once moved, such a link would point at whatever is there.
func checkCheckout(checkout, pluginDir string) error {
	var files int
	var bytes int64
	err := filepath.WalkDir(checkout, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == checkout || d.IsDir() {
			return nil
		}
		files++
		if files > pluginInstallMaxFiles {
			return fmt.Errorf("the checkout has more than %d files", pluginInstallMaxFiles)
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
			if bytes > pluginInstallMaxBytes {
				return fmt.Errorf("the checkout is larger than %d MB", pluginInstallMaxBytes>>20)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return filepath.WalkDir(pluginDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		rel, _ := filepath.Rel(pluginDir, p)
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		if filepath.IsAbs(target) {
			return fmt.Errorf("symlink %s points outside the plugin (%s)", rel, target)
		}
		r, err := filepath.Rel(pluginDir, filepath.Join(filepath.Dir(p), target))
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return fmt.Errorf("symlink %s points outside the plugin (%s)", rel, target)
		}
		return nil
	})
}

// pluginStage is one preview waiting for its confirm.
type pluginStage struct {
	token   string
	kind    string // "install" | "update"
	target  string // update: the plugin being updated
	root    string // <plugins>/.staging/<token>, removed when the stage ends
	dir     string // the plugin directory inside the checkout
	src     ghSource
	commit  string
	man     *pluginManifest
	fp      string
	created time.Time
}

func (m *pluginManager) stagingRoot() string { return filepath.Join(m.dir, pluginStagingDir) }

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// stage clones g into a fresh staging directory and validates what it finds
// exactly as the scanner would. The stage is not registered; the caller does.
func (m *pluginManager) stage(ctx context.Context, g ghSource) (*pluginStage, error) {
	m.stageMu.Lock()
	n := len(m.staged)
	m.stageMu.Unlock()
	if n >= pluginMaxStages {
		return nil, fmt.Errorf("%d previews are already waiting; confirm or cancel one first", n)
	}
	token := randomToken()
	root := filepath.Join(m.stagingRoot(), token)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	fail := func(err error) (*pluginStage, error) {
		_ = os.RemoveAll(root)
		return nil, err
	}
	checkout := filepath.Join(root, "repo")
	commit, err := cloneGitHub(ctx, g, checkout)
	if err != nil {
		return fail(err)
	}
	if err := os.RemoveAll(filepath.Join(checkout, ".git")); err != nil {
		return fail(err)
	}
	dir := checkout
	if g.Subdir != "" {
		dir = filepath.Join(checkout, filepath.FromSlash(g.Subdir))
	}
	// The subdirectory itself must be a real directory in the checkout, not
	// reached through a symlink anywhere along its path.
	realCheckout, err1 := filepath.EvalSymlinks(checkout)
	realDir, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil {
		return fail(fmt.Errorf("%s has no directory %q", g.Owner+"/"+g.Repo, g.Subdir))
	}
	if want := filepath.Join(realCheckout, filepath.FromSlash(g.Subdir)); realDir != want {
		return fail(fmt.Errorf("subdirectory %q is reached through a symlink", g.Subdir))
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fail(fmt.Errorf("%q is not a directory in %s", g.Subdir, g.Owner+"/"+g.Repo))
	}
	if err := checkCheckout(checkout, dir); err != nil {
		return fail(err)
	}
	man, err := loadPluginManifest(dir, "")
	if err != nil {
		return fail(fmt.Errorf("%s: %v", g.String(), err))
	}
	return &pluginStage{
		token: token, root: root, dir: dir, src: g, commit: commit,
		man: man, fp: man.fingerprint(), created: time.Now(),
	}, nil
}

func (m *pluginManager) putStage(st *pluginStage) {
	m.stageMu.Lock()
	m.staged[st.token] = st
	m.stageMu.Unlock()
}

// takeStage removes and returns a live stage of the given kind.
func (m *pluginManager) takeStage(token, kind string) (*pluginStage, error) {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	st := m.staged[token]
	if st == nil || (kind != "" && st.kind != kind) {
		return nil, errNoStage
	}
	delete(m.staged, token)
	if time.Since(st.created) > pluginStageTTL {
		_ = os.RemoveAll(st.root)
		return nil, errNoStage
	}
	return st, nil
}

// sweepStaging drops expired previews and any staging directory no live
// preview owns. all (at boot) removes every unowned directory; otherwise only
// ones older than the TTL, so a clone in progress is left alone.
func (m *pluginManager) sweepStaging(all bool) {
	m.stageMu.Lock()
	owned := map[string]bool{}
	for tok, st := range m.staged {
		if time.Since(st.created) > pluginStageTTL {
			delete(m.staged, tok)
			_ = os.RemoveAll(st.root)
			continue
		}
		owned[tok] = true
	}
	m.stageMu.Unlock()
	ents, err := os.ReadDir(m.stagingRoot())
	if err != nil {
		return
	}
	for _, de := range ents {
		if owned[de.Name()] {
			continue
		}
		p := filepath.Join(m.stagingRoot(), de.Name())
		if !all {
			info, err := de.Info()
			if err != nil || time.Since(info.ModTime()) <= pluginStageTTL {
				continue
			}
		}
		_ = os.RemoveAll(p)
	}
}

// ---------------------------------------------------------------------------
// preview payload
// ---------------------------------------------------------------------------

type pluginPreview struct {
	Token       string           `json:"token"`
	Name        string           `json:"name"`
	Version     string           `json:"version"`
	Description string           `json:"description"`
	Source      string           `json:"source"`
	Ref         string           `json:"ref"`
	Commit      string           `json:"commit"`
	Fingerprint string           `json:"fingerprint"`
	Permissions pluginPerms      `json:"permissions"`
	Themes      []pluginThemeOut `json:"themes"`
	Fonts       []pluginFontOut  `json:"fonts"`
	Warnings    []string         `json:"warnings"`
	// Update previews only.
	CurrentCommit      string `json:"current_commit,omitempty"`
	ChangesPermissions *bool  `json:"changes_permissions,omitempty"`
}

func (st *pluginStage) preview() pluginPreview {
	p := pluginPayload{Themes: []pluginThemeOut{}, Fonts: []pluginFontOut{}, Warnings: []string{}}
	appearanceListing(&p, &pluginEntry{Name: st.man.Name, Man: st.man}, false)
	return pluginPreview{
		Token: st.token, Name: st.man.Name, Version: st.man.Version, Description: st.man.Description,
		Source: st.src.String(), Ref: st.src.Ref, Commit: st.commit, Fingerprint: st.fp,
		Permissions: st.man.perms(), Themes: p.Themes, Fonts: p.Fonts, Warnings: p.Warnings,
	}
}

// ---------------------------------------------------------------------------
// install
// ---------------------------------------------------------------------------

// nameTaken refuses a name that already belongs to a plugin, saying which kind.
func (m *pluginManager) nameTaken(name string) error {
	recs := loadPluginSources()
	r, hasRec := recs[name]
	if hasRec && r.Kind == pluginSourceLinked {
		return fmt.Errorf("plugin %q is already linked from %s; unlink it first", name, r.Path)
	}
	final := filepath.Join(m.dir, name)
	if _, err := os.Lstat(final); err == nil {
		if hasRec && r.Kind == pluginSourceGitHub {
			return fmt.Errorf("plugin %q is already installed from github %s; use update", name, r.Source)
		}
		return fmt.Errorf("a hand-placed plugin %q already exists at %s", name, final)
	}
	return nil
}

func (m *pluginManager) installPreview(ctx context.Context, source, ref string) (pluginPreview, error) {
	g, err := parsePluginSource(source, ref)
	if err != nil {
		return pluginPreview{}, err
	}
	st, err := m.stage(ctx, g)
	if err != nil {
		return pluginPreview{}, err
	}
	if err := m.nameTaken(st.man.Name); err != nil {
		_ = os.RemoveAll(st.root)
		return pluginPreview{}, err
	}
	st.kind = "install"
	m.putStage(st)
	return st.preview(), nil
}

// restage re-reads a stage's manifest: the one approved is the one on disk at
// confirm time, and it must still be the one the human saw.
func (st *pluginStage) verify(want string) error {
	man, err := loadPluginManifest(st.dir, "")
	if err != nil || man.fingerprint() != st.fp || want != st.fp || man.Name != st.man.Name {
		return errPluginChanged
	}
	return nil
}

func (m *pluginManager) installConfirm(token, fp string, enable bool) error {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	st, err := m.takeStage(token, "install")
	if err != nil {
		return err
	}
	defer os.RemoveAll(st.root)
	if err := st.verify(fp); err != nil {
		return err
	}
	name := st.man.Name
	if err := m.nameTaken(name); err != nil {
		return err
	}
	if err := pluginRename(st.dir, filepath.Join(m.dir, name)); err != nil {
		return fmt.Errorf("move the plugin into place: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := setPluginSource(name, &pluginSource{Kind: pluginSourceGitHub, Source: st.src.String(), Ref: st.src.Ref, Commit: st.commit, InstalledAt: now}); err != nil {
		return err
	}
	// A fresh install starts from nothing — a grant left behind by some earlier
	// plugin of the same name must not approve this one — and "install and
	// enable" approves exactly what the preview showed.
	if err := updatePluginGrant(name, func(g *pluginGrant) {
		*g = pluginGrant{}
		if enable {
			g.Approved = st.fp
		}
	}); err != nil {
		return err
	}
	log.Printf("plugins:  %s installed from github %s@%s%s", name, st.src.String(), shortCommit(st.commit), map[bool]string{true: " and enabled", false: ""}[enable])
	m.rescan()
	return nil
}

func (m *pluginManager) cancelStage(token string) {
	m.stageMu.Lock()
	st := m.staged[token]
	delete(m.staged, token)
	m.stageMu.Unlock()
	if st != nil {
		_ = os.RemoveAll(st.root)
	}
}

func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

// ---------------------------------------------------------------------------
// link / unlink / uninstall
// ---------------------------------------------------------------------------

// link registers a local checkout in place. enable approves the manifest's
// fingerprint as read here, in the same step — what the caller showed the
// human is the manifest at that path, and a later edit to it reads
// needs_approval by the ordinary rule.
func (m *pluginManager) link(p string, enable bool) (string, error) {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be absolute", p)
	}
	p = filepath.Clean(p)
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", p)
	}
	for _, under := range []string{m.dir, m.dataRoot} {
		if r, err := filepath.Rel(under, p); err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s is inside %s; a plugin there is discovered without a link", p, under)
		}
	}
	man, err := loadPluginManifest(p, "")
	if err != nil {
		return "", fmt.Errorf("%s: %v", p, err)
	}
	name := man.Name
	if err := m.nameTaken(name); err != nil {
		return "", err
	}
	if e := m.entry(name); e != nil {
		return "", fmt.Errorf("a plugin named %q already exists (%s)", name, e.Dir)
	}
	if err := setPluginSource(name, &pluginSource{Kind: pluginSourceLinked, Path: p, InstalledAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		return "", err
	}
	if err := updatePluginGrant(name, func(g *pluginGrant) {
		*g = pluginGrant{}
		if enable {
			g.Approved = man.fingerprint()
		}
	}); err != nil {
		return "", err
	}
	log.Printf("plugins:  %s linked from %s%s", name, p, map[bool]string{true: " and enabled", false: ""}[enable])
	m.rescan()
	return name, nil
}

func (m *pluginManager) unlink(name string) error {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	r, ok := loadPluginSources()[name]
	if !ok || r.Kind != pluginSourceLinked {
		return m.wrongKind(name, pluginSourceLinked)
	}
	if err := forgetPlugin(name); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.logs, name)
	m.mu.Unlock()
	log.Printf("plugins:  %s unlinked (files at %s left alone)", name, r.Path)
	m.rescan()
	return nil
}

// wrongKind explains why an operation for one kind of plugin does not apply.
func (m *pluginManager) wrongKind(name, want string) error {
	r, hasRec := loadPluginSources()[name]
	e := m.entry(name)
	if e == nil {
		m.rescan() // a directory placed since the last scan
		e = m.entry(name)
	}
	switch {
	case hasRec && r.Kind == pluginSourceLinked:
		return fmt.Errorf("plugin %q is linked from %s; use unlink", name, r.Path)
	case hasRec && r.Kind == pluginSourceGitHub && want == pluginSourceLinked:
		return fmt.Errorf("plugin %q was installed from github; use uninstall", name)
	case e != nil:
		return fmt.Errorf("plugin %q was placed by hand in %s; delete the directory yourself", name, e.Dir)
	}
	return errPluginNotFound
}

func (m *pluginManager) uninstall(name string, purgeData bool) error {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	r, ok := loadPluginSources()[name]
	if !ok || r.Kind != pluginSourceGitHub {
		return m.wrongKind(name, pluginSourceGitHub)
	}
	final := filepath.Join(m.dir, name)
	if filepath.Dir(final) != filepath.Clean(m.dir) {
		return fmt.Errorf("refusing to remove %s: not directly inside %s", final, m.dir)
	}
	st, err := os.Lstat(final)
	exists := err == nil
	if exists {
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to remove %s: it is a symlink", final)
		}
		if !st.IsDir() {
			return fmt.Errorf("refusing to remove %s: not a directory", final)
		}
	}
	m.hold(name)
	defer m.unhold(name)
	if err := updatePluginGrant(name, func(g *pluginGrant) { *g = pluginGrant{} }); err != nil {
		return err
	}
	if exists {
		if err := os.RemoveAll(final); err != nil {
			return fmt.Errorf("remove %s: %v", final, err)
		}
	}
	if err := forgetPlugin(name); err != nil {
		return err
	}
	if purgeData {
		d := m.dataDir(name)
		if st, err := os.Lstat(d); err == nil {
			if st.Mode()&fs.ModeSymlink != 0 {
				_ = os.Remove(d)
			} else {
				_ = os.RemoveAll(d)
			}
		}
	}
	m.mu.Lock()
	delete(m.logs, name)
	m.mu.Unlock()
	log.Printf("plugins:  %s uninstalled%s", name, map[bool]string{true: " (data purged)", false: ""}[purgeData])
	m.unhold(name)
	m.rescan()
	return nil
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

func (m *pluginManager) updatePreview(ctx context.Context, name string) (pluginPreview, error) {
	r, ok := loadPluginSources()[name]
	if !ok || r.Kind != pluginSourceGitHub {
		return pluginPreview{}, m.wrongKind(name, pluginSourceGitHub)
	}
	g, err := parsePluginSource(r.Source, r.Ref)
	if err != nil {
		return pluginPreview{}, fmt.Errorf("the recorded source is unusable: %v", err)
	}
	st, err := m.stage(ctx, g)
	if err != nil {
		return pluginPreview{}, err
	}
	if st.man.Name != name {
		_ = os.RemoveAll(st.root)
		return pluginPreview{}, fmt.Errorf("%s's plugin is now named %q, not %q; uninstall and install it again", g.String(), st.man.Name, name)
	}
	st.kind, st.target = "update", name
	m.putStage(st)
	base := loadPluginGrants()[name].Approved
	if base == "" {
		if e := m.entry(name); e != nil {
			base = e.FP
		}
	}
	changes := st.fp != base
	p := st.preview()
	p.CurrentCommit = r.Commit
	p.ChangesPermissions = &changes
	return p, nil
}

// updateConfirm swaps the staged checkout in: stop the server, move the old
// directory aside, move the new one in, delete the old — and on any failure put
// the old one back. The approval is untouched: an unchanged fingerprint is
// still approved, a changed one reads needs_approval, by the ordinary rule.
func (m *pluginManager) updateConfirm(name, token, fp string) error {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	st, err := m.takeStage(token, "update")
	if err != nil {
		return err
	}
	defer os.RemoveAll(st.root)
	if st.target != name {
		return errNoStage
	}
	if err := st.verify(fp); err != nil {
		return err
	}
	r, ok := loadPluginSources()[name]
	if !ok || r.Kind != pluginSourceGitHub {
		return m.wrongKind(name, pluginSourceGitHub)
	}
	final := filepath.Join(m.dir, name)
	cur, err := os.Lstat(final)
	if err != nil || cur.Mode()&fs.ModeSymlink != 0 || !cur.IsDir() {
		return fmt.Errorf("refusing to update %s: not a plain directory", final)
	}
	m.hold(name)
	defer m.unhold(name)
	aside := filepath.Join(st.root, "old")
	if err := pluginRename(final, aside); err != nil {
		return fmt.Errorf("move the old version aside: %v", err)
	}
	if err := pluginRename(st.dir, final); err != nil {
		if rerr := pluginRename(aside, final); rerr != nil {
			log.Printf("plugins:  %s: update failed AND the rollback failed: %v (the old version is at %s)", name, rerr, aside)
			return fmt.Errorf("move the new version in: %v; restoring the old one also failed: %v", err, rerr)
		}
		return fmt.Errorf("move the new version in: %v (the old version was restored)", err)
	}
	r.Commit = st.commit
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := setPluginSource(name, &r); err != nil {
		return err
	}
	log.Printf("plugins:  %s updated to %s@%s", name, st.src.String(), shortCommit(st.commit))
	m.unhold(name)
	m.rescan()
	return nil
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

// lineRing keeps the last max lines written to it.
type lineRing struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial []byte
}

func (r *lineRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partial = append(r.partial, p...)
	for {
		i := strings.IndexByte(string(r.partial), '\n')
		if i < 0 {
			break
		}
		r.push(strings.TrimRight(string(r.partial[:i]), "\r"))
		r.partial = r.partial[i+1:]
	}
	if len(r.partial) > 8192 { // a line that never ends
		r.push(string(r.partial))
		r.partial = r.partial[:0]
	}
	return len(p), nil
}

func (r *lineRing) push(line string) {
	r.lines = append(r.lines, clipLine(line, 2000))
	if over := len(r.lines) - r.max; over > 0 {
		r.lines = append([]string(nil), r.lines[over:]...)
	}
}

// last returns up to n of the most recent lines, a trailing partial included.
func (r *lineRing) last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := r.lines
	if len(r.partial) > 0 {
		all = append(slicesClone(all), string(r.partial))
	}
	if n < len(all) {
		all = all[len(all)-n:]
	}
	return slicesClone(all)
}

func slicesClone(s []string) []string { return append([]string{}, s...) }

type pluginLogPayload struct {
	Name      string   `json:"name"`
	Sandboxed bool     `json:"sandboxed"`
	Lines     []string `json:"lines"`
	Note      string   `json:"note,omitempty"`
}

func (m *pluginManager) pluginLog(name string, n int) (pluginLogPayload, error) {
	e := m.entry(name)
	if e == nil {
		m.rescan()
		if e = m.entry(name); e == nil {
			return pluginLogPayload{}, errPluginNotFound
		}
	}
	trusted := loadPluginGrants()[name].Trusted
	out := pluginLogPayload{Name: name, Sandboxed: !trusted, Lines: []string{}}
	// Only an MCP server produces output. Without one (tabs, themes, fonts)
	// the msb/trust branches below would explain an absence that has nothing
	// to do with either.
	if e.Man == nil || e.Man.MCP == nil {
		out.Sandboxed = false
		out.Note = "this plugin has no MCP server, so there is nothing to log"
		return out, nil
	}
	if trusted {
		m.mu.Lock()
		r := m.logs[name]
		m.mu.Unlock()
		if r != nil {
			out.Lines = r.last(n)
		}
		if len(out.Lines) == 0 {
			out.Note = "no output from its MCP server since lasso started"
		}
		return out, nil
	}
	msb, reason, ok := resolveMSB()
	if !ok {
		out.Note = reason
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, msb, "logs", "--tail", strconv.Itoa(n), pluginSandboxName(name))
	cmd.Stdin = nil
	b, err := cmd.CombinedOutput()
	text := strings.TrimRight(string(b), "\n")
	if err != nil {
		out.Note = "no sandbox logs: " + clipLine(strings.TrimSpace(text), 400)
		if strings.TrimSpace(text) == "" {
			out.Note = "no sandbox logs: " + err.Error()
		}
		return out, nil
	}
	if text != "" {
		out.Lines = strings.Split(text, "\n")
	}
	if len(out.Lines) == 0 {
		out.Note = "the sandbox has logged nothing"
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// serveInstallAPI answers the install/link/update/log routes, reporting
// whether it handled the request. Everything it returns on success is the
// listing, except the previews, the cancel and the log.
func (m *pluginManager) serveInstallAPI(w http.ResponseWriter, r *http.Request, rest string) bool {
	post := func() bool {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return false
		}
		return true
	}
	decode := func(v any) bool {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if len(strings.TrimSpace(string(body))) == 0 {
			return true
		}
		if err := json.Unmarshal(body, v); err != nil {
			http.Error(w, "body must be JSON: "+err.Error(), http.StatusBadRequest)
			return false
		}
		return true
	}
	fail := func(err error) {
		switch {
		case errors.Is(err, errPluginChanged):
			http.Error(w, "the plugin changed since the preview; preview again", http.StatusConflict)
		case errors.Is(err, errNoStage):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, errPluginNotFound):
			http.Error(w, "no such plugin", http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}
	listing := func(err error) {
		if err != nil {
			fail(err)
			return
		}
		writeJSON(w, m.listing())
	}
	cloneCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(r.Context(), pluginCloneTimeout+10*time.Second)
	}

	switch rest {
	case "install/preview":
		if !post() {
			return true
		}
		var in struct {
			Source string `json:"source"`
			Ref    string `json:"ref"`
		}
		if !decode(&in) {
			return true
		}
		ctx, cancel := cloneCtx()
		defer cancel()
		p, err := m.installPreview(ctx, in.Source, in.Ref)
		if err != nil {
			fail(err)
			return true
		}
		writeJSON(w, p)
		return true
	case "install/confirm":
		if !post() {
			return true
		}
		var in struct {
			Token       string `json:"token"`
			Fingerprint string `json:"fingerprint"`
			Enable      bool   `json:"enable"`
		}
		if !decode(&in) {
			return true
		}
		listing(m.installConfirm(in.Token, in.Fingerprint, in.Enable))
		return true
	case "install/cancel":
		if !post() {
			return true
		}
		var in struct {
			Token string `json:"token"`
		}
		if !decode(&in) {
			return true
		}
		m.cancelStage(in.Token)
		writeJSON(w, map[string]bool{"ok": true})
		return true
	case "link":
		if !post() {
			return true
		}
		var in struct {
			Path   string `json:"path"`
			Enable bool   `json:"enable"`
		}
		if !decode(&in) {
			return true
		}
		_, err := m.link(in.Path, in.Enable)
		listing(err)
		return true
	}

	name, action, ok := strings.Cut(rest, "/")
	if !ok || !pluginNameRE.MatchString(name) {
		return false
	}
	switch action {
	case "unlink":
		if post() {
			listing(m.unlink(name))
		}
	case "uninstall":
		if !post() {
			return true
		}
		var in struct {
			PurgeData bool `json:"purge_data"`
		}
		if decode(&in) {
			listing(m.uninstall(name, in.PurgeData))
		}
	case "update/preview":
		if !post() {
			return true
		}
		ctx, cancel := cloneCtx()
		defer cancel()
		p, err := m.updatePreview(ctx, name)
		if err != nil {
			fail(err)
			return true
		}
		writeJSON(w, p)
	case "update/confirm":
		if !post() {
			return true
		}
		var in struct {
			Token       string `json:"token"`
			Fingerprint string `json:"fingerprint"`
		}
		if decode(&in) {
			listing(m.updateConfirm(name, in.Token, in.Fingerprint))
		}
	case "log":
		if r.Method != http.MethodGet {
			http.Error(w, "GET", http.StatusMethodNotAllowed)
			return true
		}
		n := 200
		if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil {
			n = v
		}
		n = max(1, min(n, 2000))
		out, err := m.pluginLog(name, n)
		if err != nil {
			fail(err)
			return true
		}
		writeJSON(w, out)
	default:
		return false
	}
	return true
}
