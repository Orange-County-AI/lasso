import { useQuery } from "@tanstack/react-query"
import {
  AlertTriangle,
  Bot,
  Check,
  ChevronDown,
  ChevronRight,
  File as FileIcon,
  Globe,
  Image as ImageIcon,
  ListTodo,
  Loader2,
  Paperclip,
  Pencil,
  Plus,
  Search,
  Send,
  SquareTerminal,
  Users,
  Wrench,
  X,
} from "lucide-react"
import * as React from "react"
import { AgentPicker } from "@/components/AgentPicker"
import { Markdown, resolveMarkdownSrc } from "@/components/Markdown"
import { NO_AUTOCORRECT } from "@/components/ui/input"
import { Orb } from "@/components/ui/orb"
import { api, type ChatDiffLine, type ChatItem, type ChatTool } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import {
  newestUserID,
  type QueuedEcho,
  reconcileQueued,
} from "@/lib/chat-queue"
import { qk } from "@/lib/query"
import { cn } from "@/lib/utils"

// The agent session as a conversation: what the terminal shows, rendered so a
// phone can read it without a 40-column TUI. The shape follows Moshi's chat
// view — a tool result grouped into a card, thinking folded away, a diff
// excerpted rather than dumped — but the palette is lasso's own, so it inherits
// whatever theme (and backdrop) the rest of the app is wearing.
//
// Input stays the real TUI, deliberately: the composer pastes into the herdr
// terminal and presses Enter, and an ask is answered by typing its dialog's own
// keystrokes (AskCard → POST /api/chat/answer). Nothing here answers an agent
// out of band — every write lands in the pane, so a card always says what the
// harness actually received.

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
          // biome-ignore lint/suspicious/noArrayIndexKey: a hunk's lines are positional and never reorder; the index is the identity, and a repeated line ("  }") has no other one.
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

// PickMark is the radio/checkbox of one option, drawn with borders rather than
// an icon font: lasso's chrome separates by border and a brightness step, and
// the mark is the only thing on the card that has to read at a glance.
function PickMark({ multi, chosen }: { multi: boolean; chosen: boolean }) {
  return (
    <span
      aria-hidden
      className={cn(
        "mt-[3px] flex size-3.5 shrink-0 items-center justify-center border",
        multi ? "rounded-[4px]" : "rounded-full",
        chosen
          ? "border-primary bg-primary text-primary-foreground"
          : "border-muted-foreground/50"
      )}
    >
      {chosen && <Check className="size-2.5" strokeWidth={3} />}
    </span>
  )
}

