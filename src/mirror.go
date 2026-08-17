package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// herdr-mirror (a herdr plugin) streams other machines' herdr workspaces into
// THIS herdr's sidebar. Each mirrored remote workspace is a real local
// workspace, and each mirrored pane a real local pane running `herdr-mirror
// pane <ssh-target> <remoteWs>:<remotePane>` — so to lasso, which only ever
// asked herdr for a pane listing, a mirror of someone else's agent is
// indistinguishable from a pane on this box. On titan that is 32 of 61 panes.
//
// Three signals could tell them apart, and only one of them is both cheap and
// correct:
//
//   - The workspace label prefix ("ocai: clem"). Wrong on two counts: the
//     prefix is configurable per host (hosts.toml `prefix`), and a local
//     workspace is free to have a colon in its name.
//   - The pane's foreground argv, via herdr's pane.process_info. Authoritative,
//     and it carries the ssh target — but it is one RPC per pane, paid on every
//     /api/grid poll. activeCwd affords that for the ONE focused pane; the grid
//     cannot afford it 61 times.
//   - The daemon's own object map, ~/.local/state/herdr-mirror/<host>-map.json,
//     which is what this file reads. It is the mapping herdr-mirror itself
//     works from: remote id -> local id, per host, plus the workspace's label as
//     it reads on the REMOTE (`lastRemoteLabel`) — so the host attribution and
//     the un-prefixed label both come from the daemon rather than from parsing
//     its output back out of a display string. One small file read per mirrored
//     host, cached, and gated (see mirrorSentinelCwd) so a host running no
//     mirrors pays nothing at all.
//
// Staleness is self-correcting in the one direction that matters: a map entry
// whose local workspace is gone (a retired host's file lingers) matches no pane
// and is silently ignored, while a pane whose entry has not been written yet is
// simply not attributed until the next refresh.

// mirrorStateDir is herdr-mirror's per-host state directory, relative to the
// home of the account herdr runs as.
const mirrorStateDir = ".local/state/herdr-mirror"

// mirrorSentinelCwd is the directory every mirror pane's shell reports as its
// cwd (herdr-mirror parks the streamer there, since a mirror has no local
// working directory of its own). It is a GATE, not a classifier: a real local
// pane that happens to cd in there reports it too — verified on titan, where
// the workspace herdr-mirror was installed from does exactly that — so it may
// only be used to decide whether reading the maps is worth it, never to decide
// that a pane is a mirror.
const mirrorSentinelCwd = "herdr-mirror/.mirror-pane"

// mirrorRef attributes one local object to the remote it mirrors.
type mirrorRef struct {
	// Host is the hosts.toml section key — the name the sidebar prefix defaults
	// to, and the name this lasso groups the row under.
	Host string
	// Workspace / Pane are the remote herdr's own ids ("w5", "w5:p1"), the
	// handles `herdr-mirror remote-invoke` addresses over there.
	Workspace string
	Pane      string
	// Label is the workspace's label on the remote ("clem"), i.e. the mirror's
	// sidebar label minus its "<host>: " prefix — taken from the daemon's
	// lastRemoteLabel rather than by stripping a prefix we would have to guess.
	Label string
}

// mirrorSet is one host's mirror attribution, by local id. Panes and workspaces
// are both indexed because the daemon writes a workspace's entry before its
// panes': a pane in that window is still a mirror, it just has no remote pane id
// yet.
type mirrorSet struct {
	panes      map[string]mirrorRef
	workspaces map[string]mirrorRef
}

func (m mirrorSet) empty() bool { return len(m.panes) == 0 && len(m.workspaces) == 0 }

// lookup attributes a pane, preferring its own entry over its workspace's.
func (m mirrorSet) lookup(workspaceID, paneID string) (mirrorRef, bool) {
	if r, ok := m.panes[paneID]; ok {
		return r, true
	}
	if r, ok := m.workspaces[workspaceID]; ok {
		return r, true
	}
	return mirrorRef{}, false
}

// mirrorMapFile is the on-disk shape of <host>-map.json. Only the fields lasso
// reads are declared; the daemon writes several more (ratios, seq, prev ids).
type mirrorMapFile struct {
	Workspaces map[string]struct {
		LocalID         string `json:"localId"`
		LastRemoteLabel string `json:"lastRemoteLabel"`
	} `json:"workspaces"`
	Panes map[string]struct {
		LocalID string `json:"localId"`
	} `json:"panes"`
}

