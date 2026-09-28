package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Browser profiles: named shared browsers, each with its own cookies, logins
// and proxy. The default profile is the one lasso always had (its profile dir,
// its browser_proxy setting, bare /cdp and /browser-mcp); every other profile
// lives at /cdp/p/<id> and /browser-mcp/<id>.
//
// A profile is its own Chromium PROCESS, not a browser context inside one.
// Two things force that. Chromium reads --proxy-server once per process, and
// CDP's per-context proxyServer only exists for throwaway contexts. And a
// throwaway context is exactly what a profile must not be: Target.
// createBrowserContext contexts are incognito-like, so cookies, localStorage
// and IndexedDB would die with every idle stop. A user-data-dir per profile is
// the one thing that persists all of it. The cost is a process (and a resource
// cap) per profile that is in use; each still stops on its own after
// -browser-idle with nothing connected, so an unused profile costs nothing.
//
// Profiles are stored as one JSON list in the settings table (browser_profiles),
// minus the default, whose proxy stays in browser_proxy so an older lasso
// reading the same database keeps working. Deleting a profile deletes its
// profile directory: the logins in it go with it.

// defaultBrowserProfile is the id of the profile lasso always had.
const defaultBrowserProfile = "default"

// browserProfilesSetting holds the non-default profiles as JSON.
const browserProfilesSetting = "browser_profiles"

// browserDefaultNameSetting holds the default profile's display name, when the
// human renamed it.
const browserDefaultNameSetting = "browser_default_profile_name"

// browserProfileMax bounds how many profiles can exist. Each running one is a
// Chromium; the limit is on how many a runaway agent can create, not on use.
const browserProfileMax = 32

type browserProfile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Proxy     string `json:"proxy"`
	CreatedAt string `json:"created_at"`
}

// browserProfileID is what an id may be: it is a path segment in two URLs and
// a directory name, so nothing that could mean anything else in either.
var browserProfileID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validBrowserProfileID(id string) error {
	if !browserProfileID.MatchString(id) {
		return fmt.Errorf("profile id %q must be 1-32 lowercase letters, digits or dashes, starting with a letter or digit", id)
	}
	return nil
}

// slugProfileID derives an id from a display name ("Work (US)" -> "work-us").
func slugProfileID(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 28 {
		s = strings.TrimRight(s[:28], "-")
	}
	if s == "" {
		s = "profile"
	}
	return s
}

func cleanProfileName(name string) (string, error) {
	n := strings.Join(strings.Fields(name), " ")
	if n == "" {
		return "", errors.New("a profile needs a name")
	}
	if len([]rune(n)) > 64 {
		return "", errors.New("profile names are at most 64 characters")
	}
	return n, nil
}

// loadExtraProfiles reads the non-default profiles. A list that does not parse
// reads as empty and says so: it must not take the default profile down.
func loadExtraProfiles() []browserProfile {
	raw, _ := getSetting(browserProfilesSetting)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var ps []browserProfile
	if err := json.Unmarshal([]byte(raw), &ps); err != nil {
		log.Printf("browser: ignoring unreadable %s setting: %v", browserProfilesSetting, err)
		return nil
	}
	out := ps[:0]
	for _, p := range ps {
		if validBrowserProfileID(p.ID) == nil && p.ID != defaultBrowserProfile {
			out = append(out, p)
		}
	}
	return out
}

func saveExtraProfiles(ps []browserProfile) error {
	if ps == nil {
		ps = []browserProfile{}
	}
	b, err := json.Marshal(ps)
	if err != nil {
		return err
	}
	return setSetting(browserProfilesSetting, string(b))
}

func defaultProfileName() string {
	if n, _ := getSetting(browserDefaultNameSetting); strings.TrimSpace(n) != "" {
		return n
	}
	return "Default"
}

// allBrowserProfiles is every profile, the default first.
func allBrowserProfiles() []browserProfile {
	proxy, _ := getSetting(browserProxySetting)
	return append([]browserProfile{{ID: defaultBrowserProfile, Name: defaultProfileName(), Proxy: proxy}}, loadExtraProfiles()...)
}

