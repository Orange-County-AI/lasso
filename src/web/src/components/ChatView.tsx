import { useQuery } from "@tanstack/react-query"
import {
  AlertTriangle,
  ArrowUp,
  Check,
  ChevronRight,
  File as FileIcon,
  Globe,
  Image as ImageIcon,
  ListTodo,
  Loader2,
  Paperclip,
  Pencil,
  Search,
  Send,
  SquareTerminal,
  Users,
  Wrench,
  X,
} from "lucide-react"
import * as React from "react"
import { Markdown, resolveMarkdownSrc } from "@/components/Markdown"
import { Orb } from "@/components/ui/orb"
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

// mergeItems folds a freshly-read page into what is already on screen: rows that
// are already here are UPDATED in place (a tool card completing, an output
// growing) and rows that are new are APPENDED — the transcript is append-only,
// so anything unseen is newer than everything seen.
//
// Replacing the list instead, which is what a single-page view does, is what
// makes a conversation develop a hole: the live window is the last N kilobytes,
// so once the file grows past it the rows at its start fall out, and they are
// exactly the history someone scrolls up to find.
//
// A page is parsed INDEPENDENTLY of its neighbours, so an older page can hold a
// call whose result landed in a newer one and parse it as still running. Letting
// that version through would flip a finished card back to "running" for good, so
// a state regression is refused and the newer parse stands. That is also what
// makes the result independent of the order the pages arrived in, which is not
// something a fetch loop can promise.
function mergeItems(prev: ChatItem[], incoming: ChatItem[]): ChatItem[] {
  if (prev.length === 0) return incoming
  const at = new Map<string, number>()
  for (let i = 0; i < prev.length; i++) at.set(prev[i].id, i)
  let changed = false
  const out = prev.slice()
  for (const item of incoming) {
    const i = at.get(item.id)
    if (i === undefined) {
      at.set(item.id, out.length)
      out.push(item)
      changed = true
    } else if (out[i] !== item && !regresses(item, out[i])) {
      out[i] = item
      changed = true
    }
  }
  return changed ? out : prev
}

// regresses reports whether an incoming row says LESS than the one on screen: a
// tool that has finished cannot become a tool that is running again.
function regresses(incoming: ChatItem, current: ChatItem): boolean {
  const a = incoming.tool
  const b = current.tool
  return Boolean(a && b && b.state !== "running" && a.state === "running")
}

// Row is one rendered row: a lone item, or a run of calls collapsed into one card.
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
function ThinkingRow({
  text,
  resolveImage,
}: {
  text: string
  resolveImage?: (src: string | undefined) => string | undefined
}) {
  const [open, setOpen] = React.useState(false)
  // The preview is the plan text's opening words, so it has to be the SOURCE
  // line rather than a rendered one — markdown has no "first line" once it is
  // elements.
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
        <div className="mt-1.5 border-border border-l-2 pl-3">
          <div className="md-body md-chat md-chat-soft">
            <Markdown source={text} resolveImageSrc={resolveImage} />
          </div>
        </div>
      )}
    </div>
  )
}

