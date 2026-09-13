import { useQuery } from "@tanstack/react-query"
import {
  AlertTriangle,
  ArrowUp,
  Check,
  ChevronRight,
  Globe,
  Image as ImageIcon,
  ListTodo,
  Loader2,
  Pencil,
  Search,
  Send,
  SquareTerminal,
  Users,
  Wrench,
  X,
} from "lucide-react"
import * as React from "react"

import { api, type ChatDiffLine, type ChatItem, type ChatTool } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import { qk } from "@/lib/query"
import { cn } from "@/lib/utils"

// The agent session as a conversation: what the terminal shows, rendered so a
// phone can read it without a 40-column TUI. The shape follows Moshi's chat
// view — a tool result grouped into a card, thinking folded away, a diff
// excerpted rather than dumped — but the palette is lasso's own, so it inherits
// whatever theme (and backdrop) the rest of the app is wearing.
//
// READ-ONLY, deliberately. Input stays the real TUI: the composer pastes into
// the herdr terminal and presses Enter, exactly as the mobile input buffer
// does, so a tool approval is still answered where the agent asked for it and
// nothing here can drift from what the pane actually received.

// One rendered row: a lone item, or a run of calls collapsed into one card.
type Row =
  | { kind: "single"; item: ChatItem }
  | { kind: "group"; id: string; calls: ChatTool[] }

// groupRows collapses consecutive tool calls that share a group key (a burst of
// Reads, a burst of Edits) into one card. Ten Reads as ten cards is a wall; as
// one card with a count it is a sentence. The key is the SERVER's — it knows
// which tool names mean the same thing across harnesses.
function groupRows(items: ChatItem[]): Row[] {
  const rows: Row[] = []
  let run: ChatTool[] = []
  let key: string | null = null
  const flush = () => {
    if (run.length === 0) return
    if (run.length === 1) {
      rows.push({
        kind: "single",
        item: { kind: "tool", id: run[0].call_id, tool: run[0] },
      })
    } else {
      rows.push({ kind: "group", id: run[0].call_id, calls: run })
    }
    run = []
    key = null
  }
  for (const item of items) {
    const g = item.kind === "tool" ? item.tool?.group : undefined
    if (item.kind === "tool" && item.tool && g) {
      if (key !== null && g !== key) flush()
      key = g
      run.push(item.tool)
      continue
    }
    flush()
    rows.push({ kind: "single", item })
  }
  flush()
  return rows
}

// Family → icon. Lasso's design law is that chrome separates by border and a
// brightness step, never by colour, so the family lives in the glyph and colour
// is reserved for STATE (a failure, a call still running). That is also what
// keeps the cards readable under every theme and backdrop the app can wear.
const FAMILY_ICON: Record<string, typeof Wrench> = {
  shell: SquareTerminal,
  eval: SquareTerminal,
  read: Pencil,
  write: Pencil,
  edit: Pencil,
  search: Search,
  web: Globe,
  todo: ListTodo,
  task: Users,
  image: ImageIcon,
  generic: Wrench,
}

function StateMark({ state }: { state: ChatTool["state"] }) {
  if (state === "running") {
    return <Loader2 className="size-3.5 shrink-0 animate-spin text-primary" />
  }
  if (state === "error") {
    return <X className="size-3.5 shrink-0 text-destructive" />
  }
  return <Check className="size-3.5 shrink-0 text-muted-foreground" />
}

function Duration({ ms }: { ms?: number }) {
  if (!ms) return null
  const s = Math.round(ms / 1000)
  // Sub-second calls are common and rounding them to "0s" reads as a bug.
  const text =
    ms < 1000
      ? `${ms}ms`
      : s < 60
        ? `${s}s`
        : `${Math.floor(s / 60)}m ${s % 60}s`
  return (
    <span className="shrink-0 text-[11px] text-muted-foreground">{text}</span>
  )
}