// ---------------------------------------------------------------------------
// the fleet: one browserManager per profile
// ---------------------------------------------------------------------------

// browserFleet owns the non-default profiles' managers. The default profile's
// manager is sharedBrowser, as it always was (tests swap it in directly).
type browserFleet struct {
	cfg browserConfig
	// onStop is told which profile's browser went away; the /browser-mcp
	// bridge closes that profile's sessions.
	onStop func(profile, why string)

	mu   sync.Mutex // guards mgrs and every write to the stored profile list
	mgrs map[string]*browserManager
}

// sharedBrowsers is the process-wide fleet main wires up; nil means only the
// default profile exists.
var sharedBrowsers *browserFleet

func newBrowserFleet(cfg browserConfig) *browserFleet {
	return &browserFleet{cfg: cfg, mgrs: map[string]*browserManager{}}
}

// browserFor resolves a profile id to its manager. "" is the default profile.
func browserFor(profile string) (*browserManager, error) {
	if profile == "" || profile == defaultBrowserProfile {
		if sharedBrowser == nil {
			return nil, errors.New("the shared browser is not configured on this lasso")
		}
		return sharedBrowser, nil
	}
	if sharedBrowsers == nil {
		return nil, fmt.Errorf("no browser profile %q", profile)
	}
	return sharedBrowsers.manager(profile)
}

// manager returns a non-default profile's manager, creating it on first use.
func (f *browserFleet) manager(id string) (*browserManager, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.mgrs[id]; ok {
		return m, nil
	}
	for _, p := range loadExtraProfiles() {
		if p.ID == id {
			return f.newManagerLocked(id), nil
		}
	}
	return nil, fmt.Errorf("no browser profile %q", id)
}

func (f *browserFleet) profileDir(id string) string {
	return filepath.Join(f.cfg.Dir, "browser-profiles", id)
}

// newManagerLocked builds a profile's manager. Its proxy is read from the
// stored list on every launch, so an edit made while it was stopped applies to
// the next start without the manager holding a copy.
func (f *browserFleet) newManagerLocked(id string) *browserManager {
	m := newBrowserManager(f.cfg)
	m.id, m.dir = id, f.profileDir(id)
	m.proxy = func() string {
		for _, p := range loadExtraProfiles() {
			if p.ID == id {
				return p.Proxy
			}
		}
		return ""
	}
	m.saveProxy = func(v string) error {
		_, err := f.update(id, nil, &v)
		return err
	}
	m.onStop = func(why string) {
		if f.onStop != nil {
			f.onStop(id, why)
		}
	}
	f.mgrs[id] = m
	return m
}

// managers is every manager that exists, the default first. A profile that
// has never been used has none yet, and needs none.
func (f *browserFleet) managers() []*browserManager {
	var out []*browserManager
	if sharedBrowser != nil {
		out = append(out, sharedBrowser)
	}
	if f == nil {
		return out
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.mgrs {
		out = append(out, m)
	}
	return out
}

// run is every profile's idle reaper and, on ctx, their shutdown.
func (f *browserFleet) run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			f.shutdown()
			return
		case <-t.C:
			for _, m := range f.managers() {
				m.reapIdle()
			}
		}
	}
}

// shutdown stops every profile's browser at once: each stop can take the
// SIGTERM grace, and they are separate processes.
func (f *browserFleet) shutdown() {
	var wg sync.WaitGroup
	for _, m := range f.managers() {
		wg.Add(1)
		go func() { defer wg.Done(); m.shutdown() }()
	}
	wg.Wait()
}

