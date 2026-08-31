package main

// Notifications — lasso telling a HUMAN about something that happened while
// they were not looking at it. Distinct from `notice` (main.go), which is a
// toast pushed over SSE to tabs that are already open and watching: by
// construction that reaches nobody when the laptop is shut and the phone is in
// a pocket, which is the only case a notification is for.
//
// The shape is two registries so that the second of either costs nothing:
//
//   - a KIND says what happened (agent blocked, today). Kinds are what a user
//     will eventually want to switch on and off individually, so they are a
//     closed enum rather than free-form strings.
//   - a TRANSPORT says how it reaches the human. Web Push (webpush.go) is the
//     only one today; ntfy, email, or an iMessage bridge would each be another
//     implementation of the same interface and would need no change here.
//
// Everything above the transport is deliberately ignorant of Web Push: a
// notification carries text, a collapse tag, and the host the event happened
// on — not endpoints, VAPID keys, or a URL. Transports own their addressing.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// notifKind is what happened. Two producers today: the blocked watcher
// (notifywatch.go), which nobody asked for explicitly, and an agent that
// deliberately pings its human (the `notify` MCP tool / `lasso notify`).
type notifKind string

const (
	// notifAgentBlocked: an agent stopped mid-task and is waiting on a human —
	// a tool-approval dialog, codex's "Action Required", omp's plan gate. The
	// one event that costs real time to miss, since nothing moves until someone
	// answers it.
	notifAgentBlocked notifKind = "agent_blocked"
	// notifAgentMessage: an agent chose to say something — "the migration is
	// green", "I need a decision on the schema". Unlike a blocked agent this is
	// not inferred from a status, so it is never collapsed or rate-limited: the
	// agent asked once and means it.
	notifAgentMessage notifKind = "agent_message"
	// notifTest is the Settings tab's "send a test notification" button. It is a
	// kind rather than a special case inside a transport so that the button
	// exercises exactly the path a real notification takes.
	notifTest notifKind = "test"
)

// notification is one thing worth telling the user about.
type notification struct {
	Kind  notifKind
	Title string
	Body  string
	// Tag collapses re-sends: a notification with a tag REPLACES an earlier one
	// carrying the same tag on the device rather than stacking beside it. Keyed
	// per pane by the watcher, so an agent that blocks, unblocks and blocks
	// again leaves one entry in the notification centre, not three.
	Tag string
	// Host is the host the event happened on ("local" or an ssh alias), so
	// opening the notification can land the tab on the right machine. It names
	// no pane on purpose: herdr's focus is one global per session, shared with
	// the TUI and every other lasso client, so a link that focused a pane would
	// move it for everyone (the same reason lib/url.ts keeps focus out of the
	// URL).
	Host string
}

// notifTransport is one way a notification reaches a human.
type notifTransport interface {
	// name identifies the transport in logs.
	name() string
	// active reports whether this transport has anywhere to deliver TO — no
	// registered devices means the whole notification pipeline is off, and the
	// watcher's fleet poll never runs. Called on a poll interval, so it must be
	// cheap.
	active() bool
	// deliver sends n. It is called off the caller's goroutine and may block up
	// to notifDeliverTimeout.
	deliver(ctx context.Context, n notification) error
}

var notifTransportsMu sync.RWMutex
var notifTransports []notifTransport

// registerNotifTransport adds a transport to the fan-out. Called once per
// transport from runServer; nothing registers a transport at init time, so CLI
// subcommands and tests deliver nowhere unless they opt in.
func registerNotifTransport(t notifTransport) {
	notifTransportsMu.Lock()
	defer notifTransportsMu.Unlock()
	notifTransports = append(notifTransports, t)
}

// notifDeliverTimeout bounds one fan-out. A push service that has gone slow
// costs at most this much of a background goroutine, and never delays the poll
// loop that produced the notification.
const notifDeliverTimeout = 30 * time.Second