// AskCard is the one card that is not a record of work but a request to the
// READER: the agent has stopped and will not continue until one of these options
// is chosen. So it renders the dialog the terminal is showing — the question in
// full, every option with its description, the agent's own recommendation
// marked — and a tap answers it, by typing that selection into the dialog the
// agent asked in (see serveChatAnswer).
//
// What a tap sends is an option INDEX, never the rendered label: the dialog
// selects whichever row its cursor is on, and a label would have to match
// through however that harness decorates it ("… (Recommended)").
function AskCard({
  tool,
  host,
  paneID,
}: {
  tool: ChatTool
  host: string
  paneID: string
}) {
  const questions = tool.ask?.questions ?? []
  // Which option of each question the reader has picked. Local until it is
  // sent: the transcript cannot know about a choice that has not reached the
  // pane yet, and the harness will not either until it is submitted.
  const [picks, setPicks] = React.useState<number[][]>(() =>
    questions.map(() => [])
  )
  const [expanded, setExpanded] = React.useState<number[]>([])
  const [sending, setSending] = React.useState(false)
  const [sent, setSent] = React.useState(false)
  const [notice, setNotice] = React.useState<string | null>(null)

  if (!tool.ask) return null

  const running = tool.state === "running"
  // A single question that takes a single answer IS the tap: that dialog submits
  // on the Enter it takes, so a separate confirmation would be a whole extra
  // gesture for nothing. Every other shape is a form, because the dialog walks
  // its questions in order and answering them one request at a time would race
  // the human's own taps.
  const oneTap = questions.length === 1 && !questions[0]?.multi
  // A question with no options is the free-text kind: the terminal is where it
  // is answered, so the card does not offer a form for it.
  const hasOptions = questions.some((q) => q.options.length > 0)
  const answerable = running && !sent && !sending && hasOptions
  const ready = questions.every((_, i) => (picks[i]?.length ?? 0) > 0)

  const send = async (answers: number[][]) => {
    setSending(true)
    setNotice(null)
    try {
      const res = await api.chatAnswer(
        host,
        paneID,
        // What the card was showing — the server checks it is still the
        // question on that pane before it types a single key. The option
        // labels ride along because a short pane scrolls the question itself
        // off the top while its options stay on screen.
        questions[0]?.question ?? "",
        (questions[0]?.options ?? []).map((o) => o.label),
        answers.map((selected, i) => ({
          selected,
          multi: Boolean(questions[i]?.multi),
          // claude's dialog is left by walking off the end of its option list,
          // so it needs the length, which the indexes alone do not give.
          options: questions[i]?.options.length ?? 0,
        }))
      )
      if (res.outcome === "sent") setSent(true)
      else setNotice(res.detail ?? "the terminal did not take the answer")
    } catch (e) {
      setNotice((e as Error).message)
    } finally {
      setSending(false)
    }
  }

  const pick = (qi: number, oi: number) => {
    if (!answerable) return
    const q = questions[qi]
    if (!q) return
    const next = picks.map((p, i) => {
      if (i !== qi) return p
      if (!q.multi) return [oi]
      return p.includes(oi)
        ? p.filter((x) => x !== oi)
        : [...p, oi].sort((a, b) => a - b)
    })
    setPicks(next)
    if (oneTap) void send(next)
  }

  // What reads as chosen. While the ask is running those are the reader's own
  // picks — the recorded answer cannot exist yet, which is the whole point of
  // the card being interactive. Once the tool has returned, the record wins:
  // that is what the agent actually received.
  const chosenLabels = (qi: number): Set<string> => {
    const q = questions[qi]
    if (!running) return new Set(q.selected ?? [])
    const out = new Set<string>()
    for (const oi of picks[qi] ?? []) {
      const opt = q.options[oi]
      if (opt) out.add(opt.label)
    }
    return out
  }

  return (
    <div
      className={cn(
        "overflow-hidden rounded-lg border bg-card",
        answerable ? "border-primary/40" : "border-border"
      )}
    >
      <div className="flex items-center gap-2 px-2.5 py-2">
        <span className="shrink-0 font-semibold text-[12.5px] text-foreground">
          {tool.title}
        </span>
        {questions.length > 1 && (
          <span className="shrink-0 rounded bg-muted px-1.5 py-px font-mono text-[10px] text-muted-foreground">
            {questions.length}
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
      </div>
      <div className="flex flex-col gap-3 border-border/60 border-t px-2.5 py-2.5">
        {questions.map((q, qi) => {
          const chosen = chosenLabels(qi)
          const clipped = q.question.length > 320 && !expanded.includes(qi)
          return (
            // biome-ignore lint/suspicious/noArrayIndexKey: a question list is positional and fixed for the ask's life; its text is not unique (a harness may ask the same question twice).
            <div key={qi} className="flex flex-col gap-1.5">
              {q.header && (
                <span className="self-start rounded bg-muted px-1.5 py-px font-mono text-[10px] text-muted-foreground uppercase tracking-wide">
                  {q.header}
                </span>
              )}
              <div
                className={cn(
                  "whitespace-pre-wrap break-words text-[13.5px] text-foreground leading-snug",
                  clipped && "line-clamp-6"
                )}
              >
                {q.question}
              </div>
              {(q.question.length > 320 || expanded.includes(qi)) && (
                <button
                  type="button"
                  onClick={() =>
                    setExpanded((prev) =>
                      prev.includes(qi)
                        ? prev.filter((x) => x !== qi)
                        : [...prev, qi]
                    )
                  }
                  className="self-start text-[11.5px] text-muted-foreground hover:text-foreground"
                >
                  {clipped ? "Show the full question" : "Show less"}
                </button>
              )}
              <div className="mt-0.5 flex flex-col gap-1.5">
                {q.options.map((o, oi) => {
                  const isChosen = chosen.has(o.label)
                  return (
                    <button
                      key={o.label}
                      type="button"
                      disabled={!answerable}
                      aria-pressed={isChosen}
                      onClick={() => pick(qi, oi)}
                      className={cn(
                        "flex w-full items-start gap-2 rounded-lg border px-2.5 py-2 text-left",
                        isChosen
                          ? "border-primary/50 bg-primary/8"
                          : "border-border",
                        answerable && !isChosen && "hover:bg-accent/40"
                      )}
                    >
                      <PickMark multi={Boolean(q.multi)} chosen={isChosen} />
                      <span className="min-w-0 flex-1">
                        <span className="flex flex-wrap items-baseline gap-x-1.5 gap-y-0.5">
                          <span
                            className={cn(
                              "text-[13px] text-foreground leading-snug",
                              isChosen && "font-semibold"
                            )}
                          >
                            {o.label}
                          </span>
                          {q.recommended === oi && (
                            <span className="shrink-0 rounded bg-muted px-1 py-px font-mono text-[9.5px] text-muted-foreground uppercase tracking-wide">
                              Recommended
                            </span>
                          )}
                        </span>
                        {o.description && (
                          <span className="mt-0.5 block text-[12px] text-muted-foreground leading-snug">
                            {o.description}
                          </span>
                        )}
                        {/* The preview is the consequence of a pick — a diff, a
                            command — so it appears once one is made, and on an
                            answered card it shows what was chosen rather than
                            what was merely offered. */}
                        {o.preview && isChosen && (
                          <span className="mt-1.5 block overflow-x-auto whitespace-pre rounded border border-border/60 bg-background/60 px-2 py-1.5 font-mono text-[11px] text-muted-foreground leading-[1.6]">
                            {o.preview}
                          </span>
                        )}
                      </span>
                    </button>
                  )
                })}
                {q.options.length === 0 && (
                  <div className="text-[11.5px] text-muted-foreground">
                    This question has no options — answer it in the terminal.
                  </div>
                )}
              </div>
              {q.custom && (
                <div className="text-[12px] text-muted-foreground">
                  Answer: <span className="text-foreground">{q.custom}</span>
                </div>
              )}
            </div>
          )
        })}
        {/* A result whose answers lasso could not read keeps its text: showing
            nothing would claim the agent was never answered. */}
        {!running && tool.output && (
          <pre className="overflow-x-auto whitespace-pre-wrap break-words rounded border border-border/60 bg-background/60 px-2 py-1.5 font-mono text-[11px] text-muted-foreground leading-[1.6]">
            {tool.output}
          </pre>
        )}
        {/* An ask that was cancelled or interrupted says so here: the options
            above are then a record of what was offered, not a form. */}
        {tool.error && (
          <div className="rounded border border-destructive/40 bg-destructive/8 px-2 py-1.5 font-mono text-[11.5px] text-destructive">
            {tool.error}
          </div>
        )}
      </div>
      {notice && (
        <div className="border-border/60 border-t bg-destructive/8 px-2.5 py-2 text-[11.5px] text-destructive">
          {notice}
        </div>
      )}
      {/* Only while the ask is genuinely still outstanding: once the harness
          has recorded an answer, "waiting for the agent" is a lie about a
          question that is already settled. */}
      {(sending || (sent && running)) && !notice && (
        <div className="border-border/60 border-t px-2.5 py-2 text-[11.5px] text-muted-foreground">
          {sending
            ? "Answering in the terminal…"
            : "Answer sent — waiting for the agent…"}
        </div>
      )}
      {!oneTap && running && hasOptions && (
        <div className="flex items-center gap-2 border-border/60 border-t px-2.5 py-2">
          <button
            type="button"
            disabled={!ready || sending}
            onClick={() => void send(picks)}
            className="flex shrink-0 items-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 font-medium text-[12.5px] text-primary-foreground disabled:opacity-40"
          >
            {sending ? (
              <Orb state="working" px={14} on="accent" />
            ) : (
              <Send className="size-3.5" />
            )}
            {sending ? "Sending…" : "Send answers"}
          </button>
          <span className="min-w-0 truncate text-[11px] text-muted-foreground">
            {ready
              ? "Answers the dialog in the terminal"
              : "Pick one per question"}
          </span>
        </div>
      )}
    </div>
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
  host,
  paneID,
}: {
  row: Row
  resolveImage?: (src: string | undefined) => string | undefined
  // Where an answer to an ask must be delivered: the host and pane this
  // payload came from, not the tab's current host and not herdr's focus.
  host: string
  paneID: string
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
      if (!item.tool) return null
      // An ask is the one call that is a question rather than a record, so it
      // gets its own card — and it is never folded into a group: a burst of
      // reads behind it must not hide the thing the agent is waiting on.
      return item.tool.ask ? (
        <AskCard tool={item.tool} host={host} paneID={paneID} />
      ) : (
        <ToolCard tool={item.tool} />
      )
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
function Composer({
  host,
  paneID,
  newestUser,
  onQueued,
}: {
  host: string
  paneID: string
  // The transcript's newest user turn as of this render, captured when a send
  // STARTS: a poll can land mid-send and see the row this send just created, and
  // a snapshot taken after that would never match again.
  newestUser: string
  onQueued: (text: string, after: string) => void
}) {
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

  // Grow with the draft, up to the cap the class sets. A textarea stays one row
  // tall and scrolls its own text instead, which turns a paragraph into a
  // one-line window — and the message being written is the one thing that has to
  // be readable while it is typed. The cap is read off the element rather than
  // repeated here, so the two cannot drift apart.
  React.useLayoutEffect(() => {
    const el = ref.current
    // `text` is the trigger and the guard at once: the height has to be
    // re-measured after every change, and the measurement belongs to the value
    // the element actually holds.
    if (!el || el.value !== text) return
    el.style.height = "auto"
    const cap = Number.parseFloat(getComputedStyle(el).maxHeight) || 0
    const full = el.scrollHeight
    el.style.height = `${cap > 0 ? Math.min(full, cap) : full}px`
    el.style.overflowY = cap > 0 && full > cap ? "auto" : "hidden"
  }, [text])

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
    // Captured before the round trip: the transcript this send is about to add
    // to is the one on screen NOW.
    const after = newestUser
    setSending(true)
    setNotice(null)
    try {
      const res = await api.chatSend(host, paneID, message)
      if (res.outcome === "confirmed") {
        setText("")
        setAttachments([])
        // The pane has it and the transcript does not yet: the echo is the only
        // thing standing between "sent" and the row appearing.
        onQueued(message, after)
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
                  setAttachments((prev) =>
                    prev.filter((x) => x.path !== a.path)
                  )
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
      <div className="flex items-stretch gap-2 px-4 pt-2 pb-1">
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
        {/* Three columns: attach, input, submit. Both buttons are square and
            pinned to the input's top edge, so the input is the only tall thing
            on the row and neither button sits under the thumb's resting corner
            — where a stray tap lands on the wrong one. */}
        <button
          type="button"
          onClick={() => fileRef.current?.click()}
          disabled={attaching || sending}
          title="Attach a file and insert its path"
          aria-label="Attach a file"
          className="flex size-10 shrink-0 items-center justify-center self-start rounded-lg border border-input text-muted-foreground disabled:opacity-40"
        >
          {attaching ? (
            <Orb state="working" px={16} />
          ) : (
            <Paperclip className="size-4" />
          )}
        </button>
        <textarea
          ref={ref}
          {...NO_AUTOCORRECT}
          // Three rows, not one: a prompt is a paragraph often enough that a
          // single-line box makes people write blind, and this one grows from
          // here as the draft fills (see the measure effect above).
          rows={3}
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
          className="max-h-40 min-w-0 flex-1 resize-none rounded-lg border border-input bg-background px-2.5 py-1.5 text-base outline-none placeholder:text-muted-foreground focus-visible:border-ring md:text-[13px]"
        />
        <button
          type="button"
          onClick={() => void send()}
          disabled={sending || (!text.trim() && attachments.length === 0)}
          title="Send (Enter)"
          aria-label="Send"
          className="flex size-10 shrink-0 items-center justify-center self-start rounded-lg bg-primary text-primary-foreground disabled:opacity-40"
        >
          {sending ? (
            // on="accent": this button is filled with the theme's accent, and
            // the orb's own scheme detection reads the DOCUMENT, which is the
            // wrong surface to choose ink for (see ui/orb.tsx).
            <Orb state="working" px={20} on="accent" />
          ) : (
            <Send className="size-5" />
          )}
        </button>
      </div>
    </div>
  )
}

export function ChatView({
  onShowTerminal,
  onNewAgent,
  className,
}: {
  onShowTerminal: () => void
  // The chat's own creator, for the widths where the footer's New button is not
  // on screen. App decides what it opens; this only says where it is (see the
  // header).
  onNewAgent: () => void
  // Merged onto the root. The view is a flex ROW's second child whenever the
  // agent sidebar is beside it (see App.tsx), and it has to be told to take the
  // width that is left over rather than its content's own.
  className?: string
}) {
  const { activePaneID, panesRev, host } = useApp()
  // The agent sheet, for the widths that have no docked column to put beside the
  // chat (see AgentPicker). Per-view state: the sheet is a transient way to pick
  // a conversation, so it does not survive leaving the chat.
  const [pickerOpen, setPickerOpen] = React.useState(false)
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
  // The reader's own position, as state rather than only the `stick` ref, because
  // the header draws from it: the inline indicator sits at the END of the
  // conversation, so a reader who has scrolled up cannot see it, and that is
  // exactly when the header has to say the same thing. See `atBottom`'s use.
  const [atBottom, setAtBottom] = React.useState(true)

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
        return {
          key: target,
          transcript: path,
          olderStart: null,
          items: incoming,
        }
      }
      return { ...prev, items: mergeItems(prev.items, incoming) }
    })
    // `data` alone: `incoming` is derived from it, and listing a value declared
    // inside the effect is not a dependency, it is a name error.
  }, [data, target])

  const items = acc.items
  const hasMore =
    acc.olderStart === null ? (data?.more ?? false) : acc.olderStart > 0

  // Messages the pane has ACCEPTED but whose row the transcript has not written
  // yet. The composer clears its draft the moment a send is confirmed, and the
  // row exists only once the harness records it — so without an echo the message
  // vanishes for the seconds in between, which reads as "my prompt didn't send".
  //
  // Reconciled on the transcript's newest USER TURN rather than on the text (see
  // newestUserID). The transcript is append-only, so once that id is no longer
  // the one captured as the send started, a message has landed. The echoes that
  // remain are re-pointed at it, so sending twice before either lands retires
  // them one apiece instead of both at once.
  const [queued, setQueued] = React.useState<QueuedEcho[]>([])
  // A queued message has no transcript id to be keyed by — that is the whole
  // point of it — so the tab numbers them.
  const echoSeq = React.useRef(0)
  const newestUser = React.useMemo(() => newestUserID(items), [items])
  React.useEffect(() => {
    if (!target) return
    setQueued((prev) => reconcileQueued(prev, target, newestUser))
  }, [newestUser, target])

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

  // Back to the newest row, for a reader who has scrolled up. `stick` is what
  // makes it STAY there: the poll's re-pin, a landed page and the composer
  // growing all consult it, so a jump that left it false would be undone by the
  // next frame of conversation.
  const jumpToBottom = React.useCallback(() => {
    const el = scrollRef.current
    if (!el) return
    stick.current = true
    setAtBottom(true)
    el.scrollTo({ top: el.scrollHeight, behavior: "smooth" })
  }, [])

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
    if (stick.current) {
      el.scrollTop = el.scrollHeight
      // Set here rather than waiting for the scroll event this queues: the
      // header must not flash an indicator for a frame it is at the bottom for.
      setAtBottom(true)
    }
  }, [rows])

  // The transcript's viewport shrinks when the composer grows into a longer
  // draft, and when a phone's keyboard opens over it. A reader who is at the
  // bottom stays there: without this, typing a paragraph scrolls the newest
  // rows out of sight, and they are the ones being answered.
  React.useEffect(() => {
    const el = scrollRef.current
    if (!el || typeof ResizeObserver === "undefined") return
    const ro = new ResizeObserver(() => {
      if (stick.current) el.scrollTop = el.scrollHeight
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // A page shorter than the viewport leaves scrollTop at 0 and fires no further
  // scroll, so a reader parked at the top would have to nudge it. Keep filling
  // until the viewport is covered or the history runs out.
  React.useEffect(() => {
    const el = scrollRef.current
    if (el && hasMore && !loadingOlder && el.scrollTop < 120) void loadOlder()
  }, [hasMore, loadingOlder, loadOlder])

  const running = data?.running ?? false
  // An empty view that is a WAIT rather than a verdict (the agent is booting, or
  // its log has not been written yet) — the server's call, not an inference from
  // a clock: see the notes in chatview.go.
  const starting = data?.starting ?? false

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
    <div
      className={cn(
        // relative: the agent sheet is an overlay INSIDE this view, so it covers
        // the conversation and the composer rather than the whole window.
        "vsurface relative flex h-full min-h-0 flex-col bg-background",
        className
      )}
    >
      <div className="flex flex-none items-center gap-2 border-border border-b px-2.5 py-1.5">
        <span className="truncate text-[12.5px] text-foreground">
          {data?.title || data?.agent || "Chat"}
        </span>
        <span className="ml-auto flex shrink-0 items-center gap-2 text-[11px] text-muted-foreground">
          {/* Only when the reader is NOT at the bottom. At the bottom the
              conversation carries the indicator itself, at the point the next
              message will land, which is where someone waiting on output is
              already looking — the header would just be the same orb twice. */}
          {running && !atBottom && (
            <span className="flex items-center gap-1 text-primary">
              <Orb state="working" px={14} />
              working
            </span>
          )}
          {data?.tokens ? <span>{Math.round(data.tokens / 1000)}k</span> : null}
        </span>
        {/* The agent list, for the widths where the footer's toggle does not
            exist and the docked column hides itself — this is a phone's (and a
            small tablet's) only way to pick which conversation is on screen, and
            the chat input dial already reaches everything else the footer
            carries. Hidden at md+ because the same list is docked there.
            The glyph is an agent, not a layout: what it opens is agents to
            select, and whether they land in a stack or a grid is the screen's
            business, not the button's. */}
        <button
          type="button"
          onClick={() => setPickerOpen(true)}
          aria-haspopup="dialog"
          aria-expanded={pickerOpen}
          title="All agents"
          aria-label="All agents"
          className="flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground hover:bg-accent hover:text-foreground md:hidden"
        >
          <Bot className="size-4" />
        </button>
        {/* Back to the terminal, at EVERY width: it is the button that used to sit
            at the left of this header, so the chat keeps one route back wherever
            it is read (the footer's own toggle is md+ only, and on a phone that
            toggle is the input dial instead). */}
        <button
          type="button"
          onClick={onShowTerminal}
          title="Back to the terminal"
          aria-label="Back to the terminal"
          className="flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <SquareTerminal className="size-4" />
        </button>
        {/* The chat's own creator, at the far right of the header and mobile
            only: below md there is no footer and so no New button, while at md+
            the footer's is the same entry point (both open the same dialog,
            scoped by the view — see App's agentsOnly). It sits at the END
            rather than ahead of the title because that is the corner a thumb
            reaches without moving the hand. */}
        <button
          type="button"
          onClick={onNewAgent}
          aria-haspopup="dialog"
          title="New agent"
          aria-label="New agent"
          className="flex size-7 shrink-0 items-center justify-center rounded-lg text-muted-foreground hover:bg-accent hover:text-foreground md:hidden"
        >
          <Plus className="size-4" />
        </button>
      </div>

      {/* The viewport and its corner control share a positioning context:
          the chevron is ABSOLUTE inside it, so it neither scrolls away with
          the rows nor takes a row of its own at the end of them (a control
          that added height would move the very edge it offers to reach). */}
      <div className="relative flex min-h-0 flex-1 flex-col">
        {/* Block layout with margins, not a flex column: a card is
            `overflow-hidden`, and as a flex item that zeroes its own min-height
            so the column squashes it to its border — a chat of 2px strips. */}
        <div
          ref={scrollRef}
          onScroll={(e) => {
            const el = e.currentTarget
            const near = el.scrollHeight - el.scrollTop - el.clientHeight < 40
            stick.current = near
            setAtBottom(near)
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
          {/* An empty view says why — and a pane still coming up says it as
              WORK: a newly created agent has no session or log for its first
              seconds, and a sentence about what is missing reads as a dead end
              for a pane the reader just asked lasso to create. `starting` is the
              server's answer to "is this a wait or a verdict" (see the notes in
              chatview.go), so the orb goes on exactly the waits. */}
          {!isLoading && !error && rows.length === 0 && !running && (
            <div
              className={cn(
                "text-[12px] text-muted-foreground",
                starting
                  ? "flex items-center justify-center gap-2 py-1"
                  : "text-center"
              )}
            >
              {starting && <Orb state="working" px={16} />}
              {data?.note || "No messages yet."}
            </div>
          )}
          {rows.map((row) => (
            <RowView
              key={row.kind === "group" ? row.id : row.item.id}
              row={row}
              resolveImage={resolveImage}
              host={data?.host ?? ""}
              paneID={data?.pane_id ?? ""}
            />
          ))}
          {/* The message the pane has taken but the transcript has not written
            yet, drawn exactly where its row will appear and in the user's own
            bubble — a lighter, dashed one, so it reads as "on its way" rather
            than as a message the session recorded. The working indicator below
            it is where the agent's answer will land, which is the order the two
            things actually happen in. */}
          {queued
            .filter((q) => q.target === target)
            .map((q) => (
              <div key={q.id} className="flex justify-end">
                <div className="max-w-[85%] rounded-xl rounded-br-sm border border-primary/20 border-dashed bg-primary/8 px-3 py-2 text-[13.5px] text-foreground leading-snug opacity-60">
                  <div className="whitespace-pre-wrap break-words">
                    {q.text}
                  </div>
                  <div className="mt-1 text-[10.5px] text-muted-foreground">
                    queued…
                  </div>
                </div>
              </div>
            ))}
          {/* Where the agent's NEXT message will land. The header says the same
            thing, but a view waiting on output should say it at the point the
            output will appear — and that gap is seconds long every turn, since
            the harness writes a message only once it is complete (see
            src/chatview.go). Left-aligned like the agent rows it precedes, not
            centred like the page-loading rows above: this one is a placeholder
            in the conversation, not a note about the fetch. */}
          {!isLoading && !error && running && (
            <div className="flex items-center gap-2 text-[12px] text-muted-foreground">
              <Orb state="working" px={16} />
              working…
            </div>
          )}
        </div>
        {/* Only when the newest row is out of sight, which is the only time
            this says anything: a reader at the bottom is already looking at
            where the next message lands. It is also the state the header names
            ("working" beside the orb), said where the gesture is. */}
        {!atBottom && rows.length > 0 && (
          <button
            type="button"
            onClick={jumpToBottom}
            title="Jump to the newest message"
            aria-label="Jump to the newest message"
            className="absolute right-3 bottom-3 flex size-8 items-center justify-center rounded-full border border-border bg-card text-muted-foreground shadow-sm transition-colors hover:bg-accent hover:text-foreground"
          >
            <ChevronDown className="size-4" />
          </button>
        )}
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
          newestUser={newestUser}
          onQueued={(text, after) =>
            setQueued((prev) => [
              ...prev,
              { id: echoSeq.current++, target, text, after },
            ])
          }
        />
      )}

      {/* Last, so it covers the header, the transcript and the composer alike —
          the whole view is what a sheet replaces while it is open. */}
      {pickerOpen && <AgentPicker onClose={() => setPickerOpen(false)} />}
    </div>
  )
}
