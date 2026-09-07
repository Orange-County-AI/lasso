package main

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Resolving the directory the file viewer should follow for a pane.
//
// A harness's working directory is not the pane's. Claude Code keeps its own
// cwd — it moves when the agent cds inside its Bash tool — while the claude
// process (and the pane's shell) stay in the dir claude was launched from. So a
// pane where `claude` was started in ~ reports ~ for the pane cwd even after
// the agent has been working in ~/projects/foo for an hour, and a viewer
// following the pane sits on the wrong tree.
//
// Claude records the live value in its session transcript: every entry carries
// the session's cwd at the time it was written. Luvus hands us the session id
// per pane (the snapshot's `agent_session`, also `session` on agent.get), so
// the last cwd in ~/.claude/projects/<slug>/<session>.jsonl is the agent's real
// working dir — and it is authoritative in a way no process inspection is.
//
// Process inspection is in fact no longer available at all: UHP's
// pane.processes reports executable NAMES and a root pid, never an argument
// vector and never a per-process cwd ("Full argument vectors are never exposed
// because they can contain prompts, credentials, or tokens"). So the transcript
// is the only source for an agent's own cwd, and a pane that is a window onto
// another machine is not detectable — see the note on activeCwd.
//
// Panes with no readable harness cwd (plain shells, other harnesses, an agent
// whose transcript we can't find) fall back to the pane's own cwd — see
// activeCwd.

const (
	// How long a resolved harness cwd is served without re-checking the
	// transcript. The hub refreshes on every luvus event, which bursts while an
	// agent streams output; this keeps a burst down to one stat.
	harnessCwdTTL = time.Second
	// How long a failed lookup is remembered before the project dirs are
	// scanned again (the scan is the expensive path, especially over SFTP).
	harnessCwdMissTTL = 30 * time.Second
	// Bytes read from the tail of a transcript. Entries are one JSON object per
	// line; 64K covers many, and survives a single large tool result.
	transcriptTailBytes = 64 << 10
)

type harnessCwdEntry struct {
	path string    // resolved transcript path; "" when nothing was found
	size int64     // transcript size at the last read, to skip re-reads
	cwd  string    // last cwd parsed out of it
	at   time.Time // when this entry was refreshed
}

var harnessCwdCache = struct {
	sync.Mutex
	m map[string]*harnessCwdEntry
}{m: map[string]*harnessCwdEntry{}}

// harnessCwd returns the working directory the pane's harness is itself using,
// or "" when there is none to read.
func harnessCwd(b Backend, p pane) string {
	id := claudeSessionID(p)
	if id == "" {
		return ""
	}
	key := b.Name() + "|" + id

	harnessCwdCache.Lock()
	e := harnessCwdCache.m[key]
	if e != nil {
		ttl := harnessCwdTTL
		if e.path == "" {
			ttl = harnessCwdMissTTL
		}
		if time.Since(e.at) < ttl {
			cwd := e.cwd
			harnessCwdCache.Unlock()
			return cwd
		}
	}
	known := harnessCwdEntry{}
	if e != nil {
		known = *e
	}
	harnessCwdCache.Unlock()

	// Resolve outside the lock: this is file I/O, and SFTP round-trips on a
	// remote host. Two refreshes racing here just do the work twice.
	cur := harnessCwdEntry{path: known.path, at: time.Now()}
	if cur.path == "" {
		cur.path = findClaudeTranscript(b, id, p)
	}
	if cur.path != "" {
		if fi, err := b.Stat(cur.path); err != nil || fi.IsDir() {
			// Gone (session ended, transcript moved). Re-scan next time.
			cur.path, cur.cwd = "", ""
		} else if cur.path == known.path && fi.Size() == known.size && known.cwd != "" {
			cur.size, cur.cwd = known.size, known.cwd // unchanged since the last read
		} else {
			cur.size = fi.Size()
			cur.cwd = lastTranscriptCwd(readTranscriptTail(b, cur.path, fi.Size()))
			if cur.cwd != "" && cur.cwd != known.cwd {
				// A cwd that no longer exists (deleted worktree) would strand the
				// viewer on a dead path; fall back to the pane's own cwd instead.
				if fi, err := b.Stat(cur.cwd); err != nil || !fi.IsDir() {
					cur.cwd = ""
				}
			}
		}
	}

	harnessCwdCache.Lock()
	harnessCwdCache.m[key] = &cur
	harnessCwdCache.Unlock()
	return cur.cwd
}

