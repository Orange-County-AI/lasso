package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Agent-to-agent messaging: the store-and-forward layer behind the
// message_agent MCP tool. send_agent types straight into a pane — immediate,
// but concurrent senders race each other in the composer and a message lands
// mid-turn. message_agent instead enqueues an addressed, sender-identified
// message in lasso's db; the dispatcher below delivers it into the recipient's
// pane only when herdr reports the agent idle, batching everything queued for
// that recipient into one submitted turn, in arrival order. Recipients are
// addressed by agent title or id, optionally host-qualified ("clem",
// "clem@gigachad", "<id>@titan"), resolved against live agents across hosts.

const (
	msgPending   = "pending"
	msgDelivered = "delivered"
	msgFailed    = "failed"
)

// AgentMessage is one queued message: who it's for (host + agent id), who sent
// it, and the body. Status moves pending → delivered (submitted into the pane)
// or pending → failed (the recipient died before it could be delivered).
type AgentMessage struct {
	ID          string
	Host        string
	AgentID     string
	SenderLabel string // display name in the envelope ("clem", "user", "ci")
	SenderAddr  string // reply address ("<agent id>@<host>") when the sender is a lasso agent
	Body        string
	CreatedAt   time.Time
}

// newMessageID mints a unique message id: the same time-based base-36 form as
// agent ids, plus a random tag so several enqueues in one call don't collide.
func newMessageID() string {
	return "m_" + strconv.FormatInt(time.Now().UnixNano(), 36) + randSuffix()
}

// messageEnvelope wraps a queued message for delivery into a pane. The header
// carries the sender identity and message id in-band — the recipient's TUI has
// no other metadata channel — and, when the sender is itself a lasso agent,
// the exact address a reply should target.
func messageEnvelope(m AgentMessage) string {
	from := m.SenderLabel
	if from == "" {
		from = "unknown"
	}
	hdr := fmt.Sprintf("[lasso message %s from %s", m.ID, from)
	if m.SenderAddr != "" {
		hdr += fmt.Sprintf(" — reply via the lasso message_agent tool to %q", m.SenderAddr)
	}
	return hdr + "]\n" + m.Body
}

// ---------------------------------------------------------------------------
// recipient resolution
// ---------------------------------------------------------------------------

// paneLookup returns a host's live panes keyed by pane id. Callers cache per
// batch (one pane.list per host per message_agent call / dispatch pass).
type paneLookup func(host string) (map[string]pane, error)

// hostPanes fetches a host's pane.list once, keyed by pane id.
func hostPanes(b Backend) (map[string]pane, error) {
	res, err := b.HerdrCall("pane.list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var pl struct {
		Panes []pane `json:"panes"`
	}
	if err := json.Unmarshal(res, &pl); err != nil {
		return nil, err
	}
	m := make(map[string]pane, len(pl.Panes))
	for _, p := range pl.Panes {
		m[p.PaneID] = p
	}
	return m, nil
}

// resolveRecipient maps a recipient spec to exactly one live agent, returning
// its record and live herdr status. Two passes: first the whole spec as an
// id/title (titles may legitimately contain "@"), then split at the last "@"
// into needle@host. A definitive problem from the first pass (ambiguity, or
// matches that are all dead) is only surfaced if the split pass finds nothing
// either, so "clem@gigachad" never gets stuck on a stale record titled that.
func resolveRecipient(spec string, records []hostAgent, panes paneLookup) (AgentRecord, string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return AgentRecord{}, "", fmt.Errorf("empty recipient")
	}
	rec, status, found, err1 := matchRecipient(spec, "", records, panes)
	if found {
		return rec, status, nil
	}
	if i := strings.LastIndex(spec, "@"); i > 0 {
		rec, status, found, err2 := matchRecipient(spec[:i], spec[i+1:], records, panes)
		if found {
			return rec, status, nil
		}
		if err2 != nil {
			return AgentRecord{}, "", err2
		}
	}
	if err1 != nil {
		return AgentRecord{}, "", err1
	}
	return AgentRecord{}, "", fmt.Errorf("no agent matches %q — address by the title or id (optionally \"…@host\") that list_agents shows", spec)
}