function DiffLines({ lines }: { lines: ChatDiffLine[] }) {
  return (
    <div className="border-border/60 border-t py-1 font-mono text-[11.5px] leading-[1.7]">
      {lines.map((l, i) => (
        <div
          key={`${i}-${l.kind}`}
          className={cn(
            "flex",
            l.kind === "add" && "bg-primary/7",
            l.kind === "del" && "bg-destructive/10"
          )}
        >
          <span
            className={cn(
              "min-w-0 flex-1 truncate whitespace-pre px-3",
              l.kind === "add" && "text-foreground",
              l.kind === "del" && "text-muted-foreground/80",
              l.kind === "context" && "text-muted-foreground"
            )}
          >
            {l.kind === "add" ? "+ " : l.kind === "del" ? "− " : "  "}
            {l.text}
          </span>
        </div>
      ))}
    </div>
  )
}

function ToolBody({ tool }: { tool: ChatTool }) {
  return (
    <>
      {tool.command && (
        <pre className="overflow-x-auto whitespace-pre-wrap break-words border-border/60 border-t px-3 py-2 font-mono text-[11.5px] text-muted-foreground leading-[1.6]">
          <span className="select-none text-primary">$ </span>
          {tool.command}
        </pre>
      )}
      {tool.diff && tool.diff.length > 0 && <DiffLines lines={tool.diff} />}
      {tool.error ? (
        <div className="border-border/60 border-t bg-destructive/8 px-3 py-2 font-mono text-[11.5px] text-destructive">
          {tool.error}
        </div>
      ) : (
        tool.output && (
          <pre className="max-h-56 overflow-y-auto border-border/60 border-t px-3 py-2 font-mono text-[11px] text-muted-foreground leading-[1.6]">
            {tool.output}
          </pre>
        )
      )}
      {tool.images ? (
        <div className="border-border/60 border-t px-3 py-2 text-[11px] text-muted-foreground">
          {tool.images} image{tool.images === 1 ? "" : "s"}
        </div>
      ) : null}
    </>
  )
}

// ToolCard is one call. Diffs and commands open by default — they are what a
// reader came for — while a long output stays folded behind the header, which
// is the whole reason a card can be scanned on a phone.
function ToolCard({ tool }: { tool: ChatTool }) {
  const hasBody = Boolean(
    tool.command ||
      tool.diff?.length ||
      tool.output ||
      tool.error ||
      tool.images
  )
  const [open, setOpen] = React.useState(
    Boolean(tool.diff?.length || tool.error) && hasBody
  )
  const Icon = FAMILY_ICON[tool.family] ?? Wrench
  return (
    <div
      className={cn(
        "overflow-hidden rounded-lg border bg-card",
        tool.state === "error" ? "border-destructive/40" : "border-border"
      )}
    >
      <button
        type="button"
        onClick={() => hasBody && setOpen((v) => !v)}
        className={cn(
          "flex w-full items-center gap-2 px-2.5 py-2 text-left",
          hasBody && "hover:bg-accent/40"
        )}
      >
        {hasBody ? (
          <ChevronRight
            className={cn(
              "size-3 shrink-0 text-muted-foreground transition-transform",
              open && "rotate-90"
            )}
          />
        ) : (
          <span className="size-3 shrink-0" />
        )}
        <Icon className="size-3.5 shrink-0 text-muted-foreground" />
        <span className="shrink-0 font-semibold text-[12.5px] text-foreground">
          {tool.title}
        </span>
        {tool.subject && (
          <span className="truncate font-mono text-[11.5px] text-muted-foreground">
            {tool.subject}
          </span>
        )}
        <span className="ml-auto flex shrink-0 items-center gap-2 pl-2">
          {tool.result_line && tool.state === "completed" && (
            <span className="font-mono text-[10.5px] text-muted-foreground">
              {tool.result_line}
            </span>
          )}
          <Duration ms={tool.duration_ms} />
          <StateMark state={tool.state} />
        </span>
      </button>
      {open && hasBody && <ToolBody tool={tool} />}
    </div>
  )
}

