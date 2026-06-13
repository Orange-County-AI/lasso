package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHub-release plumbing: how lasso learns about, and updates to, newer
// versions. Two install shapes are supported (see cliUpdate):
//   - a mise-managed install (the standard install.sh path): defer to
//     `mise upgrade`, then restart the pitchfork daemon.
//   - a plain release binary (curl-installed elsewhere): compare to the latest
//     GitHub release and self-replace the binary, then restart the daemon.

const githubRepo = "knowsuchagency/lasso"

// release is the slice of the GitHub releases API we use.
type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// assetURL returns the download URL for a named asset, or "".
func (r *release) assetURL(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// assetName is the release asset for the running platform, e.g.
// "lasso-linux-amd64" / "lasso-darwin-arm64". Matches the names CI publishes.
func assetName() string {
	return fmt.Sprintf("lasso-%s-%s", runtime.GOOS, runtime.GOARCH)
}

// fetchLatestRelease queries GitHub for the latest published release. Uncached —
// used by `lasso update`, which should always see fresh state.
func fetchLatestRelease() (*release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "lasso/"+lassoSemver) // GitHub requires a UA
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases: %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("latest release has no tag")
	}
	return &rel, nil
}

// latestReleaseCache holds the latest tag with a TTL, so serveVersion (which the
// Settings tab polls) doesn't hit GitHub — and risk rate limits — per request.
var latestReleaseCache struct {
	mu         sync.Mutex
	at         time.Time
	tag        string
	err        error
	refreshing bool
}

const latestReleaseTTL = time.Hour

// latestReleaseTag returns the latest release tag, refreshing the hour-long cache
// synchronously when stale. Used by CLI paths (doctor) that can afford to block;
// serveVersion uses cachedLatestTag instead so a cold cache never stalls a
// request.
func latestReleaseTag() (string, error) {
	latestReleaseCache.mu.Lock()
	if !latestReleaseCache.at.IsZero() && time.Since(latestReleaseCache.at) < latestReleaseTTL {
		tag, err := latestReleaseCache.tag, latestReleaseCache.err
		latestReleaseCache.mu.Unlock()
		return tag, err
	}
	latestReleaseCache.mu.Unlock()
	return refreshLatestTag()
}

// cachedLatestTag returns the cached latest tag without ever blocking: a fresh
// value comes back with ok=true; a cold/stale cache returns ok=false and kicks
// off a background refresh, so the next poll has it. For serveVersion, whose
// caller (the Settings menu) shouldn't wait on GitHub.
func cachedLatestTag() (tag string, ok bool) {
	latestReleaseCache.mu.Lock()
	defer latestReleaseCache.mu.Unlock()
	fresh := !latestReleaseCache.at.IsZero() && time.Since(latestReleaseCache.at) < latestReleaseTTL
	if !fresh && !latestReleaseCache.refreshing {
		latestReleaseCache.refreshing = true
		go func() { _, _ = refreshLatestTag() }()
	}
	return latestReleaseCache.tag, fresh && latestReleaseCache.tag != ""
}

// refreshLatestTag fetches the latest release and updates the cache.
func refreshLatestTag() (string, error) {
	rel, err := fetchLatestRelease()
	latestReleaseCache.mu.Lock()
	defer latestReleaseCache.mu.Unlock()
	latestReleaseCache.at = time.Now()
	latestReleaseCache.refreshing = false
	if err != nil {
		latestReleaseCache.err = err
		return latestReleaseCache.tag, err // keep any previously-good tag
	}
	latestReleaseCache.tag, latestReleaseCache.err = rel.TagName, nil
	return rel.TagName, nil
}

// semverNewer reports whether b is a newer version than a. Both may carry a
// leading "v"; a pre-release/build suffix on the patch (e.g. "1.2.3-rc1") is
// ignored for the comparison. Unparseable input compares as not-newer.
func semverNewer(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pb[i] != pa[i] {
			return pb[i] > pa[i]
		}
	}
	return false
}

