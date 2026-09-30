import { ArrowLeft, ArrowRight, X } from "lucide-react"
import * as React from "react"
import { Button } from "@/components/ui/button"
import { tourAnchor } from "@/lib/onboarding"
import { cn } from "@/lib/utils"

// What App does to the screen before a step is shown, so the step's anchor is
// actually there to point at: "terminal" puts the left column back on the
// terminal (the chat and grid overlays cover it), "sidebar" opens the right
// sidebar on its Files tab.
export type TourPrepare = "terminal" | "sidebar"

export interface TourStep {
  id: string
  title: string
  body: React.ReactNode
  // data-tour anchor to spotlight. None = a centered card.
  target?: string
  prepare?: TourPrepare
  // The spotlit control stays clickable, so the human can try it while the
  // card explains it. Only for controls that change a view in place: one that
  // opens a modal would put that modal under the tour's scrim.
  tryIt?: boolean
  // Width gate. The footer (and every anchor in it) is md+ only, and a phone
  // carries the same commands on the input dial inside the terminal's iframe,
  // which the tour cannot reach into.
  only?: "desktop" | "mobile"
}

const Kbd = ({ children }: { children: React.ReactNode }) => (
  <kbd className="rounded border border-border bg-muted px-1 py-px font-mono text-[11px] text-muted-foreground">
    {children}
  </kbd>
)

export const TOUR_STEPS: TourStep[] = [
  {
    id: "welcome",
    title: "Welcome to lasso",
    body: (
      <>
        Lasso is a browser front end for herdr: your terminals and coding
        agents, on this machine and any other you can reach. This tour takes
        about a minute. Use <Kbd>←</Kbd> <Kbd>→</Kbd> to move and <Kbd>Esc</Kbd>{" "}
        to skip.
      </>
    ),
  },
  {
    id: "terminal",
    title: "The terminal",
    target: "terminal",
    prepare: "terminal",
    body: (
      <>
        This is herdr itself, live. Every agent runs in a pane here, and
        anything you type goes straight to the focused pane. Other browsers on
        this lasso see the same session.
      </>
    ),
  },
  {
    id: "new",
    title: "Start an agent",
    target: "new",
    only: "desktop",
    body: (
      <>
        New starts a coding agent in a fresh pane, or a plain terminal, on any
        host. <Kbd>⌘O</Kbd> opens it on the agent form, <Kbd>⌘I</Kbd> on the
        terminal form.
      </>
    ),
  },
  {
    id: "chat",
    title: "Read it as a conversation",
    target: "chat",
    tryIt: true,
    only: "desktop",
    body: (
      <>
        Chat shows the focused agent&apos;s session as messages instead of
        terminal output, with a composer to reply. Try it now: click Chat, then
        click it again to come back. <Kbd>⌘J</Kbd> toggles it too.
      </>
    ),
  },
  {
    id: "agents",
    title: "Every agent at once",
    target: "agents",
    tryIt: true,
    only: "desktop",
    body: (
      <>
        Agents lays out every agent as its own card, grouped by machine, so you
        can keep an eye on several and answer whichever is waiting on you.{" "}
        <Kbd>⌘E</Kbd>
      </>
    ),
  },
  {
    id: "host",
    title: "Other machines",
    target: "host",
    only: "desktop",
    body: (
      <>
        Lasso can drive herdr on other hosts over SSH. Switch here to work on
        another machine from this same tab.
      </>
    ),
  },
  {
    id: "sidebar",
    title: "The sidebar",
    target: "sidebar-tabs",
    prepare: "sidebar",
    tryIt: true,
    only: "desktop",
    body: (
      <>
        Files follows the focused pane&apos;s folder, with a viewer and editor.
        Browser is a shared Chromium your agents can drive while you watch.
        Scratch is a notepad, Usage your provider quotas. Click through them;{" "}
        <Kbd>⌘\</Kbd> shows or hides the whole panel.
      </>
    ),
  },
  {
    id: "shortcuts",
    title: "Keyboard shortcuts",
    target: "shortcuts",
    only: "desktop",
    body: (
      <>
        Everything above has a shortcut, and <Kbd>⌘/</Kbd> lists them. They work
        even while the terminal has focus.
      </>
    ),
  },
  {
    id: "dial",
    title: "On your phone",
    only: "mobile",
    body: (
      <>
        On a small screen the terminal gets the whole display. The round dial
        beside the keyboard carries New, Chat, the host menu and the sidebar.
        Add lasso to your home screen to get notifications from your agents.
      </>
    ),
  },
  {
    id: "done",
    title: "You're set",
    body: (
      <>
        Start an agent and you&apos;re off. You can replay this tour any time
        from Settings → General → Take the tour.
      </>
    ),
  },
]