// matchRecipient resolves needle (an agent id or title) against records,
// optionally scoped to host. found=true only for a single live match; a
// definitive problem (ambiguity, or matches that are all dead/unreachable)
// comes back as err with found=false; found=false with a nil err means nothing
// matched at all. Id matches take precedence over title matches, so an id is
// always addressable even if it collides with some agent's title.
func matchRecipient(needle, host string, records []hostAgent, panes paneLookup) (AgentRecord, string, bool, error) {
	var byID, byTitle []AgentRecord
	for _, ha := range records {
		if host != "" && ha.Host != host {
			continue
		}
		r := ha.Agent
		r.Host = ha.Host
		if r.ID == needle {
			byID = append(byID, r)
		} else if strings.EqualFold(r.Title, needle) {
			byTitle = append(byTitle, r)
		}
	}
	if len(byID) > 0 {
		return pickLiveRecipient(needle, "id", byID, panes)
	}
	if len(byTitle) > 0 {
		return pickLiveRecipient(needle, "title", byTitle, panes)
	}
	return AgentRecord{}, "", false, nil
}

// pickLiveRecipient narrows candidate records to the ones whose pane still
// hosts a running agent, and insists on exactly one — messaging is addressed,
// so a dead or ambiguous target is refused, never guessed.
func pickLiveRecipient(needle, kind string, cands []AgentRecord, panes paneLookup) (AgentRecord, string, bool, error) {
	var live []AgentRecord
	var statuses []string
	var hostErr error
	for _, r := range cands {
		if r.RootPane == "" {
			continue
		}
		m, err := panes(r.Host)
		if err != nil {
			if hostErr == nil {
				hostErr = fmt.Errorf("host %q unreachable: %v", r.Host, err)
			}
			continue
		}
		p, ok := m[r.RootPane]
		if !ok || p.Agent == "" {
			continue
		}
		st := p.AgentStatus
		if st == "" {
			st = "unknown"
		}
		live, statuses = append(live, r), append(statuses, st)
	}
	switch len(live) {
	case 1:
		return live[0], statuses[0], true, nil
	case 0:
		if hostErr != nil {
			return AgentRecord{}, "", false, fmt.Errorf("%s %q: %v", kind, needle, hostErr)
		}
		return AgentRecord{}, "", false, fmt.Errorf("%s %q matches %d recorded agent(s) but none is running (pane gone or agent exited)", kind, needle, len(cands))
	default:
		return AgentRecord{}, "", false, fmt.Errorf("%s %q is ambiguous across live agents %s — address one as \"<id>@<host>\"", kind, needle, addrList(live))
	}
}

// addrList renders records as unambiguous "<id>@<host>" reply addresses for
// error messages.
func addrList(recs []AgentRecord) string {
	parts := make([]string, len(recs))
	for i, r := range recs {
		parts[i] = fmt.Sprintf("%s@%s (title %q)", r.ID, r.Host, r.Title)
	}
	return strings.Join(parts, ", ")
}

// resolveMessageSender turns the caller's self-identification into the envelope
// identity: a lasso agent passes its $HERDR_PANE_ID (resolved exactly like
// whoami, refusing cross-host pane-id collisions), anyone else passes a free-
// text label. A from_pane that doesn't resolve is an error rather than a silent
// fallback — a mis-attributed message is worse than a failed send.
func resolveMessageSender(ctx context.Context, fromPane, fromHost, from string) (label, addr string, err error) {
	fromPane = strings.TrimSpace(fromPane)
	if fromPane == "" {
		if strings.TrimSpace(from) == "" {
			return "", "", fmt.Errorf("identify the sender: pass from_pane (your $HERDR_PANE_ID) if you are a lasso agent, or a free-text `from` label otherwise")
		}
		return strings.TrimSpace(from), "", nil
	}
	var wo whoamiOut
	if fromHost == "" {
		wo = resolveWhoamiAcrossHosts(ctx, fromPane)
	} else {
		b, berr := agentBackendResolver(fromHost)
		if berr != nil {
			return "", "", berr
		}
		recs, rerr := listAgents(fromHost)
		if rerr != nil {
			return "", "", rerr
		}
		wo = resolveWhoami(b, fromHost, recs, fromPane)
	}
	if !wo.Found {
		return "", "", fmt.Errorf("from_pane %q did not resolve to a lasso agent: %s", fromPane, wo.Detail)
	}
	label = wo.Agent.Title
	if label == "" {
		label = wo.Agent.ID
	}
	return label, wo.Agent.ID + "@" + wo.Agent.Host, nil
}

