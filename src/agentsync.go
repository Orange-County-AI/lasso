// Agent theme sync: when lasso's theme changes (via /api/theme-set, a host
// switch, or an out-of-band edit to herdr's config.toml picked up by the hub
// poll), mirror it into the agent CLIs' own theme files — a generated
// opencode theme (themes/herdr.json, pinned via tui.json), Claude Code's
// ~/.claude/themes/herdr.json, ghostty's themes/herdr, omp's
// ~/.omp/agent/themes/herdr.json (pinned in both of its mode slots, and the one
// mirror a running agent picks up live), and lasso's settings.json
// (.theme.resolved, read by claude-contextline) — so agents render in step with
// herdr. Writes go through the Backend interface, so every reachable host gets
// the same treatment over SFTP (see syncRemoteTheme).
//
// Reach and convergence are the two things this file gets right on purpose:
//
//   - Reach: a theme write needs ssh, not a herdr this lasso can drive. A host
//     running a mismatched protocol (or no herdr at all) is written over a
//     files-only connection (newRemoteFileBackend); only the reload nudge is
//     skipped. Gating file writes on protocol compatibility is what left two
//     Macs a month behind the fleet's palette.
//   - Convergence: every completed host probe (convergeThemeOnProbe, called from
//     putHost) compares the theme lasso last wrote to a host against the live
//     one, so a machine asleep or unreachable during a theme change catches up
//     on its next probe instead of waiting for the next change.
//
// This subsumes the old per-machine herdr-theme-sync watcher daemons: lasso is
// the single writer of herdr's [theme].name in practice, and its hub poll
// catches edits it didn't make, so no file-watching service is needed.
//
// Gated by the sync_agent_themes setting (default on), which the Settings tab
// exposes as "Sync agent themes", and by the per-host theme_sync_off deny-list
// below, which switches off every theme write lasso makes to one host.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// syncAgentThemesKey is the settings-table key for the toggle. Unset means on.
const syncAgentThemesKey = "sync_agent_themes"

// syncAgentThemesEnabled reports whether agent theme sync is on (the default).
func syncAgentThemesEnabled() bool {
	v, err := getSetting(syncAgentThemesKey)
	if err != nil || v == "" {
		return true
	}
	on, err := strconv.ParseBool(v)
	return err != nil || on
}

// themeSyncOffKey holds the per-host opt-out: a JSON array of host names
// ("local" or ssh aliases) lasso must write NO theme to at all — neither
// herdr's [theme].name (syncRemoteTheme) nor any agent theme file
// (syncAgentThemesVia). A deny-list, like uiState.UsageHidden, so a host added
// to the ssh config later syncs by default rather than silently staying behind.
//
// It lives in the db of the machine lasso runs on, not in each host's own
// lasso.db where hostconfig.go's creator settings live: it is this lasso's
// policy about what it writes, and the gate has to answer for a host that is
// asleep — a sqlite3 round trip over ssh on every theme change could not.
const themeSyncOffKey = "theme_sync_off"

// themeSyncOffHosts is the deny-list, sorted and never nil (the API serves it
// as-is). A nil db (tests, shutdown) reads as "nothing disabled".
func themeSyncOffHosts() []string {
	if db == nil {
		return []string{}
	}
	v, err := getSetting(themeSyncOffKey)
	if err != nil || v == "" {
		return []string{}
	}
	var hosts []string
	if err := json.Unmarshal([]byte(v), &hosts); err != nil {
		return []string{}
	}
	sort.Strings(hosts)
	return hosts
}

// themeSyncEnabledFor reports whether lasso may write themes to host. Hosts are
// keyed as the Backend names them ("local" or an ssh alias); the empty host
// means local. Unlisted hosts sync — the default.
func themeSyncEnabledFor(host string) bool {
	if isLocalHost(host) {
		host = "local"
	}
	for _, h := range themeSyncOffHosts() {
		if h == host {
			return false
		}
	}
	return true
}

// setThemeSyncFor adds host to the deny-list (on=false) or drops it (on=true).
func setThemeSyncFor(host string, on bool) error {
	if isLocalHost(host) {
		host = "local"
	}
	cur := themeSyncOffHosts()
	out := make([]string, 0, len(cur)+1)
	for _, h := range cur {
		if h != host {
			out = append(out, h)
		}
	}
	if !on {
		out = append(out, host)
	}
	sort.Strings(out)
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return setSetting(themeSyncOffKey, string(b))
}

// themeFanoutConcurrency keeps one theme click from opening an ssh master to
// every configured alias at once. Theme writes wait on SFTP/ssh latency, so six
// concurrent hosts keeps the common fleet responsive without a connection burst.
const themeFanoutConcurrency = 6

// themeFanoutMu serializes complete fan-outs. The theme-set endpoint and hub
// poll can notice the same change together; their writers skip unchanged bytes,
// making the second pass nearly free once the first has finished.
var themeFanoutMu sync.Mutex

// themeSem bounds concurrent theme writes server-wide, so the fan-out and the
// per-host convergence pushes (convergeThemeOnProbe) share one budget instead of
// each keeping its own — a sweep that finds ten stale hosts must not open ten
// ssh masters beside a fan-out already running.
var themeSem = make(chan struct{}, themeFanoutConcurrency)