const MD = "(min-width: 768px)"
const CARD_W = 320
const GAP = 12
const PAD = 6

type Rect = { top: number; left: number; width: number; height: number }

// The anchor's box, padded, or null when it is missing or not laid out (a
// display:none footer control, a collapsed panel).
function measure(target?: string): Rect | null {
  if (!target) return null
  const el = tourAnchor(target)
  if (!el) return null
  const r = el.getBoundingClientRect()
  if (r.width < 1 || r.height < 1) return null
  return {
    top: r.top - PAD,
    left: r.left - PAD,
    width: r.width + PAD * 2,
    height: r.height + PAD * 2,
  }
}

function sameRect(a: Rect | null, b: Rect | null) {
  if (!a || !b) return a === b
  return (
    Math.abs(a.top - b.top) < 0.5 &&
    Math.abs(a.left - b.left) < 0.5 &&
    Math.abs(a.width - b.width) < 0.5 &&
    Math.abs(a.height - b.height) < 0.5
  )
}

// Where the card goes: below the spotlight if it fits, else above, else beside
// it (a tall target like the terminal fits neither), clamped to the viewport.
function placeCard(
  hole: Rect | null,
  cardH: number
): { top: number; left: number } {
  const vw = window.innerWidth
  const vh = window.innerHeight
  const w = Math.min(CARD_W, vw - 32)
  const clampX = (x: number) => Math.max(16, Math.min(x, vw - w - 16))
  const clampY = (y: number) => Math.max(16, Math.min(y, vh - cardH - 16))
  if (!hole)
    return { top: clampY((vh - cardH) / 2), left: clampX((vw - w) / 2) }
  const cx = hole.left + hole.width / 2 - w / 2
  if (hole.top + hole.height + GAP + cardH <= vh - 16)
    return { top: hole.top + hole.height + GAP, left: clampX(cx) }
  if (hole.top - GAP - cardH >= 16)
    return { top: hole.top - GAP - cardH, left: clampX(cx) }
  const cy = clampY(hole.top + hole.height / 2 - cardH / 2)
  if (hole.left + hole.width + GAP + w <= vw - 16)
    return { top: cy, left: hole.left + hole.width + GAP }
  if (hole.left - GAP - w >= 16) return { top: cy, left: hole.left - GAP - w }
  // Covers the screen (the terminal on a phone): float it in the middle.
  return { top: clampY((vh - cardH) / 2), left: clampX((vw - w) / 2) }
}