// claudeSessionID returns the pane's claude session id, or "" when the pane
// isn't currently running a claude session. Luvus reports the session as a bare
// identifier string and keeps it around after the agent exits so it can resume
// the pane, so this is gated on the pane's live agent label: once the shell is
// back in the foreground, the pane's own cwd is the truth again.
func claudeSessionID(p pane) string {
	if p.AgentSession == "" || !strings.EqualFold(p.Agent, "claude") {
		return ""
	}
	return safeSessionID(p.AgentSession)
}

// safeSessionID accepts only the [A-Za-z0-9_-] shape of a claude session id, so
// the value Luvus reports can never escape the projects dir once it is joined
// into a path.
func safeSessionID(v string) string {
	if v == "" || len(v) > 128 {
		return ""
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return ""
		}
	}
	return v
}

// findClaudeTranscript locates ~/.claude/projects/<slug>/<id>.jsonl. Claude
// names the project dir after the directory it was launched in, so the pane's
// cwd usually names it outright; when it doesn't — the agent was started with
// `cd x && claude`, or resumed from elsewhere — every project dir is probed for
// the id. "" when the transcript isn't there.
func findClaudeTranscript(b Backend, id string, p pane) string {
	home, err := b.HomeDir()
	if err != nil || home == "" {
		return ""
	}
	root := filepath.Join(home, ".claude", "projects")
	for _, dir := range []string{p.Cwd} {
		if dir == "" {
			continue
		}
		if path := filepath.Join(root, claudeProjectSlug(dir), id+".jsonl"); isFile(b, path) {
			return path
		}
	}
	ents, err := b.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if !e.Dir {
			continue
		}
		if path := filepath.Join(root, e.Name, id+".jsonl"); isFile(b, path) {
			return path
		}
	}
	return ""
}

func isFile(b Backend, path string) bool {
	fi, err := b.Stat(path)
	return err == nil && !fi.IsDir()
}

// claudeProjectSlug reproduces Claude Code's project-dir naming: the absolute
// path with every non-alphanumeric character replaced by '-', case preserved.
// /home/u/.lasso/x -> "-home-u--lasso-x".
func claudeProjectSlug(dir string) string {
	var sb strings.Builder
	sb.Grow(len(dir))
	for _, r := range dir {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	return sb.String()
}

// readTranscriptTail reads the last transcriptTailBytes of a transcript.
// Transcripts run to hundreds of megabytes, and only the newest entry matters.
func readTranscriptTail(b Backend, path string, size int64) []byte {
	f, err := b.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if off := size - transcriptTailBytes; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil
		}
	}
	// The file grows while we read it; cap the read so a busy session can't turn
	// this into an unbounded slurp.
	data, err := io.ReadAll(io.LimitReader(f, 2*transcriptTailBytes))
	if err != nil {
		return nil
	}
	return data
}

// lastTranscriptCwd returns the cwd of the newest transcript entry that carries
// one. The first line of a tail read is usually a fragment; it simply fails to
// parse, along with any other line that isn't a JSON object.
func lastTranscriptCwd(data []byte) string {
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if filepath.IsAbs(rec.Cwd) {
			return rec.Cwd
		}
	}
	return ""
}

// activeCwd resolves the directory the file viewer follows for the focused pane,
// and the host that directory lives on.
//
// Most-authoritative first: the harness's own cwd (which the pane never sees),
// then the pane's own cwd. Both come from be, since that is whose Luvus
// reported the pane. The second return is the Active.CwdSource label naming
// which resolver answered.
//
// The host is always be's. It used to be able to differ: a pane could be an
// `ssh <host> luvus agent attach …` window onto another machine, and lasso
// recovered that from the foreground argv luvus exposed. UHP exposes no
// argument vector at all (pane.processes reports executable names and a root
// pid, deliberately withholding argv), so a remote-attach pane is no longer
// distinguishable from a local shell — and guessing would repoint the viewer,
// and its editor's saves, at the wrong filesystem. A pane's work is therefore
// resolved on the host whose Luvus reported it, full stop.
func activeCwd(be Backend, p pane) (cwd, source, host string) {
	local := be.Name()
	if c := harnessCwd(be, p); c != "" {
		return c, "harness", local
	}
	return paneCwd(p), "shell", local
}