// create adds a profile. id "" derives one from the name, made unique.
func (f *browserFleet) create(name, id, proxy string) (browserProfile, error) {
	name, err := cleanProfileName(name)
	if err != nil {
		return browserProfile{}, err
	}
	proxy, err = validateBrowserProxy(proxy)
	if err != nil {
		return browserProfile{}, err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	explicit := id != ""
	if !explicit {
		id = slugProfileID(name)
	}
	if err := validBrowserProfileID(id); err != nil {
		return browserProfile{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ps := loadExtraProfiles()
	if len(ps)+1 >= browserProfileMax {
		return browserProfile{}, fmt.Errorf("lasso allows at most %d browser profiles; delete one first", browserProfileMax)
	}
	taken := func(c string) bool {
		if c == defaultBrowserProfile {
			return true
		}
		for _, p := range ps {
			if p.ID == c {
				return true
			}
		}
		return false
	}
	if taken(id) {
		if explicit {
			return browserProfile{}, fmt.Errorf("a browser profile with id %q already exists", id)
		}
		base := id
		for n := 2; taken(id); n++ {
			id = base + "-" + strconv.Itoa(n)
		}
	}
	for _, p := range ps {
		if strings.EqualFold(p.Name, name) {
			return browserProfile{}, fmt.Errorf("a browser profile named %q already exists", name)
		}
	}
	if strings.EqualFold(defaultProfileName(), name) {
		return browserProfile{}, fmt.Errorf("a browser profile named %q already exists", name)
	}
	p := browserProfile{ID: id, Name: name, Proxy: proxy, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	// A directory left by an earlier profile with the same id would hand the
	// new one its predecessor's logins.
	_ = os.RemoveAll(f.profileDir(id))
	if err := saveExtraProfiles(append(ps, p)); err != nil {
		return browserProfile{}, fmt.Errorf("save: %w", err)
	}
	log.Printf("browser: created profile %q (%s)", id, name)
	return p, nil
}

// update renames a profile and/or stores a new proxy for it. It does NOT
// relaunch a running browser; applyProxy (the caller for a proxy change) does.
func (f *browserFleet) update(id string, name, proxy *string) (browserProfile, error) {
	if name != nil {
		n, err := cleanProfileName(*name)
		if err != nil {
			return browserProfile{}, err
		}
		name = &n
	}
	if proxy != nil {
		v, err := validateBrowserProxy(*proxy)
		if err != nil {
			return browserProfile{}, err
		}
		proxy = &v
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ps := loadExtraProfiles()
	if name != nil {
		clash := strings.EqualFold(*name, defaultProfileName()) && id != defaultBrowserProfile
		for _, p := range ps {
			if p.ID != id && strings.EqualFold(p.Name, *name) {
				clash = true
			}
		}
		if clash {
			return browserProfile{}, fmt.Errorf("a browser profile named %q already exists", *name)
		}
	}
	if id == defaultBrowserProfile {
		if name != nil {
			if err := setSetting(browserDefaultNameSetting, *name); err != nil {
				return browserProfile{}, fmt.Errorf("save: %w", err)
			}
		}
		if proxy != nil {
			if err := setSetting(browserProxySetting, *proxy); err != nil {
				return browserProfile{}, fmt.Errorf("save: %w", err)
			}
		}
		p, _ := getSetting(browserProxySetting)
		return browserProfile{ID: id, Name: defaultProfileName(), Proxy: p}, nil
	}
	for i := range ps {
		if ps[i].ID != id {
			continue
		}
		if name != nil {
			ps[i].Name = *name
		}
		if proxy != nil {
			ps[i].Proxy = *proxy
		}
		if err := saveExtraProfiles(ps); err != nil {
			return browserProfile{}, fmt.Errorf("save: %w", err)
		}
		return ps[i], nil
	}
	return browserProfile{}, fmt.Errorf("no browser profile %q", id)
}

// remove deletes a profile: its browser stops, it leaves the list, and its
// profile directory — every cookie and login in it — is deleted.
func (f *browserFleet) remove(ctx context.Context, id string) error {
	if id == defaultBrowserProfile {
		return errors.New("the default browser profile cannot be deleted")
	}
	f.mu.Lock()
	ps := loadExtraProfiles()
	idx := -1
	for i, p := range ps {
		if p.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		f.mu.Unlock()
		return fmt.Errorf("no browser profile %q", id)
	}
	m := f.mgrs[id]
	delete(f.mgrs, id)
	if err := saveExtraProfiles(append(ps[:idx:idx], ps[idx+1:]...)); err != nil {
		f.mu.Unlock()
		return fmt.Errorf("save: %w", err)
	}
	f.mu.Unlock()
	if m != nil {
		// Retired first, so a request already holding the manager cannot
		// launch a browser into the directory about to be removed.
		m.retired.Store(true)
		if err := m.stop(ctx, "profile deleted"); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(f.profileDir(id)); err != nil {
		log.Printf("browser: removing profile %q's directory: %v", id, err)
	}
	log.Printf("browser: deleted profile %q", id)
	return nil
}

// statuses is every profile's status, the default first. Profiles that have
// never run are reported stopped without creating a manager for them.
func (f *browserFleet) statuses() []browserProfileStatus {
	out := []browserProfileStatus{}
	for _, p := range allBrowserProfiles() {
		out = append(out, f.statusOf(p))
	}
	return out
}

func (f *browserFleet) statusOf(p browserProfile) browserProfileStatus {
	var m *browserManager
	if p.ID == defaultBrowserProfile {
		m = sharedBrowser
	} else if f != nil {
		f.mu.Lock()
		m = f.mgrs[p.ID]
		f.mu.Unlock()
	}
	var st browserProfileStatus
	if m != nil {
		st = m.profileStatus()
	} else {
		st = browserProfileStatus{ID: p.ID, Default: p.ID == defaultBrowserProfile, Pages: []browserPage{},
			WSPath: cdpPathFor(p.ID), MCPPath: browserMCPPathFor(p.ID)}
	}
	st.Name, st.Proxy = p.Name, p.Proxy
	return st
}

// profileStatus is one profile's status by id.
func (f *browserFleet) profileStatus(id string) (browserProfileStatus, error) {
	for _, p := range allBrowserProfiles() {
		if p.ID == id {
			return f.statusOf(p), nil
		}
	}
	return browserProfileStatus{}, fmt.Errorf("no browser profile %q", id)
}

// resolveProfile maps what an agent or a request names — an id, or a display
// name in any case — to a profile id. "" is the default.
func resolveProfile(arg string) (string, error) {
	a := strings.TrimSpace(arg)
	if a == "" {
		return defaultBrowserProfile, nil
	}
	ps := allBrowserProfiles()
	for _, p := range ps {
		if p.ID == strings.ToLower(a) {
			return p.ID, nil
		}
	}
	for _, p := range ps {
		if strings.EqualFold(p.Name, a) {
			return p.ID, nil
		}
	}
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, fmt.Sprintf("%s (%q)", p.ID, p.Name))
	}
	return "", fmt.Errorf("no browser profile %q; profiles: %s", a, strings.Join(names, ", "))
}

// browserProfilesChanged tells every connected tab to refetch the profile list:
// an agent (or another browser) created, renamed or deleted one, and the
// Browser tab's selector should show it without a reload.
var browserProfilesChanged = func() {
	if srvHub != nil {
		srvHub.broadcast("browser-profiles", struct{}{})
	}
}

// ---------------------------------------------------------------------------
// HTTP: /api/browser (status + actions), /api/browser/proxy,
// /api/browser/profiles[/<id>]
// ---------------------------------------------------------------------------

// status is /api/browser's answer: the default profile's fields at the top, as
// before profiles, plus every profile.
func (f *browserFleet) status() browserStatus {
	st := sharedBrowser.status()
	st.Profiles = f.statuses()
	return st
}

func (f *browserFleet) serveStatus(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, f.status())
	case http.MethodPost:
		var body struct {
			Action  string `json:"action"`
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		id, err := resolveProfile(body.Profile)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		m, err := browserFor(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		switch body.Action {
		case "start":
			_, err = m.ensure(r.Context())
		case "stop":
			err = m.stop(r.Context(), "stopped from the API")
		case "restart":
			if err = m.stop(r.Context(), "restart requested"); err == nil {
				_, err = m.ensure(r.Context())
			}
		default:
			http.Error(w, `action must be "start", "stop" or "restart"`, http.StatusBadRequest)
			return
		}
		st := f.status()
		if err != nil {
			// The reason the tab shows is the failing profile's, not the
			// default's: the top level only describes the default.
			if ps, e := f.profileStatus(id); e == nil && ps.Reason != "" {
				st.Reason = ps.Reason
			} else if st.Reason == "" || id != defaultBrowserProfile {
				st.Reason = err.Error()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(st)
			return
		}
		writeJSON(w, st)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *browserFleet) serveProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Proxy   string `json:"proxy"`
		Profile string `json:"profile"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id, err := resolveProfile(body.Profile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	m, err := browserFor(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	err = m.applyProxy(r.Context(), body.Proxy)
	if err == nil || !errors.As(err, new(errBadProxy)) {
		browserProfilesChanged()
	}
	m.writeProxyResult(w, err, func() browserStatus {
		// A failed relaunch must report THIS profile's reason: the top level
		// only describes the default.
		st := f.status()
		if id != defaultBrowserProfile {
			ps, _ := f.profileStatus(id)
			st.Reason = ps.Reason
		}
		return st
	})
}

// serveProfiles is /api/browser/profiles (GET list, POST create) and
// /api/browser/profiles/<id> (GET, PATCH, DELETE). Validation failures are
// 400 with the sentence as plain text, which the dialog shows as-is.
func (f *browserFleet) serveProfiles(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/browser/profiles"), "/")
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, f.statuses())
		case http.MethodPost:
			var body struct {
				Name  string `json:"name"`
				ID    string `json:"id"`
				Proxy string `json:"proxy"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			p, err := f.create(body.Name, body.ID, body.Proxy)
			if err != nil {
				http.Error(w, err.Error(), profileErrStatus(err))
				return
			}
			browserProfilesChanged()
			writeJSON(w, f.statusOf(p))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		st, err := f.profileStatus(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, st)
	case http.MethodPatch:
		var body struct {
			Name  *string `json:"name"`
			Proxy *string `json:"proxy"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		st, err := f.edit(r.Context(), id, body.Name, body.Proxy)
		if err != nil {
			http.Error(w, err.Error(), profileErrStatus(err))
			return
		}
		writeJSON(w, st)
	case http.MethodDelete:
		if err := f.remove(r.Context(), id); err != nil {
			http.Error(w, err.Error(), profileErrStatus(err))
			return
		}
		browserProfilesChanged()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// edit renames a profile and/or changes its proxy, relaunching a running
// browser for a new proxy (the pages it had are reopened). A relaunch that
// fails leaves the change stored and says so.
func (f *browserFleet) edit(ctx context.Context, id string, name, proxy *string) (browserProfileStatus, error) {
	if _, err := f.profileStatus(id); err != nil {
		return browserProfileStatus{}, err
	}
	if name != nil {
		if _, err := f.update(id, name, nil); err != nil {
			return browserProfileStatus{}, err
		}
	}
	var relaunchErr error
	if proxy != nil {
		m, err := browserFor(id)
		if err != nil {
			return browserProfileStatus{}, err
		}
		if err := m.applyProxy(ctx, *proxy); err != nil {
			var bad errBadProxy
			if errors.As(err, &bad) || strings.HasPrefix(err.Error(), "save: ") {
				return browserProfileStatus{}, err
			}
			relaunchErr = err
		}
	}
	browserProfilesChanged()
	st, err := f.profileStatus(id)
	if err != nil {
		return browserProfileStatus{}, err
	}
	if relaunchErr != nil {
		return st, errRelaunch{fmt.Errorf("saved, but relaunching the browser failed: %w", relaunchErr)}
	}
	return st, nil
}

// errRelaunch is a change that was stored but could not be applied.
type errRelaunch struct{ error }

func profileErrStatus(err error) int {
	var bad errBadProxy
	var rl errRelaunch
	switch {
	case errors.As(err, &rl):
		return http.StatusBadGateway
	case errors.As(err, &bad):
		return http.StatusBadRequest
	case strings.HasPrefix(err.Error(), "no browser profile"):
		return http.StatusNotFound
	case strings.HasPrefix(err.Error(), "save: "):
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}