export function OnboardingTour({
  open,
  onClose,
  prepare,
}: {
  open: boolean
  // Finished or skipped; the tour does not distinguish, both mean "seen".
  onClose: () => void
  prepare: (what: TourPrepare) => void
}) {
  const [wide, setWide] = React.useState(() => window.matchMedia(MD).matches)
  React.useEffect(() => {
    const mq = window.matchMedia(MD)
    const on = () => setWide(mq.matches)
    mq.addEventListener("change", on)
    return () => mq.removeEventListener("change", on)
  }, [])
  const steps = React.useMemo(
    () => TOUR_STEPS.filter((s) => !s.only || (s.only === "desktop") === wide),
    [wide]
  )
  const [index, setIndex] = React.useState(0)
  React.useEffect(() => {
    if (open) setIndex(0)
  }, [open])
  const i = Math.min(index, steps.length - 1)
  const step = steps[i]
  const last = i === steps.length - 1

  // Prepare the screen for the step when it becomes current, not on every
  // render: a human who closes the sidebar mid-step is not fought.
  React.useEffect(() => {
    if (open && step.prepare) prepare(step.prepare)
  }, [open, step, prepare])

  // Follow the anchor. Polled rather than observed: what moves it is a panel
  // animating, a footer reflowing, a view swapping, none of which reports to
  // one element's ResizeObserver. Only runs while the tour is up.
  const [hole, setHole] = React.useState<Rect | null>(null)
  React.useLayoutEffect(() => {
    if (!open) return
    const tick = () => {
      const next = measure(step.target)
      setHole((prev) => (sameRect(prev, next) ? prev : next))
    }
    tick()
    const id = window.setInterval(tick, 150)
    window.addEventListener("resize", tick)
    return () => {
      window.clearInterval(id)
      window.removeEventListener("resize", tick)
    }
  }, [open, step])

  // A callback ref, since the card mounts and unmounts with `open`.
  const [cardH, setCardH] = React.useState(180)
  const cardRef = React.useCallback((el: HTMLDivElement | null) => {
    if (!el) return
    const ro = new ResizeObserver(() => setCardH(el.offsetHeight))
    ro.observe(el)
    setCardH(el.offsetHeight)
    return () => ro.disconnect()
  }, [])

  const next = React.useCallback(() => {
    if (last) onClose()
    else setIndex(i + 1)
  }, [last, onClose, i])
  const back = React.useCallback(() => setIndex(Math.max(0, i - 1)), [i])

  // Capture phase, so the arrows move the tour rather than the terminal or a
  // list that happens to hold focus. Cmd chords pass through untouched: the
  // tour tells people to try them.
  React.useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey) return
      if (e.key === "Escape") onClose()
      else if (e.key === "ArrowRight") next()
      else if (e.key === "ArrowLeft") back()
      else return
      e.preventDefault()
      e.stopPropagation()
    }
    document.addEventListener("keydown", onKey, true)
    return () => document.removeEventListener("keydown", onKey, true)
  }, [open, next, back, onClose])

  // Focus the primary button on every step, taking focus out of the terminal
  // iframe so the keys above reach this document at all.
  const nextRef = React.useRef<HTMLButtonElement>(null)
  // biome-ignore lint/correctness/useExhaustiveDependencies: i is the trigger, one focus per step
  React.useEffect(() => {
    if (open) nextRef.current?.focus({ preventScroll: true })
  }, [open, i])

  if (!open) return null

  const pos = placeCard(hole, cardH)
  const shade = "pointer-events-auto fixed bg-black/55"

  return (
    <div className="fixed inset-0 z-[60]" style={{ pointerEvents: "none" }}>
      {hole ? (
        <>
          {/* Four panes around the hole rather than one scrim with a cutout,
              so a tryIt target underneath really is clickable and everything
              else really is not. */}
          <div
            className={shade}
            style={{ top: 0, left: 0, right: 0, height: Math.max(0, hole.top) }}
          />
          <div
            className={shade}
            style={{
              top: hole.top + hole.height,
              left: 0,
              right: 0,
              bottom: 0,
            }}
          />
          <div
            className={shade}
            style={{
              top: hole.top,
              left: 0,
              width: Math.max(0, hole.left),
              height: hole.height,
            }}
          />
          <div
            className={shade}
            style={{
              top: hole.top,
              left: hole.left + hole.width,
              right: 0,
              height: hole.height,
            }}
          />
          <div
            aria-hidden
            className={cn(
              "fixed rounded-lg ring-2 ring-primary transition-all duration-200",
              !step.tryIt && "pointer-events-auto"
            )}
            style={{
              top: hole.top,
              left: hole.left,
              width: hole.width,
              height: hole.height,
            }}
          />
        </>
      ) : (
        <div className={cn(shade, "inset-0")} />
      )}
      <div
        ref={cardRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="tour-title"
        className="pointer-events-auto fixed flex flex-col gap-3 rounded-xl bg-popover p-4 text-popover-foreground text-sm shadow-elev-modal ring-1 ring-foreground/10 transition-[top,left] duration-200"
        style={{
          top: pos.top,
          left: pos.left,
          width: Math.min(CARD_W, window.innerWidth - 32),
        }}
      >
        <div className="flex items-start gap-2">
          <h2 id="tour-title" className="flex-1 font-medium text-base">
            {step.title}
          </h2>
          <Button
            variant="ghost"
            size="icon-xs"
            className="-mt-0.5 -mr-1.5"
            title="Skip the tour (Esc)"
            aria-label="Skip the tour"
            onClick={onClose}
          >
            <X />
          </Button>
        </div>
        <p className="text-muted-foreground leading-relaxed">{step.body}</p>
        <div className="flex items-center gap-2">
          <div className="flex flex-1 gap-1" aria-hidden>
            {steps.map((s, n) => (
              <span
                key={s.id}
                className={cn(
                  "size-1.5 rounded-full bg-muted-foreground/30",
                  n === i && "bg-primary"
                )}
              />
            ))}
          </div>
          <span className="sr-only">
            Step {i + 1} of {steps.length}
          </span>
          {i > 0 && (
            <Button variant="ghost" size="sm" onClick={back}>
              <ArrowLeft />
              Back
            </Button>
          )}
          <Button ref={nextRef} size="sm" onClick={next}>
            {last ? "Get started" : i === 0 ? "Show me" : "Next"}
            {!last && <ArrowRight />}
          </Button>
        </div>
      </div>
    </div>
  )
}
