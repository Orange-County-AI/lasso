import { useQueries } from "@tanstack/react-query"
import {
  LayoutGrid,
  Loader2,
  Maximize2,
  Paperclip,
  Plus,
  Search,
  Send,
  SquareX,
  X,
} from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import { AgentLines } from "@/components/AgentParts"
import { Markdown } from "@/components/Markdown"
import { Orb } from "@/components/ui/orb"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import {
  agentName,
  groupAgentsByHost,
  paneKey,
  sortAgentsByPriority,
  useAgents,
} from "@/lib/agents"
import { api, type ChatItem, type ChatPayload, type HostPane } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk, queryClient } from "@/lib/query"
import { cn } from "@/lib/utils"

// The fleet as parallel conversations: every agent lasso can reach in a grid,
// grouped by herdr machine unless the header toggle says otherwise, attention
// order (blocked > working > idle > done) either way. Each card reads the
// agent's TRANSCRIPT — the same source ChatView renders — never the terminal:
// the pty stays fitted to the iframe underneath (see App's overlay note), and
// N terminals cannot share one column anyway.
//
// Polling is per agent at 5s (not ChatView's 2s): N × 2s against SFTP
// transcripts is the fan-out this view buys into, and a parallel monitor reads
// tails, not keystrokes. The queries share qk.chat entries with the single
// chat, so opening a card is a cache hit, not a refetch.

// Auto-fill, not breakpoints: the overlay is ~60% of the viewport, so viewport
// breakpoints cannot know a card's width. One column on a narrow tablet
// portrait, up to three on a wide desktop.
const gridClass =
  "grid gap-2 [grid-template-columns:repeat(auto-fill,minmax(300px,1fr))]"

