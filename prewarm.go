package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"time"
)

// Pre-warmed shell pool. Creating a terminal otherwise pays the user's full shell
// rc boot (~5s of mise/fnox/starship) on the click. Instead we keep a few tmux
// sessions running the default shell, boot their rc in the BACKGROUND, and on a
// new tab CLAIM a booted one — rename it to the tab's session and `cd` it into the
// workspace dir. That's near-instant; the prompt then paints the moment the
// viewport attaches (markPrimePending), with no rc wait.
//
// Purely an optimization: if no warm shell is ready, callers fall back to a cold
// tmuxNewSession (startTabShell), so the pool can never make terminal creation
// fail — worst case is the old ~5s behavior.

const (
	warmPrefix   = "lasso_warm_" // distinct from "lasso_<tab>" so reconcile/agents ignore it
	warmPoolSize = 2
	warmMinAge   = 3 * time.Second // floor before a shell can be considered booted
)

type warmShell struct {
	born    time.Time
	lastCap string
	stable  int  // consecutive ticks the pane was idle-shell + unchanged
	ready   bool // rc finished — safe to claim (a `cd` won't be eaten by rc)
}

var warmPool struct {
	mu       sync.Mutex
	sessions map[string]*warmShell
	started  bool
}

func isWarmSession(s string) bool { return strings.HasPrefix(s, warmPrefix) }

// startWarmPool launches the maintainer goroutine (idempotent).
func startWarmPool() {
	warmPool.mu.Lock()
	if warmPool.started {
		warmPool.mu.Unlock()
		return
	}
	warmPool.started = true
	warmPool.sessions = map[string]*warmShell{}
	warmPool.mu.Unlock()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		refillWarmPool()
		for {
			select {
			case <-srvCtx.Done():
				return
			case <-t.C:
				refillWarmPool()
			}
		}
	}()
}

// refillWarmPool prunes dead sessions, advances readiness (rc-done detection), and
// spawns new warm shells up to the pool size.
func refillWarmPool() {
	live := map[string]bool{}
	for _, s := range tmuxListSessions() {
		if isWarmSession(s) {
			live[s] = true
		}
	}

	warmPool.mu.Lock()
	for s := range warmPool.sessions {
		if !live[s] {
			delete(warmPool.sessions, s)
		}
	}
	// Snapshot the not-yet-ready sessions to probe outside the lock.
	var probing []string
	for s, w := range warmPool.sessions {
		if !w.ready {
			probing = append(probing, s)
		}
	}
	need := warmPoolSize - len(warmPool.sessions)
	warmPool.mu.Unlock()

	// rc-done detection: a warm shell is ready once it's old enough, its foreground
	// is an idle shell (no rc child running), and its screen has stopped changing
	// for two ticks. That ensures a `cd` we send on claim lands at the interactive
	// prompt instead of being flushed as typeahead mid-rc.
	for _, s := range probing {
		warmPool.mu.Lock()
		w := warmPool.sessions[s]
		warmPool.mu.Unlock()
		if w == nil || time.Since(w.born) < warmMinAge || !foregroundIsShell(s) {
			if w != nil {
				w.stable = 0
			}
			continue
		}
		cap, _ := tmuxCapture(s)
		warmPool.mu.Lock()
		if w := warmPool.sessions[s]; w != nil {
			if cap == w.lastCap {
				w.stable++
			} else {
				w.lastCap, w.stable = cap, 0
			}
			if w.stable >= 2 {
				w.ready = true
			}
		}
		warmPool.mu.Unlock()
	}

	home, _ := os.UserHomeDir()
	for i := 0; i < need; i++ {
		var tok [6]byte
		if _, err := rand.Read(tok[:]); err != nil {
			return
		}
		s := warmPrefix + hex.EncodeToString(tok[:])
		if err := tmuxNewSession(s, home, nil); err != nil {
			return
		}
		warmPool.mu.Lock()
		warmPool.sessions[s] = &warmShell{born: time.Now()}
		warmPool.mu.Unlock()
	}
}

// claimWarmInto renames a booted warm shell to target and moves it into workDir,
// returning false if none are ready. The shell is already rc-booted, so the `cd`
// runs instantly; the prompt paints when the viewport attaches (markPrimePending).
func claimWarmInto(target, workDir string) bool {
	warmPool.mu.Lock()
	pick := ""
	for s, w := range warmPool.sessions {
		if w.ready {
			pick = s
			break
		}
	}
	if pick != "" {
		delete(warmPool.sessions, pick)
	}
	warmPool.mu.Unlock()
	if pick == "" {
		return false
	}
	if !tmuxHasSession(pick) || tmux("rename-session", "-t", pick, target) != nil {
		return false // raced a death/rename — caller falls back to a cold session
	}
	if workDir != "" {
		// Booted shell at $HOME → move to the workspace dir and wipe the echo, so
		// the primed prompt lands cleanly at the right cwd.
		_ = tmuxSendLine(target, "cd "+shellSingleQuote(workDir)+" && clear")
	}
	markPrimePending(target)
	go refillWarmPool() // top the pool back up
	return true
}

// startTabShell sets up a new shell tab's tmux session — instantly from the warm
// pool when possible, otherwise a cold session (paying the rc boot). The single
// entry point every shell-creating handler uses.
func startTabShell(tabID, workDir string) error {
	target := tabSession(tabID)
	if claimWarmInto(target, workDir) {
		return nil
	}
	return tmuxNewSession(target, workDir, []string{"LASSO_TAB_ID=" + tabID})
}

// shellSingleQuote wraps s in single quotes safe for a POSIX shell command line.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
