import { Check, ChevronsUpDown } from "lucide-react"
import { Popover } from "radix-ui"
import * as React from "react"
import { cn } from "@/lib/utils"

export type ComboOption = { value: string; label: string }

// A filter-as-you-type select. Built on radix-ui's Popover (the same unified
// primitive package the rest of the UI uses) with our own substring filter and
// keyboard navigation — no extra dependency. Selection is tracked by the value
// string so callers keep plain-string form state.
export function Combobox({
  id,
  items,
  value,
  onValueChange,
  placeholder = "Select…",
  filterPlaceholder = "Filter…",
  emptyText = "No matches.",
  disabled,
  className,
}: {
  id?: string
  items: ComboOption[]
  value: string
  onValueChange: (value: string) => void
  placeholder?: string
  filterPlaceholder?: string
  emptyText?: string
  disabled?: boolean
  className?: string
}) {
  const [open, setOpen] = React.useState(false)
  const [query, setQuery] = React.useState("")
  const [active, setActive] = React.useState(0)
  const listRef = React.useRef<HTMLDivElement>(null)
  // Tracks what last moved the highlight. We only auto-scroll for keyboard
  // navigation: scrolling the highlight into view on pointer-driven changes
  // fights the user's wheel scroll (each hover re-snaps the list), making it
  // feel like the list can't be scrolled with the cursor inside it.
  const navSource = React.useRef<"keyboard" | "pointer">("keyboard")

  const selected = items.find((i) => i.value === value) ?? null
  const filtered = React.useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return items
    return items.filter((i) => i.label.toLowerCase().includes(q))
  }, [items, query])

  // Reset the highlight to the top whenever the filtered set changes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset on filter change
  React.useEffect(() => {
    setActive(0)
  }, [query])

  // Keep the highlighted item scrolled into view — but only when the highlight
  // moved via the keyboard. Doing it on pointer-driven changes would re-snap the
  // list on every hover and block wheel-scrolling with the cursor inside.
  React.useEffect(() => {
    if (!open || navSource.current !== "keyboard") return
    listRef.current
      ?.querySelector<HTMLElement>(`[data-index="${active}"]`)
      ?.scrollIntoView({ block: "nearest" })
  }, [active, open])

  const choose = (opt: ComboOption) => {
    onValueChange(opt.value)
    setOpen(false)
    setQuery("")
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault()
      navSource.current = "keyboard"
      setActive((a) => Math.min(a + 1, filtered.length - 1))
    } else if (e.key === "ArrowUp") {
      e.preventDefault()
      navSource.current = "keyboard"
      setActive((a) => Math.max(a - 1, 0))
    } else if (e.key === "Enter") {
      e.preventDefault()
      const opt = filtered[active]
      if (opt) choose(opt)
    }
  }

  return (
    <Popover.Root
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (!o) setQuery("")
      }}
    >
      <Popover.Trigger
        id={id}
        type="button"
        disabled={disabled}
        className={cn(
          "flex h-8 w-full items-center justify-between gap-2 rounded-lg border border-input bg-background px-2.5 py-1.5 text-sm shadow-well outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50",
          !selected && "text-muted-foreground",
          className
        )}
      >
        <span className="truncate">
          {selected ? selected.label : placeholder}
        </span>
        <ChevronsUpDown className="size-4 shrink-0 text-muted-foreground" />
      </Popover.Trigger>
      <Popover.Portal>
        <Popover.Content
          align="start"
          sideOffset={4}
          className="isolate z-50 w-[var(--radix-popover-trigger-width)] overflow-hidden rounded-lg border border-border bg-popover p-1 text-popover-foreground shadow-elev-pop outline-none"
          onOpenAutoFocus={(e) => {
            // Focus the filter input rather than the first list item.
            e.preventDefault()
            const content = e.currentTarget as HTMLElement | null
            content?.querySelector("input")?.focus()
          }}
        >
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder={filterPlaceholder}
            className="mb-1 w-full rounded-md border border-input bg-background px-2 py-1 text-sm outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring"
          />
          <div ref={listRef} className="max-h-60 overflow-y-auto">
            {filtered.length === 0 ? (
              <div className="px-2 py-1.5 text-muted-foreground text-sm">
                {emptyText}
              </div>
            ) : (
              filtered.map((opt, i) => (
                <button
                  key={opt.value}
                  type="button"
                  data-index={i}
                  onClick={() => choose(opt)}
                  onMouseMove={() => {
                    navSource.current = "pointer"
                    setActive(i)
                  }}
                  className={cn(
                    "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm outline-none",
                    // The keyboard/pointer cursor uses the primary tint so the
                    // highlighted option reads clearly — `bg-accent` resolves to
                    // the near-white hover gray (--h-hover) and is imperceptible
                    // against the popover, so you couldn't tell what was selected.
                    i === active && "bg-primary text-primary-foreground"
                  )}
                >
                  <Check
                    className={cn(
                      "size-4 shrink-0",
                      opt.value === value ? "opacity-100" : "opacity-0"
                    )}
                  />
                  <span className="truncate">{opt.label}</span>
                </button>
              ))
            )}
          </div>
        </Popover.Content>
      </Popover.Portal>
    </Popover.Root>
  )
}
