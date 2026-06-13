package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// tmux is lasso's terminal/persistence layer.
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

// tmuxPrefix is the leading argv every LOCAL tmux call carries: our private
// socket and "no user config". Built fresh per call so the socket path is always
// current. (Remote calls build their own prefix in remoteBackend.TmuxArgv.)
func tmuxPrefix() []string {
	return []string{"-S", lassoTmuxSock(), "-f", "/dev/null"}
}

// --- host-aware session routing ------------------------------------------------
//
// Every tmux session is created on exactly one host and never migrates, so we
// route a session's tmux commands by looking up the host it was created on.
// sessionHosts records that mapping; an unregistered session (every LOCAL one)
// resolves to "" → the local backend, so local behavior is unchanged and only
// remote sessions are tagged.

var sessionHosts sync.Map // session name → host alias ("" / "local" = local)

func setSessionHost(session, host string) {
	if isLocalHost(host) {
		sessionHosts.Delete(session) // local is the default; no need to store
		return
	}
	sessionHosts.Store(session, host)
}

func clearSessionHost(session string) { sessionHosts.Delete(session) }

// hostForSession returns the host a session lives on ("" = local).
func hostForSession(session string) string {
	if v, ok := sessionHosts.Load(session); ok {
		return v.(string)
	}
	return ""
}