// Everything a filter may match, lowercased once: the name and labels the
// header shows, the harness and cwd it works in, the machine it runs on, and
// what was actually said — user and agent prose plus tool headings, subjects
// and result lines. Thinking is skipped (folded in the UI, skipped here).
function agentSearchText(pane: HostPane, chat?: ChatPayload): string {
  const bits: (string | undefined)[] = [
    agentName(pane),
    pane.workspace_label,
    pane.pane_label,
    pane.tab_label,
    pane.terminal_title,
    pane.agent,
    pane.cwd,
    pane.host,
    pane.host_label,
    chat?.title,
    chat?.agent,
    chat?.cwd,
  ]
  for (const item of chat?.items ?? []) {
    if (item.kind === "user" || item.kind === "agent") bits.push(item.text)
    else if (item.kind === "tool" && item.tool)
      bits.push(
        item.tool.title,
        item.tool.subject,
        item.tool.result_line,
        item.tool.command
      )
    else if (item.kind === "marker") bits.push(item.text)
  }
  return bits.filter(Boolean).join("\n").toLowerCase()
}
export function AgentsView({
  onShowChat,
  onNewAgent,
  className,
}: {
  // Opening a card lands on the single-chat conversation for it: full history,
  // ask answering, close. The card focuses the pane first (moving the tab when
  // the agent is elsewhere), then this flips the view — the chat follows the
  // same pane_id over SSE that the terminal does.
  onShowChat: () => void
  onNewAgent: () => void
  className?: string
}) {
  const { agents, isLoading, error, unlisted, focusAgent } = useAgents()
  const { host: tabHost } = useApp()
  const [groupByHost, setGroupByHost] = React.useState(true)
  const [filter, setFilter] = React.useState("")

  // Transcripts live HERE, not per card, for one reason: the search filters on
  // transcript content, and that needs every conversation's text in one place.
  // Same keys (and 5s poll) the cards used to own, so opening a card in the
  // single chat is still a cache hit, not a refetch.
  const chatQueries = useQueries({
    queries: agents.map((p) => ({
      queryKey: qk.chat(p.host, p.pane_id),
      queryFn: () => api.chat(p.pane_id, undefined, p.host),
      refetchInterval: 5000,
      refetchIntervalInBackground: false,
    })),
  })
  const chats = React.useMemo(() => {
    const at = new Map<string, ChatPayload | undefined>()
    agents.forEach((p, i) => {
      at.set(paneKey(p), chatQueries[i]?.data)
    })
    return at
  }, [agents, chatQueries])

  const q = filter.trim().toLowerCase()
  const visible = React.useMemo(() => {
    if (!q) return agents
    return agents.filter((p) =>
      agentSearchText(p, chats.get(paneKey(p))).includes(q)
    )
  }, [agents, chats, q])
  // Priority order either way — the toggle only decides whether machine
  // sections divide the grid, never what floats to the top.
  const groups = React.useMemo(
    () => groupAgentsByHost(visible, tabHost),
    [visible, tabHost]
  )
  const flat = React.useMemo(() => sortAgentsByPriority(visible), [visible])
  const blocked = visible.filter((p) => p.agent_status === "blocked").length
  const working = visible.filter((p) => p.agent_status === "working").length
  const openCard = (p: HostPane) => async () => {
    onShowChat()
    await focusAgent(p)
  }

  return (
    <div
      className={cn(
        "vsurface relative flex h-full min-h-0 flex-col bg-background",
        className
      )}
    >
      <div className="flex flex-none flex-wrap items-center gap-2 border-border border-b px-2.5 py-1.5">
        <span className="shrink-0 text-[12.5px] text-foreground">
          Agents{agents.length > 0 && ` (${agents.length})`}
        </span>
        {unlisted.length > 0 && (
          <span
            className="shrink-0 text-[11px] text-muted-foreground"
            title={unlisted.join(", ")}
          >
            {unlisted.length} host{unlisted.length === 1 ? "" : "s"} unreachable
          </span>
        )}
        {/* Filter-as-you-type over metadata and transcript content (see
            agentSearchText): the transcripts are already here, so matching is
            a substring over text this view holds rather than a new fetch. */}
        <div className="relative min-w-0 flex-1 basis-40">
          <Search className="pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter agents…"
            aria-label="Filter agents"
            className="h-8 w-full rounded-md border border-input bg-background pr-7 pl-8 text-[13px] outline-none placeholder:text-muted-foreground focus:border-primary"
          />
          {filter && (
            <button
              type="button"
              onClick={() => setFilter("")}
              title="Clear filter"
              aria-label="Clear filter"
              className="absolute top-1/2 right-1.5 flex size-5 -translate-y-1/2 items-center justify-center rounded text-muted-foreground hover:bg-accent hover:text-foreground"
            >
              <X className="size-3.5" />
            </button>
          )}
        </div>
        <span className="flex shrink-0 items-center gap-1">
          {/* Ghost when off, filled when on: aria-pressed alone is invisible,
              and the grid looks identical either way until you read the
              section headers — the button itself has to say which mode won. */}
          <Button
            variant={groupByHost ? "secondary" : "ghost"}
            size="sm"
            aria-pressed={groupByHost}
            title={
              groupByHost
                ? "Ungroup: one list in priority order"
                : "Group by machine"
            }
            onClick={() => setGroupByHost((v) => !v)}
          >
            <LayoutGrid />
            Group
          </Button>
          <Button
            variant="ghost"
            size="sm"
            title="New agent"
            onClick={onNewAgent}
          >
            <Plus />
            New
          </Button>
        </span>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {isLoading && (
          <div className="flex items-center justify-center gap-2 py-6 text-[12px] text-muted-foreground">
            <Orb state="working" px={16} />
            loading agents…
          </div>
        )}
        {error && (
          <div className="rounded-lg border border-destructive/40 bg-destructive/8 px-3 py-2 text-[12px] text-destructive">
            could not list agents: {(error as Error).message}
          </div>
        )}
        {!isLoading && !error && agents.length === 0 && (
          <div className="py-6 text-center text-[12px] text-muted-foreground">
            No agents running anywhere lasso can reach.
          </div>
        )}
        {!isLoading && !error && agents.length > 0 && visible.length === 0 && (
          <div className="py-6 text-center text-[12px] text-muted-foreground">
            No agents match “{filter.trim()}”.{" "}
            <button
              type="button"
              onClick={() => setFilter("")}
              className="text-primary hover:underline"
            >
              Clear
            </button>
          </div>
        )}
        {groupByHost
          ? groups.map((g) => (
              <section key={g.host} className="mb-3 last:mb-0">
                <header className="mb-1.5 flex items-center gap-2 px-1">
                  <span
                    className="shrink-0 rounded-sm bg-muted px-1.5 py-0.5 text-[11px] text-foreground/80"
                    title={g.host}
                  >
                    {g.hostLabel}
                  </span>
                  <span className="text-[11px] text-muted-foreground">
                    {g.panes.length} agent{g.panes.length === 1 ? "" : "s"}
                    {g.blocked > 0 && (
                      <span className="text-destructive">
                        {" "}
                        · {g.blocked} blocked
                      </span>
                    )}
                    {g.working > 0 && (
                      <span className="text-primary">
                        {" "}
                        · {g.working} working
                      </span>
                    )}
                  </span>
                </header>
                {/* Auto-fill, not breakpoints: the overlay is ~60% of the
                    viewport, so viewport breakpoints cannot know a card's
                    width. One column on a narrow tablet portrait, up to three
                    on a wide desktop. */}
                <div className={gridClass}>
                  {g.panes.map((p) => (
                    <AgentCard
                      key={paneKey(p)}
                      pane={p}
                      data={chats.get(paneKey(p))}
                      onOpen={openCard(p)}
                    />
                  ))}
                </div>
              </section>
            ))
          : visible.length > 0 && (
              <>
                <div className="mb-1.5 px-1 text-[11px] text-muted-foreground">
                  {visible.length} agent{visible.length === 1 ? "" : "s"}
                  {blocked > 0 && (
                    <span className="text-destructive">
                      {" "}
                      · {blocked} blocked
                    </span>
                  )}
                  {working > 0 && (
                    <span className="text-primary"> · {working} working</span>
                  )}
                </div>
                <div className={gridClass}>
                  {flat.map((p) => (
                    <AgentCard
                      key={paneKey(p)}
                      pane={p}
                      data={chats.get(paneKey(p))}
                      onOpen={openCard(p)}
                    />
                  ))}
                </div>
              </>
            )}
      </div>
    </div>
  )
}