// activeNotifTransports returns the transports with somewhere to deliver.
func activeNotifTransports() []notifTransport {
	notifTransportsMu.RLock()
	all := notifTransports
	notifTransportsMu.RUnlock()
	var out []notifTransport
	for _, t := range all {
		if t.active() {
			out = append(out, t)
		}
	}
	return out
}

// notifyEnabled reports whether any transport has a destination. The blocked
// watcher gates its fleet-wide pane.list poll on this: with no device
// subscribed, notifications must cost exactly nothing.
func notifyEnabled() bool { return len(activeNotifTransports()) > 0 }

// deliverNotification sends n through every active transport and reports which
// ones took it, plus whatever the rest failed with.
//
// This is the request-path form. An agent that pings its human has to be told
// whether anything is actually listening — "sent" when nothing is subscribed is
// the one answer that would make the feature worse than not having it — and the
// Settings tab's test button exists precisely to surface the error. Background
// producers use publishNotification instead, which cannot wait on a push
// service.
//
// A partial result is a real one: sent names the transports that accepted it and
// err carries the others' failures, so both can be reported at once.
func deliverNotification(ctx context.Context, n notification) (sent []string, err error) {
	var errs []error
	for _, t := range activeNotifTransports() {
		if e := t.deliver(ctx, n); e != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.name(), e))
			continue
		}
		sent = append(sent, t.name())
	}
	err = errors.Join(errs...)
	// Log what went out, not just what failed. This interrupts a human on a
	// locked phone, so "why did I get that?" has to be answerable from the log
	// afterwards — otherwise the only way to explain a notification is to
	// re-derive it from a live fleet query, which is exactly what happened the
	// first time one looked surprising. The tag names the pane; the body is left
	// out (it is the agent's text, and it already reached the device).
	switch {
	case len(sent) > 0 && err != nil:
		log.Printf("notify: %s %q via %s [%s] (some failed: %v)", n.Kind, n.Title, strings.Join(sent, ", "), n.Tag, err)
	case len(sent) > 0:
		log.Printf("notify: %s %q via %s [%s]", n.Kind, n.Title, strings.Join(sent, ", "), n.Tag)
	case err != nil:
		log.Printf("notify: %s %q undelivered: %v", n.Kind, n.Title, err)
	}
	return sent, err
}

// notifyNoDestination is what a caller is told when the notification went
// nowhere because nothing is subscribed. It names the fix, because the caller is
// often an agent that will otherwise report "I notified you" to a human who
// received nothing.
const notifyNoDestination = "nothing is subscribed to notifications — a device has to register itself in lasso's Settings tab first (on iOS: add lasso to the Home Screen, then enable it there)"

// notifyResult is the request-path answer: whether the notification reached
// anything, through what, and what went wrong. Shared by the Settings test
// button and the notify MCP tool so "sent" means the same thing in both.
type notifyResult struct {
	Sent       bool
	Transports []string
	Detail     string
}

// notifyNow delivers n and describes the outcome. It never errors: a caller
// asking to reach a human wants to know what happened, and "no device is
// subscribed" is an answer, not a fault.
func notifyNow(ctx context.Context, n notification) notifyResult {
	sent, err := deliverNotification(ctx, n)
	res := notifyResult{Sent: len(sent) > 0, Transports: sent}
	switch {
	case err != nil:
		res.Detail = err.Error()
	case len(sent) == 0:
		res.Detail = notifyNoDestination
	}
	return res
}

// publishNotification fans n out to every active transport, off the caller's
// goroutine. Its callers are poll loops; none may wait on a push service, and a
// delivery failure is theirs to ignore — deliverNotification logs both the
// outcome and any failure, so there is nothing left for this to report.
func publishNotification(n notification) {
	if !notifyEnabled() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifDeliverTimeout)
		defer cancel()
		_, _ = deliverNotification(ctx, n)
	}()
}