// ---------------------------------------------------------------------------
// dispatcher
// ---------------------------------------------------------------------------

// msgKick wakes the dispatcher immediately after an enqueue, so delivery to an
// already-idle recipient doesn't wait out the poll interval.
var msgKick = make(chan struct{}, 1)

func kickMessageDispatch() {
	select {
	case msgKick <- struct{}{}:
	default:
	}
}

// messageDispatchLoop drains the agent_messages queue for as long as the server
// runs. A pass touches no host unless something is pending, so the steady-state
// cost is one local sqlite query per tick.
func messageDispatchLoop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-msgKick:
		case <-t.C:
		}
		dispatchPendingMessages(gridHostBackend)
	}
}

// dispatchPendingMessages makes one delivery pass over the queue. Everything
// queued for a recipient goes in a single paneSubmit — one submitted turn, in
// arrival order — and only when herdr reports the agent idle: submitting
// mid-turn would interleave with the in-flight work, and a blocked agent is
// sitting on a dialog where typed text does not belong. Hosts unreachable this
// pass are skipped (their messages stay pending); messages whose agent is gone
// are marked failed so a later occupant of the pane never receives them.
func dispatchPendingMessages(backendFor func(string) (Backend, error)) {
	pending, err := listPendingMessages()
	if err != nil || len(pending) == 0 {
		return
	}
	type target struct{ host, agentID string }
	var order []target
	grouped := map[target][]AgentMessage{}
	for _, m := range pending {
		k := target{m.Host, m.AgentID}
		if _, ok := grouped[k]; !ok {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], m)
	}
	// One backend + pane.list + record load per involved host. A nil panes entry
	// means the host was unreachable this pass — leave its messages pending.
	backends := map[string]Backend{}
	panes := map[string]map[string]pane{}
	recs := map[string]map[string]AgentRecord{}
	for _, k := range order {
		if _, seen := panes[k.host]; seen {
			continue
		}
		panes[k.host] = nil
		b, err := backendFor(k.host)
		if err != nil {
			continue
		}
		pm, err := hostPanes(b)
		if err != nil {
			continue
		}
		rm := map[string]AgentRecord{}
		rs, err := listAgents(k.host)
		if err != nil {
			continue
		}
		for _, r := range rs {
			rm[r.ID] = r
		}
		backends[k.host], panes[k.host], recs[k.host] = b, pm, rm
	}
	failAll := func(msgs []AgentMessage, why string) {
		for _, m := range msgs {
			_ = markMessageFailed(m.ID, why)
		}
	}
	for _, k := range order {
		pm := panes[k.host]
		if pm == nil {
			continue // host unreachable — retry next pass
		}
		msgs := grouped[k]
		rec, ok := recs[k.host][k.agentID]
		switch p, live := pm[rec.RootPane]; {
		case !ok || rec.RootPane == "":
			failAll(msgs, "agent record or pane is gone")
		case !live:
			failAll(msgs, "agent pane is gone")
		case p.Agent == "":
			failAll(msgs, "agent process exited (pane is a bare shell)")
		case p.AgentStatus != "idle":
			// mid-turn or blocked — keep queued for a later pass
		default:
			parts := make([]string, len(msgs))
			for i, m := range msgs {
				parts[i] = messageEnvelope(m)
			}
			paneSubmit(backends[k.host], rec.RootPane, strings.Join(parts, "\n\n"))
			for _, m := range msgs {
				_ = markMessageDelivered(m.ID)
			}
		}
	}
}