// themeFanoutHosts returns the settled, reachable remote hosts lasso may write a
// theme to. Reachable is the whole bar on purpose: every theme write is file I/O
// over SFTP (ghostty's theme, Claude's, opencode's, omp's, herdr's config.toml),
// which needs ssh and nothing else.
//
// It used to also demand a RUNNING, PROTOCOL-COMPATIBLE herdr, and that is how a
// fleet silently drifted apart: a box one herdr release behind (protocol 19 vs
// 20) was dropped from every fan-out and kept whatever palette it had when it
// last matched — observed on a Mac stuck three weeks and a theme behind, its
// ghostty still dark against a light herdr. Compatibility decides whether lasso
// can DRIVE a host, not whether it may write a file on one.
//
// Checking the deny-list here, before themeBackend dials, preserves an opt-out
// even when a host has no existing pooled connection.
func themeFanoutHosts(rows []HostInfo) []string {
	hosts := make([]string, 0, len(rows))
	for _, hi := range rows {
		if hi.Alias == "" || isLocalHost(hi.Alias) || hi.State != "" ||
			!hi.Reachable || !themeSyncEnabledFor(hi.Alias) {
			continue
		}
		hosts = append(hosts, hi.Alias)
	}
	return hosts
}

// themeTarget is what writing a theme to a host needs: file I/O, the path herdr
// on that host reads its config from, and — when HerdrSock is non-empty — a live
// herdr to ask for a reload.
type themeTarget interface {
	Backend
	herdrConfigPath() string
}

// themeBackend returns a connection to host for theme writes plus the release to
// call when they're done. A host lasso can drive answers from the pool: its
// master is already up, stays up, and carries a herdr that can be asked to
// reload. A host lasso cannot drive — herdr stopped, or speaking a protocol this
// build refuses — gets a throwaway files-only connection instead of being
// skipped, and the caller closes it.
func themeBackend(host string) (themeTarget, func(), error) {
	if hi, ok := findHost(host); ok && hi.Reachable && hi.Running && hi.Compatible {
		b, err := namedHostBackend(host)
		if err == nil {
			if t, ok := b.(themeTarget); ok {
				return t, func() {}, nil
			}
		} else {
			log.Printf("theme:    %s: no pooled connection (%v) — writing theme files over a fresh ssh connection", host, err)
		}
	}
	rb, err := newRemoteFileBackend(srvCtx, host)
	if err != nil {
		return nil, nil, err
	}
	return rb, func() { _ = rb.Close() }, nil
}

// syncThemeToHost pushes rt to one host and records the result (themeSynced), so
// a host that failed or was skipped is retried by the next convergence pass. It
// is best-effort: a host lasso cannot reach logs and is left for that retry.
func syncThemeToHost(host string, rt resolvedTheme) {
	if !themeSyncEnabledFor(host) {
		return
	}
	if isLocalHost(host) {
		if err := syncAgentThemesVia(localFsBackend(), rt); err != nil {
			forgetThemeSynced(host)
			return
		}
		markThemeSynced(host, rt.Resolved)
		return
	}
	t, release, err := themeBackend(host)
	if err != nil {
		forgetThemeSynced(host)
		log.Printf("theme:    %s not reachable to sync theme: %v", host, err)
		return
	}
	defer release()
	if err := syncRemoteTheme(t, rt.Resolved); err != nil {
		forgetThemeSynced(host)
		return
	}
	markThemeSynced(host, rt.Resolved)
}

// syncThemeEverywhere mirrors rt locally and across each settled reachable host.
// Callers run it off their request/poll paths because remote SFTP writes can
// wait on ssh; themeSem caps the fleet-wide connection burst.
func syncThemeEverywhere(rt resolvedTheme) {
	themeFanoutMu.Lock()
	defer themeFanoutMu.Unlock()

	rows, _ := hostSnapshot()
	hosts := append([]string{"local"}, themeFanoutHosts(rows)...)
	var wg sync.WaitGroup
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			themeSem <- struct{}{}
			defer func() { <-themeSem }()
			syncThemeToHost(host, rt)
		}(host)
	}
	wg.Wait()
}

// convergeThemeSyncFor pushes the current theme onto a host whose sync was just
// switched back on, so the checkbox takes effect now instead of at the next
// theme or host switch. Best-effort and meant to run off the request path: a
// host lasso can't reach right now just logs and converges later.
func convergeThemeSyncFor(host string) {
	forgetThemeSynced(host) // it was denied, so whatever we last wrote is moot
	syncThemeToHost(host, liveTheme())
}

// liveTheme is the theme lasso is painting right now: the hub's, which follows
// herdr's config.toml live (or the one -theme pinned), and a fresh read of that
// config before the hub exists.
func liveTheme() resolvedTheme {
	if srvHub != nil {
		return srvHub.themeSnapshot()
	}
	return loadHerdrTheme(*themeName)
}

// themeSynced records the theme name lasso last WROTE to each host, plus which
// hosts have a convergence push in flight. It is the whole mechanism behind
// catching a host up: a machine asleep when the user picked a palette used to
// keep the old one until the next theme change or host switch, because nothing
// ever revisited it — the way a laptop ended up three weeks behind the fleet.
// Now every completed host probe (convergeThemeOnProbe) compares this record
// against the live theme, so a machine converges within a refresh cycle of
// coming back and a host already in step costs nothing.
//
// Deliberately in memory, not the settings table: it records what THIS process
// wrote and can vouch for, so a restarted lasso reconciles the fleet once
// rather than trusting a note on disk about files it never saw.
var themeSynced struct {
	mu       sync.Mutex
	by       map[string]string // host -> theme name last written successfully
	inFlight map[string]bool   // hosts with a convergence push running
}

