package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// liveSeen records which tab sessions we've observed alive in THIS lasso process.
// It's how the exit watcher distinguishes a shell the user *exited* (was alive,
// now gone → close the tab) from a session that's gone because the machine
// rebooted (never alive this process → restore it as a fresh shell on attach).
var liveSeen = struct {
	mu sync.Mutex
	m  map[string]bool
}{m: map[string]bool{}}

func markSeen(tabID string) { liveSeen.mu.Lock(); liveSeen.m[tabID] = true; liveSeen.mu.Unlock() }
func unsee(tabID string)    { liveSeen.mu.Lock(); delete(liveSeen.m, tabID); liveSeen.mu.Unlock() }
func wasSeen(tabID string) bool {
	liveSeen.mu.Lock()
	defer liveSeen.mu.Unlock()
	return liveSeen.m[tabID]
}

// Startup reconciliation between the saved workspace/tab tree (SQLite) and the
// live tmux sessions on lasso's server. Across a lasso restart/upgrade the tmux
// server keeps running, so a tab's session is still live and just gets
// reattached. Across a full reboot the tmux server is gone; we restore the tree
// from the DB and recreate FRESH SHELLS in each tab's saved cwd on first attach —
// we never auto-relaunch agents (the user restarts them).

// reconcileTabs runs once at boot. It retires tabs whose working directory is
// gone (a deleted worktree) and kills orphan lasso_* sessions that no live tab
// claims (crash leftovers). It does NOT pre-create sessions — that's lazy, via
// ensureTabSession on first attach, so boot doesn't spawn dozens of shells.
func reconcileTabs() {
	tabs, err := allLiveTabs()
	if err != nil {
		return
	}
	want := map[string]bool{}
	for _, t := range tabs {
		// Remote tabs live on another host's tmux server; the local reconcile
		// neither stats their cwd nor manages their sessions. Skip them.
		if tabHost(t.ID) != "" {
			continue
		}
		session := tabSession(t.ID)
		// Retire a tab whose directory no longer exists (e.g. the worktree was
		// removed) so it doesn't linger in the sidebar pointing at nothing.
		if t.Cwd != "" {
			if _, err := os.Stat(t.Cwd); os.IsNotExist(err) {
				_ = closeTab(t.ID)
				_ = tmuxKillSession(session)
				continue
			}
		}
		want[session] = true
	}
	for _, s := range tmuxListSessions() {
		if strings.HasPrefix(s, "lasso_") && !want[s] {
			_ = tmuxKillSession(s)
			continue
		}
		// Reap a park session left behind by a crashed lasso instance (named
		// lassopark_<pid>), but never another LIVE instance's park — only kill it
		// if its pid is gone. Our own park is kept (we're alive).
		if pid, ok := strings.CutPrefix(s, "lassopark_"); ok {
			if n, err := strconv.Atoi(pid); err == nil && !processAlive(n) {
				_ = tmuxKillSession(s)
			}
		}
	}
}

// processAlive reports whether a pid is a live process (signal 0 probe).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// ensureTabSession returns a tab's tmux session, creating a fresh shell in the
// tab's saved cwd if the session isn't live (after a reboot the tmux server is
// gone). Agents are NOT relaunched — a recreated tab is a bare shell. Called by
// the viewport attach path so sessions come back lazily on first view. The
// boolean reports whether this call CREATED the session (a brand-new shell that
// needs its prompt primed) versus reusing a live one.
func ensureTabSession(tabID string) (string, bool, error) {
	session := tabSession(tabID)
	// Re-assert the session→host mapping from the DB: a lasso restart clears the
	// in-memory sessionHosts map, so without this a remote session would resolve
	// to the local server and look dead. (No-op for local tabs.)
	host := tabHost(tabID)
	setSessionHost(session, host)

	if tmuxHasSession(session) {
		markSeen(tabID)
		return session, false, nil
	}
	// Session gone. If we'd seen it alive this process, the user exited the shell
	// — don't resurrect it (the exit watcher is closing the tab); a stale
	// re-attach here is exactly the "flash a fresh shell" we want to avoid.
	if wasSeen(tabID) {
		return "", false, fmt.Errorf("tab %s exited", tabID)
	}
	tab, err := getTab(tabID)
	if err != nil {
		return "", false, err
	}
	if !tab.ClosedAt.IsZero() {
		return "", false, fmt.Errorf("tab %s is closed", tabID)
	}
	// Reboot recovery (recreate a fresh shell) is LOCAL-only — we can't cheaply
	// probe a remote host's filesystem here, and remote sessions live on the
	// remote tmux server independent of lasso. A missing remote session is an
	// error (the remote host or its tmux server is down).
	if host != "" {
		return "", false, fmt.Errorf("tab %s not live on host %s", tabID, host)
	}
	cwd := tab.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	} else if _, err := os.Stat(cwd); err != nil {
		// Saved dir is gone — fall back to home so new-session doesn't fail.
		cwd, _ = os.UserHomeDir()
	}
	if err := tmuxNewSession(session, cwd, []string{"LASSO_TAB_ID=" + tabID}); err != nil {
		return "", false, err
	}
	// A reboot-recreated tab is a fresh bare shell — prime its prompt too (agents
	// aren't relaunched, so a kind=="agent" tab is also just a shell here).
	markPrimePending(session)
	markSeen(tabID)
	return session, true, nil
}