// ToolGroup is a run of calls that share a group key. The heading is the union
// of the members' titles ("Read", "Read+Grep") plus how many there were, and
// expanding lists each one with its own state — so a burst is one row until you
// ask for the detail.
function ToolGroup({ calls }: { calls: ChatTool[] }) {
  const [open, setOpen] = React.useState(false)
  const title = [...new Set(calls.map((c) => c.title))].join("+")
  const last = calls[calls.length - 1]
  const errors = calls.filter((c) => c.state === "error").length
  const running = calls.some((c) => c.state === "running")
  const Icon = FAMILY_ICON[last.family] ?? Wrench
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-card">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-2 px-2.5 py-2 text-left hover:bg-accent/40"
      >
        <ChevronRight
          className={cn(
            "size-3 shrink-0 text-muted-foreground transition-transform",
            open && "rotate-90"
          )}
        />
        <Icon className="size-3.5 shrink-0 text-muted-foreground" />
        <span className="shrink-0 font-semibold text-[12.5px] text-foreground">
          {title}
        </span>
        <span className="shrink-0 rounded bg-muted px-1.5 py-px font-mono text-[10px] text-muted-foreground">
          {calls.length}
        </span>
        {last.subject && (
          <span className="truncate font-mono text-[11.5px] text-muted-foreground">
            {last.subject}
          </span>
        )}
        <span className="ml-auto pl-2">
          {errors > 0 ? (
            <X className="size-3.5 text-destructive" />
          ) : running ? (
            <Loader2 className="size-3.5 animate-spin text-primary" />
          ) : (
            <Check className="size-3.5 text-muted-foreground" />
          )}
        </span>
      </button>
      {open && (
        <div className="flex flex-col gap-1 border-border/60 border-t px-2.5 py-2">
          {calls.map((c) => (
            <div
              key={c.call_id}
              className="flex items-center gap-2 font-mono text-[11px]"
            >
              <span className="shrink-0 text-muted-foreground">›</span>
              <span className="truncate text-muted-foreground">
                {c.subject || c.command || c.name}
              </span>
              <span className="ml-auto shrink-0">
                {c.state === "error" ? (
                  <span className="text-destructive">failed</span>
                ) : c.state === "running" ? (
                  <Loader2 className="size-3 animate-spin text-primary" />
                ) : c.result_line ? (
                  <span className="text-muted-foreground/70">
                    {c.result_line}
                  </span>
                ) : null}
              </span>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// ThinkingRow is folded by default with its first line as the preview — the
// reasoning is worth having and not worth reading first.
function ThinkingRow({ text }: { text: string }) {
  const [open, setOpen] = React.useState(false)
  const preview = text.split("\n")[0]?.slice(0, 140) ?? ""
  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center gap-1.5 text-left text-[11.5px] text-muted-foreground"
      >
        <ChevronRight
          className={cn(
            "size-3 shrink-0 transition-transform",
            open && "rotate-90"
          )}
        />
        <span className="shrink-0 italic">Thinking</span>
        {!open && (
          <span className="truncate text-muted-foreground/70 italic">
            {preview}
          </span>
        )}
      </button>
      {open && (
        <div className="mt-1.5 whitespace-pre-wrap border-border border-l-2 pl-3 text-[12.5px] text-muted-foreground leading-relaxed">
          {text}
        </div>
      )}
    </div>
  )
}

function RowView({ row }: { row: Row }) {
  if (row.kind === "group") return <ToolGroup calls={row.calls} />
  const item = row.item
  switch (item.kind) {
    case "user":
      return (
        <div className="flex justify-end">
          <div className="max-w-[85%] whitespace-pre-wrap break-words rounded-xl rounded-br-sm border border-primary/20 bg-primary/8 px-3 py-2 text-[13.5px] text-foreground leading-snug">
            {item.text}
          </div>
        </div>
      )
    case "agent":
      return item.thinking ? (
        <ThinkingRow text={item.text ?? ""} />
      ) : (
        <div className="whitespace-pre-wrap break-words text-[13.5px] text-foreground leading-relaxed">
          {item.text}
        </div>
      )
    case "tool":
      return item.tool ? <ToolCard tool={item.tool} /> : null
    case "marker":
      if (item.marker === "interrupted") {
        return (
          <div className="text-center font-mono text-[12px] text-muted-foreground">
            — interrupted —
          </div>
        )
      }
      return (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/40 bg-destructive/8 px-2.5 py-2 text-[12px] text-destructive">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
          <span className="min-w-0 break-words">
            {item.text}
            {item.count && item.count > 1 ? (
              <span className="text-destructive/70"> ×{item.count}</span>
            ) : null}
          </span>
        </div>
      )
    default:
      return null
  }
}

// Unsent text, kept per TARGET (host + pane) rather than per composer. The chat
// follows herdr's focus, so a single draft on the composer would silently
// re-aim itself at whatever agent got focused next — and text written for one
// agent must never be one Enter away from another. Keyed by target, so moving
// focus and coming back finds the sentence still there; scoped to this page's
// life, which is also as long as the panes it names can be trusted to mean the
// same thing.
const draftsByTarget = new Map<string, string>()

// Composer types into the pane the transcript belongs to — addressed by host
// AND pane, both taken from the payload on screen.
//
// The draft is cleared ONLY on a confirmed submission. A refused or uncertain
// send keeps the text and says which it was: the alternative — clearing on a
// void call, as the terminal's own paste helper does — silently loses a message
// whenever the iframe is not ready, and there is no way for the human to tell
// that happened. Nothing here ever retries by itself; an uncertain send may
// already have landed, and a second attempt would duplicate a turn.
function Composer({ host, paneID }: { host: string; paneID: string }) {
  const target = `${host}\u0000${paneID}`
  const ref = React.useRef<HTMLTextAreaElement>(null)
  const [text, setTextState] = React.useState(
    () => draftsByTarget.get(target) ?? ""
  )
  const [sending, setSending] = React.useState(false)
  const [notice, setNotice] = React.useState<{
    tone: "bad" | "warn"
    text: string
  } | null>(null)

  const setText = (value: string) => {
    setTextState(value)
    draftsByTarget.set(target, value)
  }

  const send = async () => {
    const body = text.trim()
    if (!body || sending) return
    setSending(true)
    setNotice(null)
    try {
      const res = await api.chatSend(host, paneID, body)
      if (res.outcome === "confirmed") {
        setText("")
      } else if (res.outcome === "refused") {
        // Nothing reached the pane, so the draft is exactly as unsent as it was.
        setNotice({ tone: "bad", text: res.detail || "the message was refused" })
      } else {
        setNotice({
          tone: "warn",
          text: `${res.detail || "delivery unconfirmed"} — check the terminal before sending again`,
        })
      }
    } catch (e) {
      setNotice({ tone: "bad", text: (e as Error).message })
    } finally {
      setSending(false)
      ref.current?.focus({ preventScroll: true })
    }
  }

  return (
    <div className="flex-none border-border border-t bg-card">
      {notice && (
        <div
          className={cn(
            "px-3 py-1.5 text-[11.5px]",
            notice.tone === "bad"
              ? "bg-destructive/8 text-destructive"
              : "bg-muted text-muted-foreground"
          )}
        >
          {notice.text}
        </div>
      )}
      <div className="flex items-end gap-2 px-2 py-2">
        <textarea
          ref={ref}
          rows={1}
          value={text}
          disabled={sending}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault()
              void send()
            }
          }}
          placeholder="Message the agent…"
          // 16px on touch: iOS zooms the page for a smaller field.
          className="max-h-32 min-h-9 flex-1 resize-none rounded-lg border border-input bg-background px-2.5 py-1.5 text-base outline-none placeholder:text-muted-foreground focus-visible:border-ring md:text-[13px]"
        />
        <button
          type="button"
          onClick={() => void send()}
          disabled={sending || !text.trim()}
          title="Send (Enter)"
          aria-label="Send"
          className="flex size-9 flex-none items-center justify-center rounded-lg bg-primary text-primary-foreground disabled:opacity-40"
        >
          {sending ? (
            <Loader2 className="size-4 animate-spin" />
          ) : (
            <Send className="size-4" />
          )}
        </button>
      </div>
    </div>
  )
}

export function ChatView({ onShowTerminal }: { onShowTerminal: () => void }) {
  const { activePaneID, panesRev, host } = useApp()
  // The view follows the focused pane, so the chat is always the session the
  // terminal beside it would be showing. Polled: the transcript is a file the
  // agent appends to, and nothing pushes it. Bounded by the server's caps.
  const { data, isLoading, error } = useQuery({
    queryKey: qk.chat(host ?? "", activePaneID ?? ""),
    queryFn: () => api.chat(activePaneID ?? undefined),
    refetchInterval: 2000,
    // panes_rev moves when herdr sees the pane change; the interval covers the
    // transcript growing within one state.
    refetchIntervalInBackground: false,
  })
  void panesRev

  const scrollRef = React.useRef<HTMLDivElement>(null)
  const stick = React.useRef(true)
  const rows = React.useMemo(() => groupRows(data?.items ?? []), [data?.items])

  // Follow the newest row, but stop fighting a reader who scrolled up. The
  // whole `rows` identity is the trigger, not its length: a poll that only
  // extends the last card's output must re-pin too.
  React.useLayoutEffect(() => {
    const el = scrollRef.current
    if (!el || rows.length === 0) return
    if (stick.current) el.scrollTop = el.scrollHeight
  }, [rows])

  const running = data?.running ?? false
  return (
    // vsurface: this overlay COVERS the terminal, and under the atmosphere
    // --background is a 62% wash — so herdr's tab bar, its status line and the
    // pane's own text read straight through the conversation. Same opt-out
    // FileViewer takes over the tree it opens from, and for the same reason:
    // a reading surface does not get to be translucent (index.css).
    <div className="vsurface flex h-full min-h-0 flex-col bg-background">
      <div className="flex flex-none items-center gap-2 border-border border-b px-2.5 py-1.5">
        <button
          type="button"
          onClick={onShowTerminal}
          className="flex shrink-0 items-center gap-1 rounded px-1.5 py-1 text-[12px] text-muted-foreground hover:bg-accent hover:text-foreground"
          title="Back to the terminal"
        >
          <ArrowUp className="size-3.5 -rotate-90" />
          Terminal
        </button>
        <span className="truncate text-[12.5px] text-foreground">
          {data?.title || data?.agent || "Chat"}
        </span>
        <span className="ml-auto flex shrink-0 items-center gap-2 text-[11px] text-muted-foreground">
          {running && (
            <span className="flex items-center gap-1 text-primary">
              <Loader2 className="size-3 animate-spin" />
              working
            </span>
          )}
          {data?.tokens ? <span>{Math.round(data.tokens / 1000)}k</span> : null}
        </span>
      </div>

      {/* Block layout with margins, not a flex column: a card is
          `overflow-hidden`, and as a flex item that zeroes its own min-height
          so the column squashes it to its border — a chat of 2px strips. */}
      <div
        ref={scrollRef}
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
        }}
        className="min-h-0 flex-1 space-y-2.5 overflow-y-auto px-3 py-3"
      >
        {data?.more && (
          <div className="text-center font-mono text-[11px] text-muted-foreground">
            earlier messages not loaded
          </div>
        )}
        {isLoading && (
          <div className="text-center text-[12px] text-muted-foreground">
            loading…
          </div>
        )}
        {error && (
          <div className="rounded-lg border border-destructive/40 bg-destructive/8 px-3 py-2 text-[12px] text-destructive">
            could not read the session: {(error as Error).message}
          </div>
        )}
        {!isLoading && !error && rows.length === 0 && (
          <div className="text-center text-[12px] text-muted-foreground">
            {data?.note || "No messages yet."}
          </div>
        )}
        {rows.map((row) => (
          <RowView
            key={row.kind === "group" ? row.id : row.item.id}
            row={row}
          />
        ))}
      </div>

      {/* The composer addresses the HOST and PANE this payload came from, not
          the tab's current host and not herdr's current focus: pane ids are
          unique per host only, so a submission aimed by "what is live right
          now" can land in a different machine's pane of the same id. Keyed by
          target so switching panes remounts onto that target's own draft
          rather than carrying one agent's sentence over to another. */}
      {data && (
        <Composer
          key={`${data.host}\u0000${data.pane_id}`}
          host={data.host}
          paneID={data.pane_id}
        />
      )}
    </div>
  )
}
