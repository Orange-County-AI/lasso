package main

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"time"
)

// herdr 0.9's saved MACHINES are a client-side feature, and that is what makes
// them invisible here: one herdr client holds an ssh bridge per saved machine
// (`herdr machine list`) and draws whichever one is selected, while every server
// involved — including the local one lasso polls — stays unaware. Its socket API
// carries no machine method and no machine field (protocol 22), and herdr's own
// docs say selecting a machine "does not retarget CLI commands running in an
// existing pane". So a tab whose terminal is showing ticket500's session still
// answers every question lasso asks with the LOCAL herdr's focused pane: the
// Files sidebar browses titan while the screen shows ticket500, and a pasted
// screenshot is written to a machine the agent reading it cannot see — which is
// how an omp agent on ticket500 ends up told to open a /home/stephan path that
// only exists on titan.
//
// The one thing the client does leave behind is its selection. It writes
// ~/.local/state/herdr/client/endpoint-selection.json within a second of the
// switch (`{"version":1,"selected_profile":<id|null>}`, null meaning Local), and
// ~/.local/state/herdr/client/endpoints.json holds the saved profiles that id
// resolves through. Pair them and the machine on screen is recoverable.
//
// It is another program's private state, so every failure is silent and falls
// back to the local answer: a missing or unparseable file, an unknown id, a
// disabled profile, a target lasso may not drive. Two known limits, neither of
// which can produce a path on a host lasso cannot reach:
//
//   - The file belongs to the MACHINE, not to one client. Two herdr clients open
//     on different machines share it, and the last one to switch wins.
//   - A dead client leaves its last selection behind. The next client to run
//     rewrites it, so the window is small, and being wrong here costs the same
//     paste that is wrong today.

const (
	// The selection file is re-read at most this often. It is a local read of a
	// ~50-byte file, and it has to be near-live: someone switches machine and
	// pastes a screenshot a second later.
	clientSelectionTTL = time.Second
	// Profiles change only when `herdr machine add/rename/remove` runs, so they
	// are held far longer — and a selection naming an id we don't know forces a
	// re-read anyway (see machineProfile), which covers a machine added and
	// selected inside the window.
	clientProfilesTTL = 30 * time.Second
)

// herdrMachineProfile is one entry of the client's endpoints.json: an opaque id,
// the ssh target, and the remote session the profile targets.
type herdrMachineProfile struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Target  string `json:"target"`
	Session string `json:"session"`
	Enabled bool   `json:"enabled"`
}

// clientMachineHop reports the saved machine the terminal's own herdr client is
// showing, as the same sshHop an ssh attach produces — host set, agent empty,
// since a machine selection shows that server's own focused pane rather than a
// named agent. ok is false whenever the client is on Local, or the selection
// cannot be resolved to a host lasso may drive.
//
// Only the LOCAL backend can have one. Every ttyd lasso spawns runs on lasso's
// own box, so a tab on a remote host is a `herdr --remote <alias>` client — a
// standalone attach pinned to that one server, with no machine sidebar to
// select from. Reading the shared selection file for such a tab would redirect
// it at whatever machine the local client happens to be showing, which is a
// different machine's filesystem than the terminal is displaying.
func clientMachineHop(be Backend) (sshHop, bool) {
	if be.Name() != "local" {
		return sshHop{}, false
	}
	id := selectedMachineID(be)
	if id == "" {
		return sshHop{}, false
	}
	p, ok := machineProfile(be, id)
	// A disabled profile is not connected, so it cannot be on screen. A profile
	// naming a non-default remote session points at a herdr server lasso does
	// not talk to — its host backend dials that host's default socket — so its
	// panes are not the ones being displayed.
	if !ok || !p.Enabled || (p.Session != "" && p.Session != "default") {
		return sshHop{}, false
	}
	host := hostAliasFor(p.Target)
	if host == "" || host == be.Name() {
		return sshHop{}, false
	}
	return sshHop{host: host}, true
}

// clientStateDir is where the herdr client keeps its state on a host. A client
// that honors a non-default XDG_STATE_HOME writes elsewhere and simply reads as
// "no machine selected" — the local answer, which is today's behavior.
func clientStateDir(b Backend) string {
	home, err := b.HomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "herdr", "client")
}

type selectionEntry struct {
	id string
	at time.Time
}

var selectionCache = struct {
	sync.Mutex
	m map[string]selectionEntry
}{m: map[string]selectionEntry{}}

// selectedMachineID is the profile id the client is showing; "" for Local, and
// for every unreadable state.
func selectedMachineID(b Backend) string {
	key := b.Name()
	selectionCache.Lock()
	if e, ok := selectionCache.m[key]; ok && time.Since(e.at) < clientSelectionTTL {
		selectionCache.Unlock()
		return e.id
	}
	selectionCache.Unlock()

	id := readSelectedMachineID(b)
	selectionCache.Lock()
	selectionCache.m[key] = selectionEntry{id: id, at: time.Now()}
	selectionCache.Unlock()
	return id
}

func readSelectedMachineID(b Backend) string {
	dir := clientStateDir(b)
	if dir == "" {
		return ""
	}
	data, err := b.ReadFile(filepath.Join(dir, "endpoint-selection.json"))
	if err != nil {
		return ""
	}
	// selected_profile is null on Local, so the pointer distinguishes "the
	// client is on Local" from a file we failed to understand. Both answer "".
	var sel struct {
		SelectedProfile *string `json:"selected_profile"`
	}
	if json.Unmarshal(data, &sel) != nil || sel.SelectedProfile == nil {
		return ""
	}
	return *sel.SelectedProfile
}

type profilesEntry struct {
	m  map[string]herdrMachineProfile
	at time.Time
}

var profilesCache = struct {
	sync.Mutex
	m map[string]profilesEntry
}{m: map[string]profilesEntry{}}

// machineProfile looks up one saved profile by id. A cached set that doesn't
// know the id is re-read once before giving up, so a machine added and selected
// inside clientProfilesTTL still resolves.
func machineProfile(b Backend, id string) (herdrMachineProfile, bool) {
	key := b.Name()
	profilesCache.Lock()
	e, cached := profilesCache.m[key]
	profilesCache.Unlock()
	if cached && time.Since(e.at) < clientProfilesTTL {
		if p, ok := e.m[id]; ok {
			return p, true
		}
	}

	m := readMachineProfiles(b)
	profilesCache.Lock()
	profilesCache.m[key] = profilesEntry{m: m, at: time.Now()}
	profilesCache.Unlock()
	p, ok := m[id]
	return p, ok
}

func readMachineProfiles(b Backend) map[string]herdrMachineProfile {
	dir := clientStateDir(b)
	if dir == "" {
		return nil
	}
	data, err := b.ReadFile(filepath.Join(dir, "endpoints.json"))
	if err != nil {
		return nil
	}
	var f struct {
		SSH []herdrMachineProfile `json:"ssh"`
	}
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	m := make(map[string]herdrMachineProfile, len(f.SSH))
	for _, p := range f.SSH {
		if p.ID != "" {
			m[p.ID] = p
		}
	}
	return m
}