// One parallel conversation: the transcript tail plus a composer addressed at
// this card's own host + pane (pane ids are unique per host only, so the
// payload's address — not the tab's — is what a send carries). The transcript
// arrives as a prop: the parent holds every conversation's fetch, which is
// what the filter matches content against.
function AgentCard({
  pane,
  data,
  onOpen,
}: {
  pane: HostPane
  data: ChatPayload | undefined
  onOpen: () => void
}) {
  const { pane_id: paneID, host } = pane
  const items = React.useMemo(
    () => (data?.items ?? []).slice(-30),
    [data]
  )
  const running = data?.running ?? false

  // Pinned to the tail while the reader is at the bottom; a reader scrolled up
  // keeps their place (same stick contract as the single chat, per card).
  const scrollRef = React.useRef<HTMLDivElement>(null)
  const stick = React.useRef(true)
  React.useLayoutEffect(() => {
    const el = scrollRef.current
    if (!el || items.length === 0) return
    if (stick.current) el.scrollTop = el.scrollHeight
  }, [items])

  const [text, setText] = React.useState("")
  const [sending, setSending] = React.useState(false)
  const [notice, setNotice] = React.useState<string | null>(null)
  // Files staged for the next message, same contract as the single chat:
  // chips show the NAME (a thumbnail for images), the paths ride WITH the
  // message, and a mis-paste is removable without clearing the draft.
  const [attachments, setAttachments] = React.useState<
    { path: string; name: string; image: boolean }[]
  >([])
  const [attaching, setAttaching] = React.useState(false)
  const fileRef = React.useRef<HTMLInputElement>(null)
  // Pasted or picked files land on the machine the SESSION runs on — a
  // browser blob URL would be meaningless to the agent reading its own
  // filesystem.
  const attachFiles = async (files: File[]) => {
    if (files.length === 0 || attaching || sending) return
    setAttaching(true)
    setNotice(null)
    try {
      for (const file of files) {
        const { path } = await api.pasteFile(file, host, file.name)
        setAttachments((prev) =>
          prev.some((a) => a.path === path)
            ? prev
            : [
                ...prev,
                {
                  path,
                  name: file.name || (path.split("/").pop() ?? "file"),
                  image: file.type.startsWith("image/"),
                },
              ]
        )
      }
    } catch (e) {
      setNotice(`attach failed: ${(e as Error).message}`)
    } finally {
      setAttaching(false)
    }
  }
  const [confirmClose, setConfirmClose] = React.useState(false)
  const [closing, setClosing] = React.useState(false)
  const closePane = React.useCallback(async () => {
    if (closing) return
    // The dialog's job is done: the orb over the card carries the wait, and a
    // failure reports back as a toast over a card that is still there to retry
    // from instead of a dialog that already asked its question.
    setConfirmClose(false)
    setClosing(true)
    try {
      const res = await api.close([paneID], host)
      const err = res.errors?.[paneID]
      if (err) toast.error(`could not close: ${err}`)
      else {
        // The 5s all-panes poll would drop the card on its own; this lands
        // the removal on the click instead of a beat later.
        void queryClient.invalidateQueries({ queryKey: ["all-panes"] })
      }
    } catch (e) {
      toast.error(`could not close: ${(e as Error).message}`)
    } finally {
      setClosing(false)
    }
  }, [closing, host, paneID])
  const send = React.useCallback(async () => {
    const body = text.trim()
    const paths = attachments.map((a) => a.path)
    // An attachment on its own is a complete message ("here is the
    // screenshot"), so text is not required when there is one. Paths ride
    // WITH the message, space-separated like the terminal's own paste.
    if ((!body && paths.length === 0) || sending) return
    const message = [body, ...paths].filter(Boolean).join(" ")
    setSending(true)
    setNotice(null)
    try {
      const res = await api.chatSend(host, paneID, message)
      // Three-valued on purpose (see ChatView's Composer): only a confirmed
      // send clears the draft; a refused or uncertain one keeps it and says
      // which, since an uncertain send may already have landed and retrying
      // would duplicate the turn.
      if (res.outcome === "confirmed") {
        setText("")
        setAttachments([])
      } else setNotice(res.detail || `not sent (${res.outcome})`)
    } catch (e) {
      setNotice((e as Error).message)
      toast.error(`could not send: ${(e as Error).message}`)
    } finally {
      setSending(false)
    }
  }, [text, attachments, sending, host, paneID])

  return (
    <article className="relative flex h-80 min-h-0 flex-col overflow-hidden rounded-lg border border-border bg-card">
      <header className="flex flex-none items-start gap-2 border-border border-b px-2.5 py-1.5">
        <div className="flex min-w-0 flex-1 flex-col">
          <AgentLines
            pane={data?.title ? { ...pane, workspace_label: data.title } : pane}
            current={false}
            meta={
              <>
                {/* herdr's tab, inline: the pane's siblings live here, so it
                    names which conversation cluster this card belongs to. */}
                {pane.tab_label ? (
                  <span
                    className="min-w-0 flex-1 truncate"
                    title={`herdr tab: ${pane.tab_label}`}
                  >
                    tab · {pane.tab_label}
                  </span>
                ) : null}
                {data?.tokens ? (
                  <span className="shrink-0">
                    {Math.round(data.tokens / 1000)}k
                  </span>
                ) : null}
              </>
            }
          />
        </div>
        <div className="flex shrink-0 items-center">
          <button
            type="button"
            onClick={onOpen}
            title="Open as single chat"
            aria-label="Open as single chat"
            className="flex size-7 items-center justify-center rounded-lg text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            <Maximize2 className="size-3.5" />
          </button>
          <button
            type="button"
            onClick={() => setConfirmClose(true)}
            title="Close this pane"
            aria-label="Close this pane"
            className="flex size-7 items-center justify-center rounded-lg text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            <SquareX className="size-3.5" />
          </button>
        </div>
      </header>

      <div
        ref={scrollRef}
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current =
            el.scrollHeight - el.scrollTop - el.clientHeight < 40
        }}
        className="min-h-0 flex-1 space-y-2 overflow-y-auto px-2.5 py-2"
      >
        {!data && (
          <div className="flex items-center justify-center gap-2 py-4 text-[12px] text-muted-foreground">
            <Loader2 className="size-3.5 animate-spin" />
            loading…
          </div>
        )}
        {data && items.length === 0 && !running && (
          <div className="py-4 text-center text-[12px] text-muted-foreground">
            {data.note || (data.starting ? "starting…" : "No messages yet.")}
          </div>
        )}
        {items.map((item) => (
          <MiniRow key={item.id} item={item} />
        ))}
        {running && (
          <div className="flex items-center gap-2 text-[12px] text-muted-foreground">
            <Orb state="working" px={16} />
            working…
          </div>
        )}
      </div>

      <footer className="flex-none border-border border-t p-1.5">
        {notice && (
          <div className="mb-1 truncate text-[11px] text-destructive" title={notice}>
            {notice}
          </div>
        )}
        {/* The recipient, stated on every send row — not just the placeholder,
            which vanishes the moment typing starts. Draft state is keyed by
            host + pane and a priority resort only MOVES a card, but a card
            that moved while typing is easy to misread; the → line says who
            Enter actually addresses. */}
        <div
          className="mb-1 truncate text-[11px] text-muted-foreground"
          title={`Messages here go to ${agentName(pane)} on ${pane.host_label || pane.host}`}
        >
          → {agentName(pane)}
        </div>
        <div className="flex items-center gap-1.5">
          <input
            ref={fileRef}
            type="file"
            multiple
            accept="image/*"
            className="hidden"
            onChange={(e) => {
              void attachFiles(Array.from(e.target.files ?? []))
              // Clearing lets the same file be attached twice in a row.
              e.target.value = ""
            }}
          />
          <Button
            variant="ghost"
            size="icon-sm"
            title="Attach a file and insert its path"
            aria-label="Attach a file"
            disabled={attaching || sending}
            onClick={() => fileRef.current?.click()}
          >
            {attaching ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Paperclip className="size-4" />
            )}
          </Button>
          <input
            value={text}
            onChange={(e) => setText(e.target.value)}
            onPaste={(e) => {
              // Text wins over files, exactly as in the single chat: a
              // clipboard routinely holds both (an image copied off a page
              // carries its URL too).
              if (e.clipboardData.getData("text/plain")) return
              const file = Array.from(e.clipboardData.items)
                .find((it) => it.kind === "file")
                ?.getAsFile()
              if (!file) return
              e.preventDefault()
              void attachFiles([file])
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault()
                void send()
              }
            }}
            placeholder={
              data ? `Message ${data.agent || "agent"}…` : "Message…"
            }
            aria-label="Message this agent"
            className="h-8 min-w-0 flex-1 rounded-md border border-input bg-background px-2.5 text-[13px] outline-none placeholder:text-muted-foreground focus:border-primary"
          />
          <Button
            variant="ghost"
            size="icon-sm"
            title="Send"
            aria-label="Send"
            disabled={(!text.trim() && attachments.length === 0) || sending}
            onClick={() => void send()}
          >
            {sending ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Send className="size-4" />
            )}
          </Button>
        </div>
        {attachments.length > 0 && (
          <div className="mt-1.5 flex flex-wrap gap-1.5">
            {attachments.map((a) => (
              <span
                key={a.path}
                className="flex max-w-[12rem] items-center gap-1.5 rounded-lg border border-border bg-background py-0.5 pr-0.5 pl-1.5 text-[11px] text-muted-foreground"
                title={a.path}
              >
                {a.image ? (
                  <img
                    src={api.fileURL(a.path, host)}
                    alt=""
                    className="size-5 shrink-0 rounded object-cover"
                  />
                ) : null}
                <span className="truncate">{a.name}</span>
                <button
                  type="button"
                  onClick={() =>
                    setAttachments((prev) =>
                      prev.filter((x) => x.path !== a.path)
                    )
                  }
                  aria-label={`Remove ${a.name}`}
                  title="Remove attachment"
                  className="flex size-5 shrink-0 items-center justify-center rounded text-muted-foreground/70 hover:bg-accent hover:text-foreground"
                >
                  <X className="size-3" />
                </button>
              </span>
            ))}
          </div>
        )}
      </footer>
      {/* Asked, not done: this ends the agent in the pane, and the transcript
          stays on disk either way (same contract as the single chat). */}
      <AlertDialog open={confirmClose} onOpenChange={setConfirmClose}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Close this pane?</AlertDialogTitle>
            <AlertDialogDescription>
              Herdr closes {agentName(pane)} and the agent running in it stops.
              Its session transcript stays on disk.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={closing}>Cancel</AlertDialogCancel>
            <AlertDialogAction disabled={closing} onClick={() => void closePane()}>
              {closing ? "Closing…" : "Close pane"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      {/* herdr's close is serialized with retries, so a busy host takes
          seconds: the orb sits over the card (not the dialog, which is
          already gone) until the pane is actually gone. */}
      {closing && (
        <div className="absolute inset-0 z-10 flex items-center justify-center gap-2 bg-card/80 text-[12px] text-muted-foreground">
          <Orb state="working" px={16} />
          closing…
        </div>
      )}
    </article>
  )
}

