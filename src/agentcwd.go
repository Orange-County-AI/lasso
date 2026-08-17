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
// pane where `claude` was started in ~ reports ~ for both of herdr's cwds even
// after the agent has been working in ~/projects/foo for an hour, and a viewer
// following the pane sits on the wrong tree.
//
// Claude records the live value in its session transcript: every entry carries
// the session's cwd at the time it was written. herdr hands us the session id
// per pane (pane.agent_session), so the last cwd in
// ~/.claude/projects/<slug>/<session>.jsonl is the agent's real working dir —
// and it is authoritative in a way no process inspection is.
//
// Panes with no readable harness cwd (plain shells, other harnesses, an agent
// whose transcript we can't find) fall back to the pane's own cwd — see
// activeCwd.

// agentSession mirrors herdr's pane.agent_session: the harness session herdr
// would resume this pane with. Kind is "id" (a session identifier) or "path" (a
// transcript file).
type agentSession struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

const (
	// How long a resolved harness cwd is served without re-checking the
	// transcript. The hub refreshes on every herdr event, which bursts while an
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
// or "" when there is none to read. launchHint is a directory the harness was
// plausibly launched from (the foreground leader's cwd — for claude that IS its
// launch dir), used to guess the transcript's project dir before falling back to
// scanning them all.
func harnessCwd(b Backend, p pane, launchHint string) string {
	id, path := claudeSessionRef(p)
	if id == "" && path == "" {
		return ""
	}
	key := b.Name() + "|" + id + "|" + path

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
		if path != "" {
			cur.path = path
		} else {
			cur.path = findClaudeTranscript(b, id, launchHint, p)
		}
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

// claudeSessionRef returns the pane's claude session as (id, transcript path) —
// exactly one is non-empty, both are "" when the pane isn't currently running a
// claude session. herdr keeps agent_session around after the agent exits so it
// can resume the pane, so this is gated on the pane's live agent label: once the
// shell is back in the foreground, the pane's own cwd is the truth again.
func claudeSessionRef(p pane) (id, path string) {
	s := p.AgentSession
	if s == nil || p.Agent == "" || !strings.EqualFold(s.Agent, "claude") {
		return "", ""
	}
	switch s.Kind {
	case "id":
		return safeSessionID(s.Value), ""
	case "path":
		if filepath.IsAbs(s.Value) && strings.HasSuffix(s.Value, ".jsonl") {
			return "", s.Value
		}
	}
	return "", ""
}

// safeSessionID accepts only the [A-Za-z0-9_-] shape of a claude session id, so
// the value herdr reports can never escape the projects dir once it is joined
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
// names the project dir after the directory it was launched in, so the launch
// hint (and the pane's cwds) usually name it outright; when they don't — the
// agent was started with `cd x && claude`, or resumed from elsewhere — every
// project dir is probed for the id. "" when the transcript isn't there.
func findClaudeTranscript(b Backend, id, launchHint string, p pane) string {
	home, err := b.HomeDir()
	if err != nil || home == "" {
		return ""
	}
	root := filepath.Join(home, ".claude", "projects")
	for _, dir := range []string{launchHint, p.Cwd, p.ForegroundCwd} {
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

// paneProcess is one foreground process of a pane, as herdr's
// pane.process_info reports it. Argv is what makes an ssh attach recoverable
// (see panehost.go); the rest resolves the pane's own cwd.
type paneProcess struct {
	PID  uint32   `json:"pid"`
	Name string   `json:"name"`
	Cwd  string   `json:"cwd"`
	Argv []string `json:"argv"`
}

type paneProcessInfo struct {
	ForegroundProcessGroupID uint32        `json:"foreground_process_group_id"`
	ForegroundProcesses      []paneProcess `json:"foreground_processes"`
}

// paneForeground asks herdr what the pane is running. One call answers both
// questions activeCwd has — where the foreground leader sits, and whether the
// pane is really a window onto another host — so neither pays its own RPC. The
// zero value is returned when herdr predates pane.process_info or the pane is
// gone; every consumer reads that as "no answer".
func paneForeground(paneID string) paneProcessInfo {
	if paneID == "" {
		return paneProcessInfo{}
	}
	res, err := herdrCall("pane.process_info", map[string]any{"pane_id": paneID})
	if err != nil {
		return paneProcessInfo{}
	}
	return parsePaneProcessInfo(res)
}

func parsePaneProcessInfo(res json.RawMessage) paneProcessInfo {
	var r struct {
		ProcessInfo paneProcessInfo `json:"process_info"`
	}
	if json.Unmarshal(res, &r) != nil {
		return paneProcessInfo{}
	}
	return r.ProcessInfo
}

// leaderCwd is the cwd of the pane's foreground process-group LEADER: the shell
// when the pane is idle, the harness while it runs. herdr's foreground_cwd
// deliberately prefers a *descendant* whose cwd differs from the shell's, which
// under an agent is whatever transient subprocess is running (a plugin under
// ~/.claude/plugins/cache, a git hook) — enough to yank the file viewer off the
// tree. Asking for the leader specifically keeps the answer stable while still
// tracking a `cd repo && claude` that herdr's shell-reported cwd never sees.
// "" when there is no foreground job or the cwd is unreadable.
func leaderCwd(pi paneProcessInfo) string {
	if pi.ForegroundProcessGroupID == 0 {
		return ""
	}
	for _, proc := range pi.ForegroundProcesses {
		if proc.PID == pi.ForegroundProcessGroupID && filepath.IsAbs(proc.Cwd) {
			return proc.Cwd
		}
	}
	return ""
}

// activeCwd resolves the directory the file viewer follows for the focused pane,
// and the host that directory lives on.
//
// The ssh hop comes first: when the pane is an attach onto another host's herdr,
// every local answer below is the ssh client's directory and none of them
// describes what is on screen. Otherwise, most-authoritative first: the
// harness's own cwd (which the pane never sees), then the pane's foreground
// leader, then herdr's pane cwds — all on the active host, since that is whose
// herdr reported the pane. The second return is the Active.CwdSource label
// naming which resolver answered, prefixed "ssh:" when it answered on the far
// side of an attach.
func activeCwd(p pane) (cwd, source, host string) {
	pi := paneForeground(p.PaneID)
	if hop, ok := paneSSHHop(pi); ok {
		if c, src := remoteAttachCwd(hop); c != "" {
			return c, "ssh:" + src, hop.host
		}
	}
	local := curBackend().Name()
	leader := leaderCwd(pi)
	if c := harnessCwd(curBackend(), p, leader); c != "" {
		return c, "harness", local
	}
	if leader != "" {
		return leader, "leader", local
	}
	if paneCwdUsesForeground(p) {
		return paneCwd(p), "foreground", local
	}
	return paneCwd(p), "shell", local
}