function RowView({
  row,
  resolveImage,
}: {
  row: Row
  resolveImage?: (src: string | undefined) => string | undefined
}) {
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
        <ThinkingRow text={item.text ?? ""} resolveImage={resolveImage} />
      ) : (
        // Rendered, not printed: an agent writes headings, lists, tables and
        // fenced code, and showing the source of those is showing the wrong
        // thing. md-chat drops the document chrome (page padding, max-width,
        // centring) and steps the type down to the conversation's size; every
        // inner rule is shared with the file viewer's preview.
        <div className="md-body md-chat">
          <Markdown source={item.text ?? ""} resolveImageSrc={resolveImage} />
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
  const [attaching, setAttaching] = React.useState(false)
  const fileRef = React.useRef<HTMLInputElement>(null)
  const [notice, setNotice] = React.useState<{
    tone: "bad" | "warn"
    text: string
  } | null>(null)

  // Files attached to this message. Shown as chips — a thumbnail for an image,
  // a name for anything else — NOT spliced into the text as a path: the path is
  // what the agent needs, but a wall of them is not what the human meant to
  // write, and one pasted by accident has to be removable. The paths are
  // appended to the message when it is sent.
  const [attachments, setAttachments] = React.useState<
    { path: string; name: string; image: boolean }[]
  >([])

  const setText = (value: string) => {
    setTextState(value)
    draftsByTarget.set(target, value)
  }

  // A file — a pasted screenshot as much as one picked — goes to the machine the
  // SESSION is on, and the message that follows carries its path. That is the
  // contract the terminal's own paste has always had, for the same reason: the
  // agent reads the file from its own filesystem, and a browser-only blob URL
  // would be meaningless to it.
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
      setNotice({ tone: "bad", text: `attach failed: ${(e as Error).message}` })
    } finally {
      setAttaching(false)
    }
  }

  const send = async () => {
    const body = text.trim()
    const paths = attachments.map((a) => a.path)
    // An attachment on its own is a complete message ("here is the screenshot"),
    // so the text is not required when there is one.
    if ((!body && paths.length === 0) || sending) return
    // The paths go WITH the message, space-separated like the terminal's own
    // paste: the agent has to be told which file to open.
    const message = [body, ...paths].filter(Boolean).join(" ")
    setSending(true)
    setNotice(null)
    try {
      const res = await api.chatSend(host, paneID, message)
      if (res.outcome === "confirmed") {
        setText("")
        setAttachments([])
      } else if (res.outcome === "refused") {
        // Nothing reached the pane, so the draft is exactly as unsent as it was
        // — attachments included, since their paths were never delivered.
        setNotice({
          tone: "bad",
          text: res.detail || "the message was refused",
        })
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
      {attachments.length > 0 && (
        <div className="flex flex-wrap gap-1.5 px-2 pt-2">
          {attachments.map((a) => (
            <span
              key={a.path}
              className="flex max-w-[14rem] items-center gap-1.5 rounded-lg border border-border bg-card py-1 pr-1 pl-1.5 text-[11.5px] text-muted-foreground"
              // The chip shows the NAME; the path is what is actually sent, and
              // the only place it needs to be legible is a hover.
              title={a.path}
            >
              {a.image ? (
                <img
                  src={api.fileURL(a.path, host)}
                  alt=""
                  className="size-6 shrink-0 rounded object-cover"
                />
              ) : (
                <FileIcon className="size-3.5 shrink-0" />
              )}
              <span className="truncate">{a.name}</span>
              <button
                type="button"
                onClick={() =>
                  setAttachments((prev) => prev.filter((x) => x.path !== a.path))
                }
                // A mis-paste has to be undoable without clearing the message.
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
      <div className="flex items-end gap-2 px-2 py-2">
        <input
          ref={fileRef}
          type="file"
          multiple
          className="hidden"
          onChange={(e) => {
            void attachFiles(Array.from(e.target.files ?? []))
            // Clearing lets the same file be attached twice in a row.
            e.target.value = ""
          }}
        />
        <button
          type="button"
          onClick={() => fileRef.current?.click()}
          disabled={attaching || sending}
          title="Attach a file and insert its path"
          aria-label="Attach a file"
          className="flex size-9 flex-none items-center justify-center rounded-lg border border-input text-muted-foreground disabled:opacity-40"
        >
          {attaching ? (
            <Orb state="working" px={16} />
          ) : (
            <Paperclip className="size-4" />
          )}
        </button>
        <textarea
          ref={ref}
          rows={1}
          value={text}
          disabled={sending}
          onChange={(e) => setText(e.target.value)}
          onPaste={(e) => {
            // A clipboard routinely holds text AND a file (copying an image off
            // a page carries its URL too). Text wins, exactly as it does for the
            // terminal's own paste: someone pasting a screenshot of their own
            // text means the text.
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
          placeholder="Message the agent…"
          // 16px on touch: iOS zooms the page for a smaller field.
          className="max-h-32 min-h-9 flex-1 resize-none rounded-lg border border-input bg-background px-2.5 py-1.5 text-base outline-none placeholder:text-muted-foreground focus-visible:border-ring md:text-[13px]"
        />
        <button
          type="button"
          onClick={() => void send()}
          disabled={sending || (!text.trim() && attachments.length === 0)}
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

  // Everything read so far, TAGGED with the target it was read from (host +
  // pane) and the transcript that answered. Tagged rather than cleared in an
  // effect, because the answer can arrive after the target has moved: a page
  // request still in flight would otherwise prepend one conversation's rows into
  // another's. `transcript` is part of the identity too, and is compared with
  // its absent value: a pane that starts a NEW session, and a pane that has none
  // at all (a shell), are both different conversations from the one before.
  type Accumulated = {
    key: string
    transcript: string
    olderStart: number | null
    items: ChatItem[]
  }
  const [acc, setAcc] = React.useState<Accumulated>({
    key: "",
    transcript: "",
    olderStart: null,
    items: [],
  })
  const target = data ? `${data.host}\u0000${data.pane_id}` : ""

  const [loadingOlder, setLoadingOlder] = React.useState(false)
  // Height to hold the reader's place by when a page lands above them.
  const restoreHeight = React.useRef<number | null>(null)

  React.useEffect(() => {
    if (!data || !target) return
    // `?? []` because a payload with no rows is a payload about a pane with no
    // session — a reason to show the note, never a reason to crash the view.
    const incoming = data.items ?? []
    setAcc((prev) => {
      const path = data.path ?? ""
      if (prev.key !== target || prev.transcript !== path) {
        stick.current = true
        return { key: target, transcript: path, olderStart: null, items: incoming }
      }
      return { ...prev, items: mergeItems(prev.items, incoming) }
    })
    // `data` alone: `incoming` is derived from it, and listing a value declared
    // inside the effect is not a dependency, it is a name error.
  }, [data, target])

  const items = acc.items
  const hasMore =
    acc.olderStart === null ? (data?.more ?? false) : acc.olderStart > 0

  const loadOlder = React.useCallback(async () => {
    const before = acc.olderStart ?? data?.start_offset
    if (!before || before <= 0 || loadingOlder || !data) return
    // Captured, not read later: the response is only this conversation's if the
    // view is still showing it when the page lands.
    const key = acc.key
    const pane = data.pane_id
    const pageHost = data.host
    setLoadingOlder(true)
    // Measure before the prepend: the list is about to grow above the viewport,
    // and the reader is looking at the bottom of it.
    restoreHeight.current = scrollRef.current?.scrollHeight ?? null
    try {
      // The page belongs to the transcript on screen, so it is addressed to that
      // record's host — not to whichever host this tab has moved to since.
      const page = await api.chat(pane, before, pageHost)
      setAcc((prev) => {
        if (prev.key !== key) return prev
        const have = new Set(prev.items.map((it) => it.id))
        return {
          ...prev,
          items: [
            ...(page.items ?? []).filter((it) => !have.has(it.id)),
            ...prev.items,
          ],
          // A page with nothing older to give must not leave the cursor where it
          // was, or the loader would ask for the same empty page forever.
          olderStart: page.start_offset,
        }
      })
    } catch {
      // A page that will not load leaves the reader where they were; the loader
      // goes away and scrolling up tries again.
      restoreHeight.current = null
    } finally {
      setLoadingOlder(false)
    }
  }, [acc.key, acc.olderStart, data, loadingOlder])

  const rows = React.useMemo(() => groupRows(items), [items])

  // Follow the newest row, hold the reader's place when a page is prepended,
  // and stop fighting either of them when they have scrolled up. `rows` as the
  // trigger rather than its length: a poll that only extends the last card's
  // output has to re-pin too.
  React.useLayoutEffect(() => {
    const el = scrollRef.current
    if (!el || rows.length === 0) return
    const restore = restoreHeight.current
    if (restore != null) {
      el.scrollTop += el.scrollHeight - restore
      restoreHeight.current = null
      return
    }
    if (stick.current) el.scrollTop = el.scrollHeight
  }, [rows])

  // A page shorter than the viewport leaves scrollTop at 0 and fires no further
  // scroll, so a reader parked at the top would have to nudge it. Keep filling
  // until the viewport is covered or the history runs out.
  React.useEffect(() => {
    const el = scrollRef.current
    if (el && hasMore && !loadingOlder && el.scrollTop < 120) void loadOlder()
  }, [hasMore, loadingOlder, loadOlder])

  const running = data?.running ?? false

  // A relative path in an agent's prose is relative to WHERE IT IS WORKING, on
  // the machine it is working on — so resolve it the same way the file viewer
  // resolves one in a README, through /api/file on the session's own host. A
  // stable identity matters: it decides the markdown components map, and a new
  // one per render would rebuild every diagram on every poll.
  const cwd = data?.cwd
  const dataHost = data?.host
  const resolveImage = React.useCallback(
    (src: string | undefined) =>
      cwd ? resolveMarkdownSrc(src, `${cwd}/`, dataHost ?? null) : src,
    [cwd, dataHost]
  )

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
          // Reaching the top loads the page above it, which is the whole
          // gesture: a conversation reads backwards by dragging, not by a
          // button.
          if (el.scrollTop < 120 && hasMore && !loadingOlder) void loadOlder()
        }}
        className="min-h-0 flex-1 space-y-2.5 overflow-y-auto px-3 py-3"
      >
        {loadingOlder && (
          <div className="flex items-center justify-center gap-2 py-1 text-[12px] text-muted-foreground">
            <Orb state="working" px={16} />
            loading earlier…
          </div>
        )}
        {isLoading && (
          <div className="flex items-center justify-center gap-2 py-1 text-[12px] text-muted-foreground">
            <Orb state="working" px={16} />
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
            resolveImage={resolveImage}
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