// tmuxRun executes a tmux command against host's lasso tmux server (host="" =
// local), with stdin wired to in (empty = none). It builds the argv via the
// host's Backend.TmuxArgv, so local runs `tmux …` and remote runs `ssh host
// 'tmux …'` over the control master.
func tmuxRun(host, in string, args ...string) (string, error) {
	be, err := hostBackend(host)
	if err != nil {
		return "", err
	}
	argv := be.TmuxArgv(args)
	cmd := exec.Command(argv[0], argv[1:]...)
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

// Host-targeted runners. The bare tmux/tmuxOut/tmuxIn wrappers target the LOCAL
// host (host="") — used by the no-session local helpers (server setup,
// reconcile). Session helpers call the …H variants with hostForSession(session).
func tmuxH(host string, args ...string) error              { _, err := tmuxRun(host, "", args...); return err }
func tmuxOutH(host string, args ...string) (string, error) { return tmuxRun(host, "", args...) }
func tmuxInH(host, in string, args ...string) (string, error) {
	return tmuxRun(host, in, args...)
}

func tmux(args ...string) error                        { return tmuxH("", args...) }
func tmuxOut(args ...string) (string, error)           { return tmuxOutH("", args...) }
func tmuxIn(in string, args ...string) (string, error) { return tmuxInH("", in, args...) }

// tmuxEnsureServer starts the LOCAL tmux server (idempotent) and pins lasso's
// server-wide options. See tmuxEnsureServerOn.
func tmuxEnsureServer() error { return tmuxEnsureServerOn("") }

// tmuxEnsureServerOn starts host's tmux server (idempotent) and pins the
// server-wide options lasso relies on. destroy-unattached MUST be off before any
// session goes unattached, or a detached (background) session would vanish — so
// this is called before any new-session, and defensively by tmuxNewSession.
// start-server on a running server is a no-op. The session-closed FIFO hook is
// LOCAL-only (it writes to the local ~/.lasso/sessions.closed, which only the
// local process can tail); remote session-exit detection rides the poll watcher.
func tmuxEnsureServerOn(host string) error {
	// One round-trip: start the server, then set options. The `;` separators are
	// their own argv entries (tmux command sequence), not shell tokens.
	args := []string{
		"start-server", ";",
		"set", "-g", "destroy-unattached", "off", ";",
		"set", "-g", "status", "off", ";",
		"set", "-g", "history-limit", "50000", ";",
		"set", "-g", "default-terminal", "tmux-256color", ";",
		"setw", "-g", "aggressive-resize", "on", ";",
		// Window sizes to the LATEST (most-recently-active) attached client — the
		// one whose keyboard you're using right now. When >1 client views the same
		// session (two browser tabs, a phone alongside a desktop, or a lingering
		// ghost from a ttyd reconnect), their widths differ; `latest` sizes the
		// window to whoever is typing, so a long prompt wraps to *their* viewport.
		//
		// `largest` (the previous choice) instead sized to the WIDEST client, which
		// silently corrupts every narrower viewer: tmux hands the program (Claude
		// Code) the wide COLUMNS, it lays out an un-wrapped line that overruns the
		// narrow client, and the start of the line scrolls off-screen — content
		// LOST, not merely padded. `latest` can't do that: the active client is by
		// definition full-width for itself; any wider co-viewer just sees benign "·"
		// filler columns. The stale-small-orphan worry that motivated `largest` is
		// moot under `latest` — a ghost generates no input, so it never becomes the
		// latest client once you type; the worst case is a freshly-attaching client
		// flashing 80x24 for the instant before xterm fits, which self-heals on the
		// fit resize. Set explicitly because nudgeRedraw's resize-window flips the
		// per-window option to manual and must restore it.
		"setw", "-g", "window-size", "latest",
	}
	if isLocalHost(host) {
		// Notify lasso the instant a session ends (the user exited the shell) so
		// its tab closes immediately, before the ttyd client flashes a reconnect
		// against the dead session. See startSessionCloseListener. Local only.
		args = append(args, ";",
			"set-hook", "-g", "session-closed",
			`run-shell "echo #{hook_session_name} >> `+sessionClosedFIFO()+`"`)
	}
	return tmuxH(host, args...)
}

// tmuxNewSession creates a detached LOCAL session. See tmuxNewSessionOn.
func tmuxNewSession(session, cwd string, env []string) error {
	return tmuxNewSessionOn("", session, cwd, env)
}

// tmuxNewSessionOn creates a detached session on host, rooted at cwd, with each
// "KEY=VAL" in env exported into the session (tmux >=3.2 `new-session -e`). We
// always tag the session with LASSO_TAB_ID so an agent running inside can
// identify which tab/agent it is (MCP whoami). Records the session→host mapping
// FIRST so every later command for this session routes to the right host.
// Initial geometry is generous; ttyd resizes the pane on attach.
func tmuxNewSessionOn(host, session, cwd string, env []string) error {
	setSessionHost(session, host)
	if err := tmuxEnsureServerOn(host); err != nil {
		return err
	}
	args := []string{"new-session", "-d", "-s", session, "-x", "200", "-y", "50"}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	return tmuxH(host, args...)
}

// tmuxHasSession reports whether session exists on its host's server.
func tmuxHasSession(session string) bool {
	return tmuxH(hostForSession(session), "has-session", "-t", session) == nil
}

// tmuxListSessions returns the names of all sessions on the LOCAL server.
func tmuxListSessions() []string { return tmuxListSessionsOn("") }

// tmuxListSessionsOn returns the names of all sessions on host's server (empty
// when the server isn't running / has none / the host is unreachable).
func tmuxListSessionsOn(host string) []string {
	names, _ := tmuxListSessionsOnChecked(host)
	return names
}

// tmuxListSessionsOnChecked also reports whether the (possibly empty) list is
// AUTHORITATIVE. tmux answering "no server running" is a real answer — the last
// session exited and the server went away with it. Any other failure (the SSH
// hop down, a dead control master) means we couldn't ask, and an empty list
// must NOT be read as "every session exited" — the exit watcher would close
// every tab on a host over a network blip.
func tmuxListSessionsOnChecked(host string) ([]string, bool) {
	out, err := tmuxOutH(host, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		// tmux's "server isn't up" messages (current and older phrasing) — the
		// host answered, there are no sessions. Anything else is a transport
		// failure and the answer is unknown.
		msg := err.Error()
		if strings.Contains(msg, "no server running") ||
			strings.Contains(msg, "error connecting to") {
			return nil, true
		}
		return nil, false
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, true
}

// tmuxKillSession terminates a session (and the processes inside it). Used when
// a tab/workspace is closed. Clears the session→host mapping.
func tmuxKillSession(session string) error {
	host := hostForSession(session)
	err := tmuxH(host, "kill-session", "-t", session)
	clearSessionHost(session)
	return err
}

// tmuxCapture returns the visible screen of a session's active pane, the input
// to the agent-status heuristics (detect.go) and the composer/trust checks.
func tmuxCapture(session string) (string, error) {
	return tmuxOutH(hostForSession(session), "capture-pane", "-p", "-t", session)
}

// tmuxCaptureScroll returns the last n lines of scrollback + screen (n>0), for
// the "recent output" MCP read. n is clamped to a sane ceiling by the caller.
func tmuxCaptureScroll(session string, n int) (string, error) {
	return tmuxOutH(hostForSession(session), "capture-pane", "-p", "-S", fmt.Sprintf("-%d", n), "-t", session)
}

// tmuxCaptureAll returns the entire available scrollback + screen of a session's
// active pane. `-S -` starts the capture at the very beginning of the history
// buffer (up to the session's history-limit), so callers get everything tmux
// still holds rather than a fixed tail.
func tmuxCaptureAll(session string) (string, error) {
	return tmuxOutH(hostForSession(session), "capture-pane", "-p", "-S", "-", "-t", session)
}

// tmuxCurrentPath returns the live cwd of a session's foreground process — the
// the live foreground-process cwd (drives the file viewer + the cwd we save
// to recreate a shell after a reboot).
func tmuxCurrentPath(session string) (string, error) {
	out, err := tmuxOutH(hostForSession(session), "display-message", "-p", "-t", session, "#{pane_current_path}")
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
// side effect, which would freeze the geometry so a later client resize leaves
// the window mismatched (dead "·" filler columns, or — worse — a too-wide window
// that scrolls a narrow client's line off-screen). The trailing `setw
// window-size latest` both restores automatic sizing (so future resizes follow
// the active client) AND resizes back to the current client now — a second
// SIGWINCH, another harmless repaint. (`-A` only resizes once; it does NOT
// restore automatic mode.)
func nudgeRedraw(session string) {
	host := hostForSession(session)
	wh, err := tmuxOutH(host, "display-message", "-p", "-t", session, "#{window_width} #{window_height}")
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
	_ = tmuxH(host, "resize-window", "-t", session, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h-1))
	_ = tmuxH(host, "setw", "-t", session, "window-size", "latest") // repaint + restore auto-sizing
}

// nudgeRedrawWhenAttached waits for a client to attach, then forces a full
// redraw a handful of times on a schedule. We can't pick one delay: the frame
// that needs re-pushing might be a shell prompt after a slow mise/starship rc,
// OR an agent TUI that only finishes booting seconds later (and whose shell
// prompt drew first, so cursor-position can't tell us "done").
//
// The nudge exists for ONE purpose: recover a first frame eaten by the attach
// SIGWINCH (a freshly-created session attaches blank until something repaints).
// It is NOT free to over-fire: each nudgeRedraw is a real ±1 window resize, and
// an agent TUI like Claude Code renders its UI anchored to the bottom WITHOUT the
// alternate screen, so every resize repaints that frame one row up and back — a
// visible one-row "bounce". serveTabTerm drives this on EVERY switch to an agent
// tab (the frontend re-POSTs /api/tab/term per switch), so an ungated schedule
// thrashes an already-painted Claude for ~7s every time you navigate to it.
//
// So gate on the pane actually being blank: skip entirely if it already has
// content (a re-viewed, already-painted session needs no recovery), and stop the
// moment a nudge brings content up (don't thrash the frames after recovery). This
// keeps the blank-first-frame fix while not bouncing a live TUI. Best-effort; run
// in a goroutine after spawning the ttyd.
func nudgeRedrawWhenAttached(session string) {
	if !waitAttached(session) {
		return
	}
	if out, _ := tmuxCapture(session); strings.TrimSpace(out) != "" {
		return // already painted — nudging would only bounce it
	}
	// Deltas between nudges → fires at ~0.4s, 1.2s, 2.4s, 4.4s, 7.4s post-attach.
	for _, d := range []int{400, 800, 1200, 2000, 3000} {
		time.Sleep(time.Duration(d) * time.Millisecond)
		if !tmuxHasSession(session) {
			return
		}
		nudgeRedraw(session)
		if out, _ := tmuxCapture(session); strings.TrimSpace(out) != "" {
			return // frame recovered — further nudges would just bounce it
		}
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
		n, err := tmuxOutH(hostForSession(session), "display-message", "-p", "-t", session, "#{session_attached}")
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

// primeShellPromptWhenAttached makes a freshly-created SHELL session paint its
// first prompt. Some prompt frameworks (notably starship on bash) don't draw the
// first prompt until the shell both (a) has a client attached to answer the
// terminal queries starship makes while rendering, and (b) processes an input
// event — readline computes the prompt and idles in select() without painting it,
// so the pane stays blank until the user types.
//
// It only acts on sessions explicitly marked fresh at creation (markPrimePending)
// and runs once — so it never types into an existing shell (which could submit a
// half-typed command) or an agent. The viewport switches to a new tab WHILE its
// rc is still booting, so we can't just fire Enter: any Enter sent before bash is
// reading stdin buffers in the pty and replays at the first prompt — one buffered
// Enter per send → a STACK of prompts. So we wait for the screen to QUIESCE (rc
// finished), send exactly ONE Enter (the input event that makes starship paint),
// then a resize nudge to push that freshly-painted grid to the warm client (which
// otherwise wouldn't repaint until the user resized/typed — tmux streams reliably
// to a client only on resize/input). This draws one clean prompt; the path/git
// top line fills in on the first real command, as starship does.
func primeShellPromptWhenAttached(session string) {
	if _, ok := primePending.LoadAndDelete(session); !ok {
		return // not a freshly-created shell — leave it untouched
	}
	if !waitAttached(session) {
		return
	}
	// Send an Enter every ~400ms and stop the instant the pane has content. While
	// rc is still booting bash flushes typeahead, so those Enters are DISCARDED
	// (not buffered) — no stacking; the FIRST Enter after the shell goes
	// interactive lands and prints exactly one prompt, whose output streams to the
	// attached client (an Enter is an input event, which is exactly what makes tmux
	// stream to the client). We poll across the whole rc boot (not the ~1s the old
	// per-tab ttyd needed): the viewport attaches the warm client instantly, so
	// unlike a fresh ttyd — which arrived after rc had finished — we reach the shell
	// mid-boot and must keep trying until it's ready. No resize nudge here: it
	// reflows the freshly-streamed prompt and can leave a duplicate `❯` in the
	// client's buffer.
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if !tmuxHasSession(session) {
			return
		}
		if out, _ := tmuxCapture(session); strings.TrimSpace(out) != "" {
			// Prompt is in the grid, but the priming line-accept left it pushed down
			// with blank rows above (the initial prompt scrolled off). Ctrl-L
			// redraws it at the top for a clean, top-aligned terminal.
			_ = tmuxSendCtrlL(session)
			time.Sleep(200 * time.Millisecond)
			// Force the warm client to take a clean full frame — an incremental
			// stream to a switched client is unreliable, and a resize nudge reflows
			// it into a duplicate `❯`. switch-client always sends a full redraw
			// (it's how switching to an existing tab renders correctly), so bounce
			// the client off the session and back.
			forceViewportRedraw(session)
			return
		}
		_ = tmuxSendEnter(session)
		time.Sleep(400 * time.Millisecond)
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
// SEPARATE Enter keypress) — runs a command line in a pane's shell. The literal
// flag (-l) and "--" stop tmux from interpreting text that looks like a key name
// ("Enter", "C-c") or starts with "-".
func tmuxSendLine(session, line string) error {
	if err := tmuxH(hostForSession(session), "send-keys", "-t", session, "-l", "--", line); err != nil {
		return err
	}
	return tmuxSendEnter(session)
}

// tmuxSendEnter sends a real Enter keypress (distinct from a pasted "\n"). For an
// interactive agent TUI this is what actually submits a turn — see tmuxSubmit.
func tmuxSendEnter(session string) error {
	return tmuxH(hostForSession(session), "send-keys", "-t", session, "Enter")
}

// tmuxSendCtrlC sends Ctrl-C (interrupt) — used to stop a running agent.
func tmuxSendCtrlC(session string) error {
	return tmuxH(hostForSession(session), "send-keys", "-t", session, "C-c")
}

// tmuxSendCtrlL sends Ctrl-L (clear + redraw) — readline clears the screen and
// repaints the prompt at the top. Used to clean up the blank rows the prime's
// line-accept leaves above a fresh shell's prompt.
func tmuxSendCtrlL(session string) error {
	return tmuxH(hostForSession(session), "send-keys", "-t", session, "C-l")
}

// tmuxSendText pastes text into a session as a BRACKETED PASTE (no trailing
// Enter). Going through load-buffer + `paste-buffer -p` makes the TUI treat it as
// a paste, so an embedded newline stays literal instead of submitting and
// per-character autocomplete doesn't fire — the tmux-native form of the lesson
// (the hard-won composer-submit lesson). The buffer is named per-call and deleted
// after paste (-d) so concurrent sends don't clobber each other.
func tmuxSendText(session, text string) error {
	host := hostForSession(session)
	buf := "lasso_" + randSuffix()
	if _, err := tmuxInH(host, text, "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	return tmuxH(host, "paste-buffer", "-p", "-d", "-b", buf, "-t", session)
}

// shellSingleQuote wraps s in single quotes safe for a POSIX shell command line.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
// it as a turn. Two hazards, both handled (the hard-won composer-submit lesson,
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