func markThemeSynced(host, name string) {
	themeSynced.mu.Lock()
	defer themeSynced.mu.Unlock()
	if themeSynced.by == nil {
		themeSynced.by = map[string]string{}
	}
	themeSynced.by[host] = name
}

// forgetThemeSynced drops a host's record so the next probe retries it.
func forgetThemeSynced(host string) {
	themeSynced.mu.Lock()
	defer themeSynced.mu.Unlock()
	delete(themeSynced.by, host)
}

// claimThemeConverge reports whether this caller should push name to host: true
// only when the last write there wasn't already name and no push is in flight.
// The in-flight half matters because probes arrive in bursts (a sweep, then the
// footer's refresh) and a push takes seconds — without it one stale host would
// be written by several goroutines at once.
func claimThemeConverge(host, name string) bool {
	themeSynced.mu.Lock()
	defer themeSynced.mu.Unlock()
	if themeSynced.inFlight[host] || themeSynced.by[host] == name {
		return false
	}
	if themeSynced.inFlight == nil {
		themeSynced.inFlight = map[string]bool{}
	}
	themeSynced.inFlight[host] = true
	return true
}

func releaseThemeConverge(host string) {
	themeSynced.mu.Lock()
	defer themeSynced.mu.Unlock()
	delete(themeSynced.inFlight, host)
}

// syncThemeToHostFn is the seam convergence pushes go through, so the probe path
// can be driven in tests without ssh (mirroring hosts.go's probeHostFn).
var syncThemeToHostFn = syncThemeToHost

// convergeThemeOnProbe pushes the live theme onto a host whose probe just landed
// and whose theme lasso has not already written there. Called from putHost, so
// the background refresher's sweep IS the reconcile loop: a host that was
// asleep, unreachable, or failed mid-write during a theme change catches up on
// its next probe rather than waiting for the next theme change.
//
// Runs in a goroutine and holds a themeSem slot for the duration, so a sweep
// that finds the whole fleet stale (a fresh lasso, whose record is empty by
// design) converges it in waves instead of one ssh burst.
func convergeThemeOnProbe(hi HostInfo) {
	if srvHub == nil || db == nil { // not a running server: boot, CLI, tests
		return
	}
	if hi.Alias == "" || isLocalHost(hi.Alias) || hi.State != "" || !hi.Reachable {
		return
	}
	if !syncAgentThemesEnabled() || !themeSyncEnabledFor(hi.Alias) {
		return
	}
	rt := liveTheme()
	if rt.Resolved == "" || !claimThemeConverge(hi.Alias, rt.Resolved) {
		return
	}
	go func() {
		defer releaseThemeConverge(hi.Alias)
		themeSem <- struct{}{}
		defer func() { <-themeSem }()
		syncThemeToHostFn(hi.Alias, rt)
	}()
}

// syncAgentThemesVia mirrors rt into the agent theme files on backend b. Every
// CLI is attempted whatever the others did — one missing config dir must not
// cost the rest their palette — and the joined failure is returned so the caller
// knows whether this host is actually in step (syncThemeToHost records it, and a
// host that isn't is retried on its next probe). Callers that only want the
// side effect can ignore it; nothing here is fatal to a theme switch.
func syncAgentThemesVia(b Backend, rt resolvedTheme) error {
	if b == nil || !syncAgentThemesEnabled() || !themeSyncEnabledFor(b.Name()) {
		return nil
	}
	home, err := b.HomeDir()
	if err != nil || home == "" {
		log.Printf("theme:    agent sync on %s: no home dir: %v", b.Name(), err)
		if err == nil {
			err = errors.New("empty home dir")
		}
		return err
	}
	light := luminance(rt.ui.PanelBg) > 0.5
	var errs []error
	step := func(cli string, err error) {
		if err != nil {
			log.Printf("theme:    %s sync on %s: %v", cli, b.Name(), err)
			errs = append(errs, fmt.Errorf("%s: %w", cli, err))
		}
	}
	step("opencode", syncOpencodeTheme(b, home, rt))
	step("claude", syncClaudeTheme(b, home, rt, light))
	step("omp", syncOmpTheme(b, home, rt))
	step("ghostty", syncGhosttyTheme(b, home, rt))
	step("lasso appearance", syncLassoResolved(b, home, light))
	return errors.Join(errs...)
}

// resolveThemeByName resolves a canonical theme key (no custom overrides — used
// for the remote mirror, where lasso only writes [theme].name and the remote
// herdr owns any [theme.custom]).
func resolveThemeByName(name string) resolvedTheme {
	key := normalizeThemeName(name)
	def, ok := themes[key]
	if !ok {
		key, def = "catppuccin", themes["catppuccin"]
	}
	return resolvedTheme{Name: name, Resolved: key, ui: def.ui, ansi: def.ansi}
}

// ---------------------------------------------------------------------------
// opencode — generated themes/herdr.json + tui.json "theme" pin + kv mode hint
// ---------------------------------------------------------------------------
//
// opencode 1.x resolves its light/dark mode as: kv "theme_mode_lock" ??
// the terminal-detected background ?? its default. Neither works for us:
// detection fails through the lasso/ttyd/xterm chain, and the lock lives in
// opencode's state kv.json, which any running instance rewrites wholesale from
// its in-memory state on every state change — so a pin written under it gets
// clobbered back (observed: a light-locked instance overwrote lasso's dark pin
// seconds later, and every fresh launch then came up light).
//
// So the mode lock can't be load-bearing. Instead lasso generates a theme from
// herdr's resolved palette and pins it by name — both in files opencode reads
// but never writes: ~/.config/opencode/themes/herdr.json (custom themes are
// discovered from <config>/themes/*.json) and the "theme" key in tui.json.
// It also writes legacy Catppuccin aliases: custom themes override built-ins,
// so an older lasso or dev build that later rewrites tui.json to "catppuccin"
// still resolves the generated, mode-invariant Herdr palette.
// Every token is emitted as a bare hex, which opencode's resolver treats as
// mode-invariant (only {dark,light} pair objects consult the mode), so the
// rendered colors are correct no matter what mode the lock/detection lands on.
// A theme flip rewrites herdr.json with the new palette; fresh launches pick
// it up. The kv mode lock is still written as a fallback hint (it keeps
// opencode's built-in adaptive themes in step if the pin is ever removed
// manually), but nothing correct depends on it anymore.

