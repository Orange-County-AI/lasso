package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// tmux is lasso's terminal/persistence layer (it replaced the herdr daemon).
// Every terminal — an agent's shell, a plain shell tab — is a tmux *session* on
// a DEDICATED tmux server so lasso's sessions are isolated from the user's own
// tmux and survive lasso restarts/version updates (the tmux server is a separate
// long-lived process; killing lasso only detaches viewers). ttyd attaches a
// browser terminal to a session with `tmux attach -t <session>`.
//
// CRITICAL: every tmux invocation MUST carry -S (our private socket) and
// -f /dev/null (ignore the user's ~/.tmux.conf, whose prefix rebinds / plugins /
// `destroy-unattached on` would corrupt lasso sessions). A single invocation
// without -S would hit the user's real tmux server — destructive. So ALL tmux
// calls funnel through tmux()/tmuxOut()/tmuxIn() below, which prepend the prefix;
// no other code in lasso should exec tmux directly.

// tabSession is the tmux session name for a tab id. Tab ids are base36
// (strconv.FormatInt(UnixNano, 36)), which contain no '.'/':' — both illegal in
// tmux session names — so they're safe verbatim.
func tabSession(tabID string) string { return "lasso_" + tabID }

// tmuxPrefix is the leading argv every tmux call carries: our private socket and
// "no user config". Built fresh per call so the socket path is always current.
func tmuxPrefix() []string {
	return []string{"-S", lassoTmuxSock(), "-f", "/dev/null"}
}

// tmux runs a tmux command on our server, discarding stdout (returns any error,
// with stderr folded into the message like gitOutLocal does).
func tmux(args ...string) error {
	_, err := tmuxOut(args...)
	return err
}

// tmuxOut runs a tmux command on our server and returns its stdout.
func tmuxOut(args ...string) (string, error) {
	return tmuxIn("", args...)
}

// tmuxIn runs a tmux command on our server with stdin wired to in (used by
// load-buffer for the bracketed-paste path). Empty in means no stdin.
func tmuxIn(in string, args ...string) (string, error) {
	cmd := exec.Command("tmux", append(tmuxPrefix(), args...)...)
	if in != "" {
		cmd.Stdin = strings.NewReader(in)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.String(), fmt.Errorf("tmux %s: %s", args[0], msg)
		}
		return stdout.String(), fmt.Errorf("tmux %s: %w", args[0], err)
	}
	return stdout.String(), nil
}

// tmuxEnsureServer starts our tmux server (idempotent) and pins the server-wide
// options lasso relies on. destroy-unattached MUST be off before any session
// goes unattached, or a detached (background) session would vanish — so this is
// called once at startup before any new-session, and defensively by
// tmuxNewSession. start-server on a running server is a no-op.
func tmuxEnsureServer() error {
	// One round-trip: start the server, then set options. The `;` separators are
	// their own argv entries (tmux command sequence), not shell tokens.
	return tmux(
		"start-server", ";",
		"set", "-g", "destroy-unattached", "off", ";",
		"set", "-g", "status", "off", ";",
		"set", "-g", "history-limit", "50000", ";",
		"set", "-g", "default-terminal", "tmux-256color", ";",
		"setw", "-g", "aggressive-resize", "on", ";",
		// Windows size to the LARGEST attached client, so the active viewer fills
		// the pane (no dead "·" filler columns). `largest` over `latest` because a
		// stale/orphaned client left behind by a hard backend restart (e.g. a ttyd
		// orphan stuck at the default 80x24, or a degenerate 2x1) would otherwise
		// clamp the window under `latest`; being small, it's ignored under
		// `largest`. Set explicitly because nudgeRedraw's resize-window flips the
		// per-window option to manual and must restore it.
		"setw", "-g", "window-size", "largest", ";",
		// Notify lasso the instant a session ends (the user exited the shell) so
		// its tab closes immediately, before the ttyd client flashes a reconnect
		// against the dead session. See startSessionCloseListener.
		"set-hook", "-g", "session-closed",
		`run-shell "echo #{hook_session_name} >> `+sessionClosedFIFO()+`"`,
	)
}