// tabHost returns the host a tab lives on ("" = local), resolved via its
// workspace's host column.
func tabHost(tabID string) string {
	t, err := getTab(tabID)
	if err != nil {
		return ""
	}
	ws, err := getWorkspace(t.WorkspaceID)
	if err != nil || isLocalHost(ws.Host) {
		return ""
	}
	return ws.Host
}

// tabExitWatcher closes a tab when its shell exits — the way the user closes a
// workspace from the terminal (typing `exit` / Ctrl-D). A tab's tmux session,
// once seen alive this process, vanishing means the shell exited; we close the
// tab, and if it was the workspace's last live tab, the workspace too. Sessions
// gone because of a reboot were never seen alive, so they're left for
// ensureTabSession to restore instead. (Manual UI close routes through
// closeOneTab, which unsees the tab so this watcher ignores it.)
//
// REMOTE tabs are watched the same way, through their host's session listing
// (one mux'd ssh per host per tick), with two safeguards: only an AUTHORITATIVE
// answer counts (an SSH failure is not evidence the shell exited — see
// tmuxListSessionsOnChecked), and only hosts with an already-pooled connection
// are asked, so this loop never re-dials a down host once a second. A host
// whose connection was reaped gets its stale tabs swept once the next contact
// (host switch / grid poll) re-pools it.
func tabExitWatcher(ctx context.Context, h *hub) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tabs, err := allLiveTabs()
			if err != nil {
				continue
			}
			// One session listing per host per tick, fetched lazily so hosts
			// with no live tabs are never contacted.
			type hostLive struct {
				live map[string]bool
				ok   bool // authoritative — false means "couldn't ask"
			}
			byHost := map[string]*hostLive{}
			sessionsOn := func(host string) *hostLive {
				if hl, done := byHost[host]; done {
					return hl
				}
				hl := &hostLive{live: map[string]bool{}}
				var names []string
				switch {
				case host == "":
					names, hl.ok = tmuxListSessions(), true
				case hostPooled(host):
					names, hl.ok = tmuxListSessionsOnChecked(host)
				}
				for _, s := range names {
					hl.live[s] = true
				}
				byHost[host] = hl
				return hl
			}
			changed := false
			for _, tab := range tabs {
				hl := sessionsOn(tabHost(tab.ID))
				if !hl.ok {
					continue // host unreachable — not evidence the shell exited
				}
				if hl.live[tabSession(tab.ID)] {
					markSeen(tab.ID)
					continue
				}
				if wasSeen(tab.ID) {
					unsee(tab.ID)
					closeExitedTab(tab)
					changed = true
				}
			}
			if changed && h != nil {
				h.kick()
			}
		}
	}
}

// closeExitedTab tears down a tab whose shell exited, plus its workspace if that
// was the last live tab in it.
func closeExitedTab(tab Tab) {
	closeOneTab(tab.ID)
	if rest, _ := listTabs(tab.WorkspaceID); len(rest) == 0 {
		_ = closeWorkspace(tab.WorkspaceID)
	}
}

// sessionClosedFIFO is where tmux's session-closed hook writes each ended
// session's name, so we close its tab the INSTANT the shell exits instead of up
// to a poll-tick later — that lag is what let the ttyd client flash
// "can't find session / Reconnecting…" against the dead session. tabExitWatcher
// stays as a backstop if the hook/FIFO is unavailable.
func sessionClosedFIFO() string { return filepath.Join(lassoDir(), "sessions.closed") }

// startSessionCloseListener creates the FIFO and drains it, closing each lasso
// tab whose session just ended. Best-effort: on any setup error we silently rely
// on tabExitWatcher.
func startSessionCloseListener(ctx context.Context, h *hub) {
	path := sessionClosedFIFO()
	_ = os.Remove(path)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return
	}
	// O_RDWR keeps a writer end open so reads block (rather than hit EOF) between
	// hook writes, and the reader survives idle periods.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		_ = os.Remove(path)
		return
	}
	go func() {
		<-ctx.Done()
		_ = f.Close()
		_ = os.Remove(path)
	}()
	go func() {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			name := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(name, "lasso_") {
				continue
			}
			tabID := strings.TrimPrefix(name, "lasso_")
			// Only react to a session that was alive (a user exit). Deliberate
			// closes unsee the tab first (closeOneTab), so they're skipped here —
			// same rule as the watcher.
			if !wasSeen(tabID) {
				continue
			}
			unsee(tabID)
			if tab, err := getTab(tabID); err == nil {
				closeExitedTab(tab)
			}
			if h != nil {
				h.kick()
			}
		}
	}()
}

// cwdSaver periodically persists each live tab's current working directory, so a
// post-reboot recreated shell lands where the user actually was, not the original
// launch dir. Only writes when the cwd changed, to keep DB churn low.
func cwdSaver(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tabs, err := allLiveTabs()
			if err != nil {
				continue
			}
			for _, tab := range tabs {
				if tabHost(tab.ID) != "" {
					continue // remote tab cwd is saved by its own host's lasso path
				}
				session := tabSession(tab.ID)
				if !tmuxHasSession(session) {
					continue
				}
				cur, err := tmuxCurrentPath(session)
				if err != nil || cur == "" || cur == tab.Cwd {
					continue
				}
				_ = setTabCwd(tab.ID, cur)
			}
		}
	}
}