// opencodeThemeName is the generated theme lasso pins for opencode.
const opencodeThemeName = "herdr"

// opencodeThemeNames includes the generated theme and legacy names older lasso
// builds can write to tui.json or opencode's state. The custom files shadow
// opencode's built-ins, making those stale writers harmless.
var opencodeThemeNames = []string{
	opencodeThemeName,
	"catppuccin",
	"catppuccin-frappe",
	"catppuccin-macchiato",
}

func syncOpencodeTheme(b Backend, home string, rt resolvedTheme) error {
	if err := syncOpencodeThemeFile(b, home, rt); err != nil {
		return err
	}
	if err := syncOpencodeTui(b, home); err != nil {
		return err
	}
	return syncOpencodeMode(b, home, luminance(rt.ui.PanelBg) > 0.5)
}

// opencodeThemeBody renders rt as an opencode custom theme: herdr's UI tokens
// mapped onto the same semantic roles lasso uses for its own chrome, all as
// bare hex strings so the mode can't change the result.
func opencodeThemeBody(rt resolvedTheme) []byte {
	u, a := rt.ui, rt.ansi
	m := map[string]string{
		"primary":               u.Accent,
		"secondary":             u.Mauve,
		"accent":                a.Magenta, // the palette's pink slot (mocha pink, dracula pink, etc.)
		"error":                 u.Red,
		"warning":               u.Yellow,
		"success":               u.Green,
		"info":                  u.Teal,
		"text":                  u.Text,
		"textMuted":             u.Subtext0,
		"background":            u.PanelBg,
		"backgroundPanel":       u.Surface0,
		"backgroundElement":     u.Surface1,
		"border":                u.Surface1,
		"borderActive":          u.Accent,
		"borderSubtle":          u.SurfaceDim,
		"diffAdded":             u.Green,
		"diffRemoved":           u.Red,
		"diffContext":           u.Subtext0,
		"diffHunkHeader":        u.Peach,
		"diffHighlightAdded":    u.Green,
		"diffHighlightRemoved":  u.Red,
		"diffAddedBg":           blendHex(u.Green, u.PanelBg, 0.9),
		"diffRemovedBg":         blendHex(u.Red, u.PanelBg, 0.9),
		"diffContextBg":         u.Surface0,
		"diffLineNumber":        u.Subtext0,
		"diffAddedLineNumberBg": blendHex(u.Green, u.PanelBg, 0.95),
		// Removed line numbers sit on the same tinted rows.
		"diffRemovedLineNumberBg": blendHex(u.Red, u.PanelBg, 0.95),
		"markdownText":            u.Text,
		"markdownHeading":         u.Mauve,
		"markdownLink":            u.Blue,
		"markdownLinkText":        u.Teal,
		"markdownCode":            u.Green,
		"markdownBlockQuote":      u.Yellow,
		"markdownEmph":            u.Yellow,
		"markdownStrong":          u.Peach,
		"markdownHorizontalRule":  u.Subtext0,
		"markdownListItem":        u.Blue,
		"markdownListEnumeration": u.Teal,
		"markdownImage":           u.Blue,
		"markdownImageText":       u.Teal,
		"markdownCodeBlock":       u.Text,
		"syntaxComment":           u.Subtext0,
		"syntaxKeyword":           u.Mauve,
		"syntaxFunction":          u.Blue,
		"syntaxVariable":          u.Red,
		"syntaxString":            u.Green,
		"syntaxNumber":            u.Peach,
		"syntaxType":              u.Yellow,
		"syntaxOperator":          u.Teal,
		"syntaxPunctuation":       u.Text,
	}
	// Defensive: drop anything that isn't "#rrggbb" — a bare non-hex string
	// would be resolved as a color *reference* and throw at render time.
	for tok, v := range m {
		if _, _, _, ok := hexRGB(v); !ok {
			delete(m, tok)
		}
	}
	root := map[string]any{
		"$schema": "https://opencode.ai/theme.json",
		"theme":   m,
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

func syncOpencodeThemeFile(b Backend, home string, rt resolvedTheme) error {
	dir := filepath.Join(home, ".config", "opencode", "themes")
	body := opencodeThemeBody(rt)
	for _, name := range opencodeThemeNames {
		path := filepath.Join(dir, name+".json")
		if cur, err := b.ReadFile(path); err == nil && string(cur) == string(body) {
			continue
		}
		if err := b.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		log.Printf("theme:    opencode theme file -> %s (%s) on %s", name, rt.Resolved, b.Name())
		if err := b.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func syncOpencodeTui(b Backend, home string) error {
	path := filepath.Join(home, ".config", "opencode", "tui.json")

	root := map[string]json.RawMessage{}
	data, err := b.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(data, &root) != nil {
			// Malformed (or jsonc with comments) — don't clobber what we
			// can't parse.
			return nil
		}
	case errors.Is(err, fs.ErrNotExist):
		root["$schema"], _ = json.Marshal("https://opencode.ai/tui.json")
	default:
		return err
	}

	var cur string
	if raw, ok := root["theme"]; ok {
		_ = json.Unmarshal(raw, &cur)
	}
	if cur == opencodeThemeName {
		return nil
	}
	root["theme"], _ = json.Marshal(opencodeThemeName)
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	log.Printf("theme:    opencode theme -> %s on %s", opencodeThemeName, b.Name())
	return b.WriteFile(path, out, 0o644)
}

// syncOpencodeMode pins opencode's light/dark mode as a fallback hint. With
// the generated herdr theme pinned (see above), every rendered token is
// mode-invariant, so the lock only matters if the tui.json pin is removed
// manually and opencode falls back to one of its built-in adaptive themes.
// Note the lock is best-effort by nature: a running opencode keeps its
// in-memory kv state and may write it back over ours.
func syncOpencodeMode(b Backend, home string, light bool) error {
	path := filepath.Join(home, ".local", "state", "opencode", "kv.json")
	want := "dark"
	if light {
		want = "light"
	}

	root := map[string]json.RawMessage{}
	data, err := b.ReadFile(path)
	switch {
	case err == nil:
		if json.Unmarshal(data, &root) != nil {
			return nil // don't clobber what we can't parse
		}
	case errors.Is(err, fs.ErrNotExist):
		// Create it — a first-ever opencode launch then starts in the right
		// mode instead of guessing.
	default:
		return err
	}

	var lock, cur string
	if raw, ok := root["theme_mode_lock"]; ok {
		_ = json.Unmarshal(raw, &lock)
	}
	if raw, ok := root["theme_mode"]; ok {
		_ = json.Unmarshal(raw, &cur)
	}
	if lock == want && cur == want {
		return nil
	}
	root["theme_mode_lock"], _ = json.Marshal(want)
	root["theme_mode"], _ = json.Marshal(want)
	out, err := json.Marshal(root) // kv.json is compact single-line JSON
	if err != nil {
		return err
	}
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	log.Printf("theme:    opencode mode -> %s on %s", want, b.Name())
	return b.WriteFile(path, out, 0o644)
}

// ---------------------------------------------------------------------------
// omp (Oh My Pi) — generated themes/herdr.json + both config.yml theme slots
// ---------------------------------------------------------------------------
//
// omp holds two named themes — settings theme.dark and theme.light — and picks
// between them from the TERMINAL's background (OSC 11 luminance, then
// COLORFGBG, then a hardcoded "dark"), re-deciding on terminal appearance
// reports and SIGWINCH. That detection is what fails through the
// lasso/ttyd/xterm chain, the same reason opencode's needs overriding.
//
// Pinning both slots at omp's own built-in "dark"/"light" (what lasso did
// before) only half-worked. It did make the SLOT choice irrelevant, but the pin
// lives in config.yml, which omp reads once at startup — so a herdr theme flip
// never reached a RUNNING omp, and an agent launched before the flip kept the
// mode its terminal happened to look like until someone restarted it.
//
// So omp gets opencode's treatment instead: a generated theme carrying herdr's
// palette at ~/.omp/agent/themes/herdr.json, pinned in BOTH slots. Two things
// follow:
//
//   - the light/dark branch cannot matter — both slots name one theme, and its
//     colors are herdr's rather than a mode's;
//   - it repaints LIVE. omp watches <themes dir>/<current theme>.json and
//     debounce-reloads it on change, so rewriting herdr.json restyles every
//     running omp launched with the pin in place. That watcher is deliberately
//     skipped when the current theme is named "dark" or "light" — one more
//     reason the old pin could never repaint anything.
//
// omp's lookup resolves built-ins BEFORE custom files, so the generated file
// must not be called dark.json/light.json (it would be shadowed); "herdr" is
// both unshadowed and watched. Every token is a literal hex: a token that is
// neither a hex nor a declared var throws during resolution, and omp answers a
// failed theme load by falling back to its built-in "dark" — i.e. one bad token
// costs the whole palette.
//
// The config pin stays best-effort, as opencode's is: omp rewrites config.yml
// wholesale from its in-memory settings whenever one changes, so a running
// instance can write back over the slots. That cannot undo a live repaint (the
// theme file is what running instances read), and fresh launches take the pin.

// ompThemeName is the generated theme lasso writes and pins in both of omp's
// mode slots. Must not collide with an omp built-in — see above.
const ompThemeName = "herdr"

// ompThemeSchema is the $schema omp's own built-in themes carry. Informational
// only (omp validates with an in-code ArkType schema), but it makes the
// generated file behave in an editor.
const ompThemeSchema = "https://raw.githubusercontent.com/can1357/oh-my-pi/main/packages/coding-agent/theme-schema.json"

// syncOmpTheme writes the generated theme and points both mode slots at it.
// Skipped entirely on hosts where omp has never run (no ~/.omp/agent), so a
// theme switch doesn't litter config for a CLI that isn't there.
func syncOmpTheme(b Backend, home string, rt resolvedTheme) error {
	dir := filepath.Join(home, ".omp", "agent")
	if _, err := b.Stat(dir); err != nil {
		return nil // omp not set up on this host
	}
	if err := syncOmpThemeFile(b, dir, rt); err != nil {
		return err
	}
	return syncOmpThemePin(b, dir)
}

// ompColors maps herdr's UI tokens onto omp's color tokens — the same semantic
// roles lasso uses for its own chrome and for opencode's generated theme. Every
// token omp requires is present; thinkingMax is the one optional token and is
// emitted too (omp falls back to thinkingXhigh without it).
func ompColors(u uiPalette) map[string]string {
	return map[string]string{
		"accent":       u.Accent,
		"border":       u.Surface1,
		"borderAccent": u.Accent,
		"borderMuted":  u.Surface0,
		"success":      u.Green,
		"error":        u.Red,
		"warning":      u.Yellow,
		"muted":        u.Subtext0,
		"dim":          u.Overlay0,
		"text":         u.Text,
		"thinkingText": u.Subtext0,

		"selectedBg":         u.Surface0,
		"userMessageBg":      u.SurfaceDim,
		"userMessageText":    u.Text,
		"customMessageBg":    u.Surface0,
		"customMessageText":  u.Text,
		"customMessageLabel": u.Mauve,
		// Tool result frames tint their background toward the outcome, the way
		// the opencode theme tints diff rows: a 10% wash over the panel bg, so
		// success/error read at a glance without fighting the text.
		"toolPendingBg": u.SurfaceDim,
		"toolSuccessBg": blendHex(u.Green, u.PanelBg, 0.9),
		"toolErrorBg":   blendHex(u.Red, u.PanelBg, 0.9),
		"toolTitle":     u.Text,
		"toolOutput":    u.Subtext0,

		"mdHeading":         u.Mauve,
		"mdLink":            u.Blue,
		"mdLinkUrl":         u.Subtext0,
		"mdCode":            u.Green,
		"mdCodeBlock":       u.Text,
		"mdCodeBlockBorder": u.Surface1,
		"mdQuote":           u.Yellow,
		"mdQuoteBorder":     u.Surface1,
		"mdHr":              u.Overlay0,
		"mdListBullet":      u.Blue,

		"toolDiffAdded":     u.Green,
		"toolDiffRemoved":   u.Red,
		"toolDiffContext":   u.Subtext0,
		"syntaxComment":     u.Subtext0,
		"syntaxKeyword":     u.Mauve,
		"syntaxFunction":    u.Blue,
		"syntaxVariable":    u.Red,
		"syntaxString":      u.Green,
		"syntaxNumber":      u.Peach,
		"syntaxType":        u.Yellow,
		"syntaxOperator":    u.Teal,
		"syntaxPunctuation": u.Text,

		// The thinking ladder colors the editor border by reasoning level, so it
		// climbs the palette rather than repeating one hue.
		"thinkingOff":     u.Overlay0,
		"thinkingMinimal": u.Overlay1,
		"thinkingLow":     u.Blue,
		"thinkingMedium":  u.Teal,
		"thinkingHigh":    u.Mauve,
		"thinkingXhigh":   u.Peach,
		"thinkingMax":     u.Red,
		"bashMode":        u.Teal,
		"pythonMode":      u.Yellow,

		"statusLineBg":        u.Surface0,
		"statusLineSep":       u.Overlay0,
		"statusLineModel":     u.Mauve,
		"statusLinePath":      u.Blue,
		"statusLineGitClean":  u.Green,
		"statusLineGitDirty":  u.Yellow,
		"statusLineContext":   u.Teal,
		"statusLineSpend":     u.Peach,
		"statusLineStaged":    u.Green,
		"statusLineDirty":     u.Yellow,
		"statusLineUntracked": u.Red,
		"statusLineOutput":    u.Text,
		"statusLineCost":      u.Peach,
		"statusLineSubagents": u.Accent,
	}
}

// ompThemeBody renders rt as an omp custom theme.
func ompThemeBody(rt resolvedTheme) []byte {
	colors := ompColors(rt.ui)
	// A [theme.custom] override reaches us already parsed to a hex, so this is
	// belt and braces — but omp treats a non-hex token as a var reference and
	// throws when it resolves nothing, and a theme that fails to load costs the
	// whole palette (omp falls back to built-in "dark"). So a malformed token
	// takes the same token off the unmodified built-in palette instead of being
	// emitted or, as in opencode's body, dropped: omp requires all of them.
	var base map[string]string
	for tok, v := range colors {
		if _, _, _, ok := hexRGB(v); !ok {
			if base == nil {
				base = ompColors(resolveThemeByName(rt.Resolved).ui)
			}
			colors[tok] = base[tok]
		}
	}
	root := map[string]any{
		"$schema": ompThemeSchema,
		"name":    ompThemeName,
		"colors":  colors,
		"export": map[string]string{
			"pageBg": rt.ui.PanelBg,
			"cardBg": rt.ui.SurfaceDim,
			"infoBg": rt.ui.Surface0,
		},
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil
	}
	return append(out, '\n')
}

// syncOmpThemeFile writes the generated theme into omp's custom themes dir
// (<agent dir>/themes, which omp discovers and watches). Unchanged content is
// left alone: the write is what a running omp reloads on, so a no-op rewrite
// would make every poll tick restyle live sessions for nothing.
func syncOmpThemeFile(b Backend, agentDir string, rt resolvedTheme) error {
	dir := filepath.Join(agentDir, "themes")
	path := filepath.Join(dir, ompThemeName+".json")
	body := ompThemeBody(rt)
	if body == nil {
		return fmt.Errorf("render omp theme %q", rt.Resolved)
	}
	if cur, err := b.ReadFile(path); err == nil && string(cur) == string(body) {
		return nil
	}
	if err := b.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	log.Printf("theme:    omp theme file -> %s (%s) on %s", ompThemeName, rt.Resolved, b.Name())
	return b.WriteFile(path, body, 0o644)
}

// syncOmpThemePin points both of omp's mode-selected theme slots at the
// generated theme, so whichever way its terminal detection lands it loads the
// same palette — and lands on the one name whose file omp watches.
func syncOmpThemePin(b Backend, agentDir string) error {
	path := filepath.Join(agentDir, "config.yml")

	root := map[string]any{}
	data, err := b.ReadFile(path)
	switch {
	case err == nil:
		if yaml.Unmarshal(data, &root) != nil {
			return nil // don't clobber what we can't parse
		}
		if root == nil {
			root = map[string]any{} // an empty/all-comment file decodes to nil
		}
	case errors.Is(err, fs.ErrNotExist):
		// Create it — omp's dir exists, so it has run; a config it hasn't
		// written yet still gets the pin on the next launch.
	default:
		return err
	}

	// A legacy flat `theme: "name"` (omp migrates it on read) is replaced by the
	// nested form rather than merged into.
	theme, _ := root["theme"].(map[string]any)
	if theme == nil {
		theme = map[string]any{}
	}
	dark, _ := theme["dark"].(string)
	light, _ := theme["light"].(string)
	if dark == ompThemeName && light == ompThemeName {
		return nil
	}
	theme["dark"], theme["light"] = ompThemeName, ompThemeName
	root["theme"] = theme

	out, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	log.Printf("theme:    omp theme pin -> %s on %s", ompThemeName, b.Name())
	return b.WriteFile(path, out, 0o644)
}

// ---------------------------------------------------------------------------
// Claude Code — ~/.claude/themes/herdr.json (mapped palette, as herdr-theme-sync wrote)
// ---------------------------------------------------------------------------

// claudeThemeFile is the on-disk shape Claude Code expects for a custom theme:
// a base (its own dark/light palette) plus per-token hex overrides. Only tokens
// Claude knows are honored; unknown keys are ignored, so a superset is safe.
type claudeThemeFile struct {
	Name      string            `json:"name"`
	Base      string            `json:"base"`
	Overrides map[string]string `json:"overrides"`
}

// claudeOverrides maps herdr's UI tokens onto Claude Code's theme tokens
// (ported from herdr-theme-sync's mapping).
func claudeOverrides(p uiPalette) map[string]string {
	m := map[string]string{}
	put := func(hex string, toks ...string) {
		if hex == "" {
			return // let Claude's base show through
		}
		for _, t := range toks {
			m[t] = hex
		}
	}
	put(p.Text, "text")
	put(p.PanelBg, "background", "inverseText")
	put(p.Surface0, "userMessageBackground", "bashMessageBackgroundColor")
	put(p.Surface1, "userMessageBackgroundHover", "selectionBg")
	put(p.SurfaceDim, "composerSidebarBackground", "memoryBackgroundColor")
	put(p.Overlay0, "subtle", "inactive")
	put(p.Overlay1, "secondaryBorder", "suggestion")
	put(p.Accent, "permission", "ide", "promptBorder", "bashBorder")
	put(p.Mauve, "planMode", "thinking", "merged")
	put(p.Teal, "remember")
	put(p.Green, "success", "autoAccept", "diffAdded")
	put(p.Red, "error", "diffRemoved")
	put(p.Yellow, "warning")

	// Dimmed diff variants: blend the accent toward the background.
	bg := p.PanelBg
	if bg == "" {
		if luminance(p.Text) > 0.5 {
			bg = "#000000"
		} else {
			bg = "#ffffff"
		}
	}
	if p.Green != "" {
		m["diffAddedDimmed"] = blendHex(p.Green, bg, 0.6)
	}
	if p.Red != "" {
		m["diffRemovedDimmed"] = blendHex(p.Red, bg, 0.6)
	}
	return m
}

func syncClaudeTheme(b Backend, home string, rt resolvedTheme, light bool) error {
	path := filepath.Join(home, ".claude", "themes", "herdr.json")
	base := "dark"
	if light {
		base = "light"
	}
	theme := claudeThemeFile{
		Name:      "herdr (" + rt.Resolved + ")",
		Base:      base,
		Overrides: claudeOverrides(rt.ui),
	}
	out, err := json.MarshalIndent(theme, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	if cur, err := b.ReadFile(path); err == nil && string(cur) == string(out) {
		return nil
	}
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return b.WriteFile(path, out, 0o644)
}

// ---------------------------------------------------------------------------
// Ghostty — ~/.config/ghostty/themes/herdr + `theme = herdr` in the config
// ---------------------------------------------------------------------------

// ghosttyConfigPaths are the config files ghostty may read, relative to home.
// We only ever rewrite ones that already exist — a host without ghostty should
// not sprout a ghostty config. macOS keeps its config under Application Support
// (both bare and .ghostty-suffixed spellings are in the wild); the XDG path
// works on both platforms.
var ghosttyConfigPaths = [][]string{
	{".config", "ghostty", "config"},
	{"Library", "Application Support", "com.mitchellh.ghostty", "config"},
	{"Library", "Application Support", "com.mitchellh.ghostty", "config.ghostty"},
}

// ghosttyThemeBody renders rt as a ghostty theme file: the same chrome + ANSI
// split xtermJSON uses, so a ghostty window matches lasso's embedded terminal.
// Ghostty has no alpha on selection-background, so the translucent accent wash
// (see termSelectionAlpha) is pre-composited over the panel background instead.
func ghosttyThemeBody(rt resolvedTheme) []byte {
	u, a := rt.ui, rt.ansi
	var b []byte
	put := func(k, v string) {
		if v != "" {
			b = append(b, (k + " = " + v + "\n")...)
		}
	}
	b = append(b, ("# Generated by lasso — herdr theme " + rt.Resolved + ". Edits are overwritten.\n")...)
	put("background", u.PanelBg)
	put("foreground", u.Text)
	put("cursor-color", u.Text)
	put("cursor-text", u.PanelBg)
	put("selection-background", blendHex(u.Accent, u.PanelBg, 1-float64(termSelectionAlpha)/255))
	put("selection-foreground", u.Text)
	for i, hex := range []string{
		a.Black, a.Red, a.Green, a.Yellow, a.Blue, a.Magenta, a.Cyan, a.White,
		a.BrightBlack, a.BrightRed, a.BrightGreen, a.BrightYellow,
		a.BrightBlue, a.BrightMagenta, a.BrightCyan, a.BrightWhite,
	} {
		put("palette", strconv.Itoa(i)+"="+hex)
	}
	return b
}

// ghosttySetTheme points a ghostty config's `theme` key at our generated theme,
// returning the new body and whether anything changed. Existing assignments are
// rewritten in place (rather than appended past, which ghostty would also honor)
// so the file doesn't grow a line per theme change and keeps the user's
// ordering; a config with no theme key gets one appended. Any prior value —
// including a `light:X,dark:Y` pair — is replaced: once lasso syncs ghostty it
// owns the choice.
func ghosttySetTheme(body []byte, name string) ([]byte, bool) {
	want := "theme = " + name
	lines := strings.Split(string(body), "\n")
	found := false
	for i, ln := range lines {
		k, _, ok := strings.Cut(ln, "=")
		if !ok || strings.TrimSpace(k) != "theme" {
			continue // not an assignment, or not ours (comments have no bare key)
		}
		lines[i], found = want, true
	}
	if !found {
		// Keep exactly one trailing newline whether or not the file had one.
		lines = nil
		if trimmed := strings.TrimRight(string(body), "\n"); trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		}
		lines = append(lines, want, "")
	}
	out := strings.Join(lines, "\n")
	if out == string(body) {
		return nil, false
	}
	return []byte(out), true
}

// ghosttyThemeName is the theme file lasso writes and points ghostty's config at.
const ghosttyThemeName = "herdr"

func syncGhosttyTheme(b Backend, home string, rt resolvedTheme) error {
	// Ghostty resolves bare theme names against ~/.config/ghostty/themes on
	// every platform, so one location covers macOS and Linux.
	path := filepath.Join(home, ".config", "ghostty", "themes", ghosttyThemeName)
	body := ghosttyThemeBody(rt)
	if cur, err := b.ReadFile(path); err != nil || string(cur) != string(body) {
		if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := b.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}

	for _, parts := range ghosttyConfigPaths {
		cfg := filepath.Join(append([]string{home}, parts...)...)
		cur, err := b.ReadFile(cfg)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		next, changed := ghosttySetTheme(cur, ghosttyThemeName)
		if !changed {
			continue
		}
		log.Printf("theme:    ghostty theme -> %s in %s on %s", ghosttyThemeName, cfg, b.Name())
		if err := b.WriteFile(cfg, next, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// lasso settings.json — .theme.resolved (claude-contextline's light/dark cue)
// ---------------------------------------------------------------------------

func syncLassoResolved(b Backend, home string, light bool) error {
	path := filepath.Join(home, ".lasso", "settings.json")
	resolved := "dark"
	if light {
		resolved = "light"
	}

	root := map[string]json.RawMessage{}
	if data, err := b.ReadFile(path); err == nil {
		if json.Unmarshal(data, &root) != nil {
			return nil // don't clobber an unparseable file
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	theme := map[string]json.RawMessage{}
	if raw, ok := root["theme"]; ok {
		_ = json.Unmarshal(raw, &theme)
	}
	var cur string
	if raw, ok := theme["resolved"]; ok {
		_ = json.Unmarshal(raw, &cur)
	}
	if cur == resolved {
		return nil // already in step; leave the file (and its mtime) untouched
	}
	set := func(m map[string]json.RawMessage, k string, v any) {
		b, _ := json.Marshal(v)
		m[k] = b
	}
	set(theme, "mode", "herdr")
	set(theme, "resolved", resolved)
	set(theme, "updatedAt", time.Now().UTC().Format(time.RFC3339))
	set(root, "theme", theme)

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := b.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return b.WriteFile(path, out, 0o644)
}

// ---------------------------------------------------------------------------
// color math (ported from herdr-theme-sync)
// ---------------------------------------------------------------------------

func hexRGB(hex string) (r, g, b int, ok bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), true
}

// luminance returns the relative luminance (0..1) of a "#rrggbb" color.
func luminance(hex string) float64 {
	r, g, b, ok := hexRGB(hex)
	if !ok {
		return 0
	}
	return (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 255.0
}

// blendHex mixes color a toward color b by fraction t (0..1).
func blendHex(a, b string, t float64) string {
	ar, ag, ab, ok1 := hexRGB(a)
	br, bg, bb, ok2 := hexRGB(b)
	if !ok1 || !ok2 {
		return a
	}
	mix := func(x, y int) int { return int(float64(x)*(1-t) + float64(y)*t + 0.5) }
	return fmt.Sprintf("#%02x%02x%02x", mix(ar, br), mix(ag, bg), mix(ab, bb))
}