// parseSemver parses "vX.Y.Z" (optional leading v, optional -suffix on Z) into
// [3]int.
func parseSemver(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		if i == 2 { // strip any -prerelease / +build suffix from the patch
			if cut := strings.IndexAny(p, "-+"); cut >= 0 {
				p = p[:cut]
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// ---------------------------------------------------------------------------
// lasso update
// ---------------------------------------------------------------------------

// cliUpdate updates lasso in place. A mise-managed install defers to
// `mise upgrade`; otherwise it downloads the latest release for this platform and
// atomically replaces itself. Either way it then restarts the pitchfork daemon if
// one is supervising lasso.
func cliUpdate() {
	if miseManaged() {
		updateViaMise()
		return
	}
	updateViaRelease()
}

// updateViaMise upgrades the mise-installed lasso tool, then restarts the daemon.
func updateViaMise() {
	fmt.Println("updating lasso via mise …")
	if !miseUpgrade(miseLassoTool) && !miseUpgrade("lasso") {
		fatal("mise upgrade failed for %q — try `mise upgrade` manually", miseLassoTool)
	}
	restartIfSupervised(lassoDaemon())
}

// miseUpgrade runs `mise upgrade <tool>` and reports whether it succeeded.
func miseUpgrade(tool string) bool {
	return exec.Command("mise", "upgrade", tool).Run() == nil
}

// restartIfSupervised restarts the pitchfork daemon when one is registered, so an
// update lands on the running server; otherwise it just prints how to restart.
func restartIfSupervised(daemon string) {
	if !pitchforkRegistered(daemon) {
		fmt.Println("run `lasso restart` to bring the server onto the new version")
		return
	}
	fmt.Println("restarting the lasso daemon …")
	if err := pitchforkRestart(daemon); err != nil {
		fatal("pitchfork restart %s: %v", daemon, err)
	}
}

// updateViaRelease downloads the latest release binary for this platform, checks
// it against the release's checksums.txt, and atomically swaps it in.
func updateViaRelease() {
	current := lassoSemver
	rel, err := fetchLatestRelease()
	if err != nil {
		fatal("check latest release: %v", err)
	}
	if !semverNewer(current, rel.TagName) {
		fmt.Printf("lasso %s is already up to date (latest release %s)\n", current, rel.TagName)
		return
	}
	fmt.Printf("updating lasso %s → %s …\n", current, rel.TagName)

	name := assetName()
	binURL := rel.assetURL(name)
	if binURL == "" {
		fatal("release %s has no asset %q", rel.TagName, name)
	}
	bin, err := httpGet(binURL)
	if err != nil {
		fatal("download %s: %v", name, err)
	}
	// Verify against checksums.txt when the release publishes one.
	if sumsURL := rel.assetURL("checksums.txt"); sumsURL != "" {
		sums, err := httpGet(sumsURL)
		if err != nil {
			fatal("download checksums.txt: %v", err)
		}
		want := checksumFor(string(sums), name)
		if want == "" {
			fatal("checksums.txt has no entry for %s", name)
		}
		got := sha256Hex(bin)
		if got != want {
			fatal("checksum mismatch for %s (want %s, got %s)", name, want, got)
		}
	}

	if err := replaceSelf(bin); err != nil {
		fatal("replace binary: %v", err)
	}
	fmt.Printf("lasso updated to %s\n", rel.TagName)
	restartIfSupervised(lassoDaemon())
}

// replaceSelf atomically swaps the running executable with new bytes: write to a
// temp file in the same directory (so the rename stays on one filesystem), make
// it executable, then rename over the current path. On Unix the running process
// keeps the old inode, so replacing a live binary is safe.
func replaceSelf(data []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".lasso-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, exe)
}

// httpGet fetches a URL and returns its body, failing on a non-2xx status.
func httpGet(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "lasso/"+lassoSemver)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checksumFor finds the sha256 for name in sha256sum-format text
// ("<hex>  <filename>" per line).
func checksumFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return f[0]
		}
	}
	return ""
}