const (
	// How long a host's attribution is served before the maps are read again. A
	// miss costs a directory listing plus a read per host file — nothing locally,
	// an SFTP round-trip each over ssh — and the grid polls every couple of
	// seconds, so it must not be paid per poll. The cost of being stale is small
	// and self-healing: a mirror created within the window renders as an ordinary
	// local row until the next refresh, never as the wrong host.
	mirrorMapTTL = 10 * time.Second
	// A host with no mirror state at all is remembered longer — the answer is
	// "this machine does not run herdr-mirror", which does not change on the
	// timescale a grid poll cares about.
	mirrorMapNoneTTL = 2 * time.Minute
)

type mirrorCacheEntry struct {
	set mirrorSet
	at  time.Time
}

var mirrorCache = struct {
	sync.Mutex
	m map[string]*mirrorCacheEntry
}{m: map[string]*mirrorCacheEntry{}}

// hostMirrors returns the mirror attribution for the host b drives. panes is the
// listing about to be annotated: when none of them sits in the mirror sentinel
// directory this host is running no mirrors, and the maps are not read at all.
func hostMirrors(b Backend, panes []pane) mirrorSet {
	if !anyMirrorSentinel(panes) {
		return mirrorSet{}
	}
	key := b.Name()
	mirrorCache.Lock()
	if e := mirrorCache.m[key]; e != nil {
		ttl := mirrorMapTTL
		if e.set.empty() {
			ttl = mirrorMapNoneTTL
		}
		if time.Since(e.at) < ttl {
			set := e.set
			mirrorCache.Unlock()
			return set
		}
	}
	mirrorCache.Unlock()

	// Read outside the lock: two refreshes racing here merely do the work twice,
	// which is far cheaper than serializing every host's grid fetch behind one
	// slow SFTP listing.
	set := readMirrorMaps(b)
	mirrorCache.Lock()
	mirrorCache.m[key] = &mirrorCacheEntry{set: set, at: time.Now()}
	mirrorCache.Unlock()
	return set
}

// anyMirrorSentinel reports whether any pane is parked in herdr-mirror's
// streamer directory — the cheap "is it worth looking?" test.
func anyMirrorSentinel(panes []pane) bool {
	for _, p := range panes {
		if strings.HasSuffix(p.Cwd, mirrorSentinelCwd) ||
			strings.HasSuffix(p.ForegroundCwd, mirrorSentinelCwd) {
			return true
		}
	}
	return false
}

// readMirrorMaps parses every <host>-map.json in herdr-mirror's state directory.
// A host whose file is missing, unreadable or malformed contributes nothing
// rather than failing the whole attribution: one broken map must not make the
// other eight hosts' rows read as local.
func readMirrorMaps(b Backend) mirrorSet {
	home, err := b.HomeDir()
	if err != nil {
		return mirrorSet{}
	}
	dir := filepath.Join(home, mirrorStateDir)
	ents, err := b.ReadDir(dir)
	if err != nil {
		return mirrorSet{}
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.Dir {
			continue
		}
		if host, ok := strings.CutSuffix(e.Name, "-map.json"); ok && host != "" {
			names = append(names, e.Name)
		}
	}
	// Sorted so that the (pathological) case of two hosts claiming one local id
	// resolves the same way every poll instead of flickering between them.
	sort.Strings(names)

	set := mirrorSet{panes: map[string]mirrorRef{}, workspaces: map[string]mirrorRef{}}
	for _, name := range names {
		host := strings.TrimSuffix(name, "-map.json")
		raw, err := b.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var mf mirrorMapFile
		if json.Unmarshal(raw, &mf) != nil {
			continue
		}
		labels := map[string]string{}
		for remoteWS, w := range mf.Workspaces {
			labels[remoteWS] = w.LastRemoteLabel
			if w.LocalID == "" {
				continue
			}
			set.workspaces[w.LocalID] = mirrorRef{
				Host:      host,
				Workspace: remoteWS,
				Label:     w.LastRemoteLabel,
			}
		}
		for remotePane, p := range mf.Panes {
			if p.LocalID == "" {
				continue
			}
			remoteWS, _, _ := strings.Cut(remotePane, ":")
			set.panes[p.LocalID] = mirrorRef{
				Host:      host,
				Workspace: remoteWS,
				Pane:      remotePane,
				Label:     labels[remoteWS],
			}
		}
	}
	return set
}