// Compact transcript row: user bubbles right, agent prose rendered (not
// printed), tool bursts as one line each, asks as a pointer to the single
// chat — answering an approval needs its options and its on-screen check,
// which live where the full conversation does.
function MiniRow({ item }: { item: ChatItem }) {
  switch (item.kind) {
    case "user":
      return (
        <div className="flex justify-end">
          <div className="max-w-[90%] whitespace-pre-wrap break-words rounded-xl rounded-br-sm border border-primary/20 bg-primary/8 px-2.5 py-1.5 text-[12.5px] text-foreground leading-snug">
            {item.text}
          </div>
        </div>
      )
    case "agent":
      if (item.thinking) return null
      return (
        <div className="md-body md-chat break-words text-[12.5px]">
          <Markdown source={item.text ?? ""} />
        </div>
      )
    case "tool": {
      const t = item.tool
      if (!t) return null
      if (t.ask)
        return (
          <div className="rounded-lg border border-destructive/40 bg-destructive/8 px-2.5 py-1.5 text-[12px] text-destructive">
            needs input — open as chat to answer
          </div>
        )
      return (
        <div className="flex items-center gap-1.5 text-[11.5px] text-muted-foreground">
          {t.state === "running" ? (
            <Orb state="working" px={12} />
          ) : (
            <span className="size-1.5 shrink-0 rounded-full bg-current opacity-70" />
          )}
          <span className="min-w-0 truncate">
            {t.title}
            {t.subject ? ` — ${t.subject}` : ""}
            {t.state !== "running" && t.result_line
              ? ` · ${t.result_line}`
              : ""}
          </span>
        </div>
      )
    }
    case "marker":
      return (
        <div className="text-center font-mono text-[11px] text-muted-foreground">
          {item.marker === "interrupted" ? "— interrupted —" : item.text}
        </div>
      )
    default:
      return null
  }
}