// tmuxNewSession creates a detached session named session, rooted at cwd, with
// each "KEY=VAL" in env exported into the session (tmux >=3.2 `new-session -e`).
// We always tag the session with LASSO_TAB_ID so an agent running inside can
// identify which tab/agent it is (MCP whoami) — there is no $HERDR_PANE_ID now.
// Initial geometry is generous; ttyd resizes the pane to the real viewport on
// attach (aggressive-resize sizes per attached client).
func tmuxNewSession(session, cwd string, env []string) error {
	if err := tmuxEnsureServer(); err != nil {
		return err
	}
	args := []string{"new-session", "-d", "-s", session, "-x", "200", "-y", "50"}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	return tmux(args...)
}

// tmuxHasSession reports whether session exists on our server.
func tmuxHasSession(session string) bool {
	return tmux("has-session", "-t", session) == nil
}

// tmuxListSessions returns the names of all sessions on our server (empty when
// the server isn't running / has none).
func tmuxListSessions() []string {
	out, err := tmuxOut("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names
}

// tmuxKillSession terminates a session (and the processes inside it). Used when
// a tab/workspace is closed.
func tmuxKillSession(session string) error {
	return tmux("kill-session", "-t", session)
}

// tmuxCapture returns the visible screen of a session's active pane, the input
// to the agent-status heuristics (detect.go) and the composer/trust checks.
func tmuxCapture(session string) (string, error) {
	return tmuxOut("capture-pane", "-p", "-t", session)
}

// tmuxCaptureScroll returns the last n lines of scrollback + screen (n>0), for
// the "recent output" MCP read. n is clamped to a sane ceiling by the caller.
func tmuxCaptureScroll(session string, n int) (string, error) {
	return tmuxOut("capture-pane", "-p", "-S", fmt.Sprintf("-%d", n), "-t", session)
}

// tmuxCurrentPath returns the live cwd of a session's foreground process — the
// foreground_cwd herdr used to surface (drives the file viewer + the cwd we save
// to recreate a shell after a reboot).
func tmuxCurrentPath(session string) (string, error) {
	out, err := tmuxOut("display-message", "-p", "-t", session, "#{pane_current_path}")
	return strings.TrimSpace(out), err
}

// nudgeRedraw forces whatever is running in a session to repaint. A session is
// created detached at a fixed size, then a differently-sized ttyd client attaches
// — the resize delivers a SIGWINCH mid-startup that eats bash's first prompt (or
// an agent TUI's first frame), leaving the pane blank until the user types. We
// replay that SIGWINCH deliberately: a one-row resize, then restore automatic
// (client-driven) sizing, makes both shells (readline redraws on SIGWINCH) and
// TUIs repaint.
//
// CRITICAL: resize-window flips the window's window-size option to *manual* as a
// side effect, which would freeze the geometry so a later client widen leaves
// dead "·" filler columns. The trailing `setw window-size largest` both restores
// automatic sizing (so future resizes follow the client) AND resizes back to the
// current client now — a second SIGWINCH, another harmless repaint. (`-A` only
// resizes once; it does NOT restore automatic mode.)
func nudgeRedraw(session string) {
	wh, err := tmuxOut("display-message", "-p", "-t", session, "#{window_width} #{window_height}")
	if err != nil {
		return
	}
	parts := strings.Fields(wh)
	if len(parts) != 2 {
		return
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	if w <= 0 || h <= 1 {
		return
	}
	_ = tmux("resize-window", "-t", session, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h-1))
	_ = tmux("setw", "-t", session, "window-size", "largest") // repaint + restore auto-sizing
}

// nudgeRedrawWhenAttached waits for a client to attach, then forces a full
// redraw a handful of times on a schedule. We can't pick one delay: the frame
// that needs re-pushing might be a shell prompt after a slow mise/starship rc,
// OR an agent TUI that only finishes booting seconds later (and whose shell
// prompt drew first, so cursor-position can't tell us "done"). Re-drawing
// already-correct content is invisible, so over-nudging is harmless — we just
// need to land at least one redraw after the program's real frame appears.
// Best-effort; run in a goroutine after spawning the ttyd.
func nudgeRedrawWhenAttached(session string) {
	if !waitAttached(session) {
		return
	}
	// Deltas between nudges → fires at ~0.4s, 1.2s, 2.4s, 4.4s, 7.4s post-attach.
	for _, d := range []int{400, 800, 1200, 2000, 3000} {
		time.Sleep(time.Duration(d) * time.Millisecond)
		if !tmuxHasSession(session) {
			return
		}
		nudgeRedraw(session)
	}
}

// waitAttached blocks until a client (ttyd) attaches to session — returns true —
// or a ~10s timeout / the session vanishing (false). Both the redraw nudge and
// the shell-prompt prime need a real attached client first: tmux only answers a
// program's terminal queries (cursor-position DSR, etc.) when a client is there
// to answer for, and SIGWINCH/repaint only matter once someone's watching.
func waitAttached(session string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n, err := tmuxOut("display-message", "-p", "-t", session, "#{session_attached}")
		if err != nil {
			return false // session gone
		}
		if s := strings.TrimSpace(n); s != "" && s != "0" {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// primeShellPromptWhenAttached makes a SHELL session paint its prompt. Some
// prompt frameworks (notably starship on bash) don't draw the first prompt until
// the shell both (a) has a client attached to answer the terminal queries
// starship makes while rendering, and (b) processes an input event — readline
// computes the prompt and sits idle in select() without painting it, so the pane
// stays blank until the user types.
//
// We prime with a single Enter (an empty command — harmless) rather than a
// resize: a resize only repaints starship's LAST line, leaving an orphan `❯`
// above the real prompt the Enter then draws in full (the "double prompt"). One
// line-accept on a still-blank shell draws the FULL multi-line prompt cleanly and
// alone. We re-check before each Enter and stop the moment the prompt appears, so
// a shell that's already painted (or that the user has started typing in) is
// never touched.
//
// SHELL-ONLY: never call this on an agent session — the Enter could submit a
// half-typed agent command.
func primeShellPromptWhenAttached(session string) {
	if !waitAttached(session) {
		return
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if !tmuxHasSession(session) {
			return
		}
		if out, _ := tmuxCapture(session); strings.TrimSpace(out) != "" {
			return // prompt is up — nothing to prime
		}
		_ = tmuxSendEnter(session)
		time.Sleep(300 * time.Millisecond)
	}
}

// --- the persistent viewport: one ttyd, switched between sessions -------------
//
// Instead of spawning a ttyd + `tmux attach` per tab (paying the slow browser
// xterm⇄ttyd attach handshake every time a tab is first viewed), lasso keeps ONE
// long-lived ttyd whose tmux client we re-point at the selected tab's session
// with `switch-client`. The browser connects once; thereafter a tab switch is
// just tmux repainting the already-warm client — instant, no reconnect. See
// tabterm.go.
//
// The client always has somewhere to live: a hidden per-instance "park" session
// that always exists, so killing a tab's session never detaches (and kills) the
// viewport — tmux falls the client back to a still-living session instead.

// tmuxParkSession is this lasso process's park session. It's keyed by PID (not a
// fixed name) so two lasso instances sharing the tmux server (e.g. a dev build
// and the prod daemon both on ~/.lasso/tmux.sock) get DISTINCT parks: a fresh or
// reconnected browser client always lands on its OWN instance's park, which is
// how each instance scopes "its" client(s) for switch-client (see
// tmuxAdoptableClients). The name avoids the "lasso_" tab prefix so reconcile's
// orphan sweep never touches it.
func tmuxParkSession() string { return fmt.Sprintf("lassopark_%d", os.Getpid()) }

// tmuxEnsurePark creates this instance's park session (idempotent). A plain shell
// in $HOME; it's never shown except for the sub-second flash before the first
// switch-client, and never enumerated as a tab.
func tmuxEnsurePark() error {
	park := tmuxParkSession()
	if tmuxHasSession(park) {
		return nil
	}
	home, _ := os.UserHomeDir()
	return tmuxNewSession(park, home, nil)
}

// tmuxClientSessions maps every attached client's tty → the session it's
// currently viewing. The viewport watcher uses this to (a) discover clients that
// (re)connected onto our park and (b) tell which of our clients aren't yet on the
// wanted session — switching only those, so a steady viewport never re-repaints.
func tmuxClientSessions() map[string]string {
	out, err := tmuxOut("list-clients", "-F", "#{client_tty}\t#{client_session}")
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		tty, sess, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && tty != "" {
			m[tty] = sess
		}
	}
	return m
}

// tmuxSwitchClient points the client at tty to view session, without tearing down
// its connection — the heart of the warm-viewport model.
func tmuxSwitchClient(tty, session string) error {
	return tmux("switch-client", "-c", tty, "-t", session)
}

// tmuxSendLine types one command line into a cooked-mode shell (text, then a
// SEPARATE Enter keypress) — the equivalent of herdr's `pane run`. The literal
// flag (-l) and "--" stop tmux from interpreting text that looks like a key name
// ("Enter", "C-c") or starts with "-".
func tmuxSendLine(session, line string) error {
	if err := tmux("send-keys", "-t", session, "-l", "--", line); err != nil {
		return err
	}
	return tmuxSendEnter(session)
}

// tmuxSendEnter sends a real Enter keypress (distinct from a pasted "\n"). For an
// interactive agent TUI this is what actually submits a turn — see tmuxSubmit.
func tmuxSendEnter(session string) error {
	return tmux("send-keys", "-t", session, "Enter")
}

// tmuxSendCtrlC sends Ctrl-C (interrupt) — used to stop a running agent.
func tmuxSendCtrlC(session string) error {
	return tmux("send-keys", "-t", session, "C-c")
}

// tmuxSendText pastes text into a session as a BRACKETED PASTE (no trailing
// Enter). Going through load-buffer + `paste-buffer -p` makes the TUI treat it as
// a paste, so an embedded newline stays literal instead of submitting and
// per-character autocomplete doesn't fire — the tmux-native form of the lesson
// baked into the old herdr paneSubmit. The buffer is named per-call and deleted
// after paste (-d) so concurrent sends don't clobber each other.
func tmuxSendText(session, text string) error {
	buf := "lasso_" + randSuffix()
	if _, err := tmuxIn(text, "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	return tmux("paste-buffer", "-p", "-d", "-b", buf, "-t", session)
}

// tmuxWaitReady blocks until a fresh session's shell stops changing its visible
// output (it finished sourcing rc — mise/fnox/etc.) or a timeout elapses, so the
// command we send next isn't raced by shell startup and lose its leading bytes.
// Prompt-agnostic: watches for the screen to stabilize, mirroring the old
// waitPaneReady.
func tmuxWaitReady(session string) {
	deadline := time.Now().Add(10 * time.Second)
	var prev string
	stable := 0
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		out, err := tmuxCapture(session)
		if err != nil {
			continue
		}
		t := strings.TrimRight(out, " \t\n")
		if t != "" && t == prev {
			if stable++; stable >= 2 { // ~600ms unchanged → settled
				return
			}
		} else {
			stable = 0
		}
		prev = t
	}
}

// tmuxSubmit types text into an interactive agent TUI (claude/codex) and submits
// it as a turn. Two hazards, both handled (the hard-won herdr paneSubmit lesson,
// re-expressed on tmux):
//
//  1. Bracketed paste vs Enter. The TUIs run in raw mode with bracketed paste, so
//     a newline appended to the message pastes literally and never submits. The
//     Enter must be a separate real keypress (tmuxSendEnter), not part of the
//     paste — hence tmuxSendText (paste) then tmuxSendEnter.
//
//  2. A race between the paste committing and the Enter. The paste and the Enter
//     are separate writes; a busy TUI applies the paste a beat late, so an Enter
//     sent immediately hits a still-empty composer and is a no-op — the message
//     then sits unsubmitted. So: paste, wait until the composer actually shows it,
//     then send Enter, re-sending until the composer is observed empty (the turn
//     went through). A repeat Enter is harmless on an empty/submitted composer.
func tmuxSubmit(session, text string) {
	_ = tmuxSendText(session, text)
	// Wait for the paste to land before pressing Enter; if we never see it (read
	// failures, an unfamiliar composer) fall through and try Enter anyway rather
	// than dropping the turn.
	commit := time.Now().Add(3 * time.Second)
	for time.Now().Before(commit) {
		time.Sleep(150 * time.Millisecond)
		if !tmuxComposerEmpty(session) {
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = tmuxSendEnter(session)
		time.Sleep(300 * time.Millisecond)
		if tmuxComposerEmpty(session) || time.Now().After(deadline) {
			return
		}
	}
}

// tmuxComposerEmpty reports whether the agent TUI's composer holds no pending
// draft — tmuxSubmit uses it to confirm a turn submitted. The composer sits
// between the last pair of horizontal-rule lines the TUI draws above its footer;
// an empty box is just the prompt marker ("❯"/"›"/">") with nothing after. When
// the composer can't be located it returns false (don't claim empty), so
// tmuxSubmit errs toward an extra harmless Enter rather than a dropped message.
func tmuxComposerEmpty(session string) bool {
	out, err := tmuxCapture(session)
	if err != nil {
		return false
	}
	return composerEmpty(out)
}

// composerEmpty is the pure-text core of tmuxComposerEmpty (also unit-tested).
func composerEmpty(screen string) bool {
	lines := strings.Split(screen, "\n")
	isRule := func(s string) bool {
		t := strings.TrimSpace(s)
		return len([]rune(t)) >= 10 && strings.Trim(t, "─") == ""
	}
	last, prev := -1, -1
	for i, ln := range lines {
		if isRule(ln) {
			prev, last = last, i
		}
	}
	if prev < 0 || last <= prev {
		return false // composer geometry not found
	}
	box := strings.TrimSpace(strings.Join(lines[prev+1:last], ""))
	box = strings.TrimSpace(strings.TrimLeft(box, "❯›> "))
	return box == ""
}

// tmuxConfirmTrust watches a freshly-launched agent session for its per-directory
// trust dialog (claude's "trust this folder" / codex's "trust the contents of
// this directory") and accepts it (both default to "Yes", confirm on Enter).
// Neither agent's --dangerously-* flag bypasses this gate. Polls rather than
// sleeping so it survives a slow setup script; if already trusted the dialog
// never appears and this times out harmlessly.
func tmuxConfirmTrust(session string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		out, err := tmuxCapture(session)
		if err != nil {
			continue
		}
		if strings.Contains(out, "trust this folder") || // claude
			strings.Contains(out, "trust the contents of this directory") { // codex
			_ = tmuxSendEnter(session)
			return
		}
	}
}

// tmuxKillAgent stops the agent running in a session: Ctrl-C, then poll until the
// foreground process is back to a shell (the agent exited). It does NOT kill the
// session — the shell stays so the tab survives. Returns whether the agent
// actually went away within the deadline.
func tmuxKillAgent(session string) bool {
	for i := 0; i < 3; i++ {
		_ = tmuxSendCtrlC(session)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			if sessionAgentKind(session) == "" {
				return true
			}
		}
	}
	return sessionAgentKind(session) == ""
}
