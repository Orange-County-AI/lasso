import { ChevronDown } from "lucide-react"
import { Popover } from "radix-ui"
import * as React from "react"
import { cn } from "@/lib/utils"

// A free-text field with a suggestions dropdown — an editable combobox. Unlike
// the native <input list=datalist> (which filters suggestions by the *current*
// value, so a filled field only ever shows its own value) and unlike Combobox
// (which only commits values drawn from its list), this always shows every
// suggestion when opened and narrows only as the user actively types, while
// committing whatever free text they enter. Used for the model field, where
// names churn faster than releases so the list is a hint, not a constraint.
export function EditableCombobox({
  id,
  value,
  onValueChange,
  suggestions,
  placeholder,
  className,
}: {
  id?: string
  value: string
  onValueChange: (value: string) => void
  suggestions: string[]
  placeholder?: string
  className?: string
}) {
  const [open, setOpen] = React.useState(false)
  const [active, setActive] = React.useState(-1)
  // Suggestions narrow only while the user is *typing*. Opening via focus or the
  // chevron shows the full list (typing=false) even when the field already holds
  // a complete suggestion — that's the datalist quirk this component exists to
  // fix. Choosing an item or reopening resets it.
  const [typing, setTyping] = React.useState(false)
  const inputRef = React.useRef<HTMLInputElement>(null)
  const listRef = React.useRef<HTMLDivElement>(null)

  const filtered = React.useMemo(() => {
    const q = value.trim().toLowerCase()
    if (!typing || !q) return suggestions
    return suggestions.filter((s) => s.toLowerCase().includes(q))
  }, [suggestions, value, typing])

  const showList = open && filtered.length > 0

  // Keep the highlighted row in view during keyboard navigation.
  React.useEffect(() => {
    if (!showList || active < 0) return
    listRef.current
      ?.querySelector<HTMLElement>(`[data-index="${active}"]`)
      ?.scrollIntoView({ block: "nearest" })
  }, [active, showList])

  const choose = (s: string) => {
    onValueChange(s)
    setTyping(false)
    setActive(-1)
    setOpen(false)
    inputRef.current?.focus()
  }

  const openAll = () => {
    setTyping(false)
    setActive(-1)
    setOpen(true)
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault()
      if (!open) return openAll()
      setActive((a) => Math.min(a + 1, filtered.length - 1))
    } else if (e.key === "ArrowUp") {
      e.preventDefault()
      setActive((a) => Math.max(a - 1, 0))
    } else if (e.key === "Enter") {
      // Only intercept Enter to pick a highlighted suggestion; otherwise let it
      // bubble so the form's submit handling still works from this field.
      if (open && active >= 0 && filtered[active]) {
        e.preventDefault()
        choose(filtered[active])
      }
    } else if (e.key === "Escape") {
      if (open) {
        e.preventDefault()
        setOpen(false)
      }
    }
  }

  return (
    <Popover.Root open={showList} onOpenChange={setOpen}>
      <Popover.Anchor asChild>
        <div className="relative">
          <input
            id={id}
            ref={inputRef}
            value={value}
            placeholder={placeholder}
            autoComplete="off"
            spellCheck={false}
            onChange={(e) => {
              onValueChange(e.target.value)
              setTyping(true)
              setActive(-1)
              setOpen(true)
            }}
            onFocus={openAll}
            onKeyDown={onKeyDown}
            className={cn(
              "h-8 w-full rounded-lg border border-input bg-background py-1.5 pr-8 pl-2.5 text-sm shadow-well outline-none transition-colors placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50",
              className
            )}
          />
          <button
            type="button"
            tabIndex={-1}
            aria-label="Toggle suggestions"
            // mousedown (not click) so the input keeps focus and we control open
            // state ourselves rather than letting the blur/focus race close it.
            onMouseDown={(e) => {
              e.preventDefault()
              if (open) setOpen(false)
              else openAll()
              inputRef.current?.focus()
            }}
            className="absolute top-1/2 right-2 -translate-y-1/2 text-muted-foreground transition-colors hover:text-foreground"
          >
            <ChevronDown className="size-4" />
          </button>
        </div>
      </Popover.Anchor>
      {/* Not portaled — same reasoning as Combobox: inside a modal Dialog a
          body-portaled popover can't be wheel-scrolled. */}
      <Popover.Content
        align="start"
        sideOffset={4}
        // Keep focus in the input so typing continues to filter the open list.
        onOpenAutoFocus={(e) => e.preventDefault()}
        // Don't steal focus back on close (e.g. outside click / Escape).
        onCloseAutoFocus={(e) => e.preventDefault()}
        className="isolate z-50 w-[var(--radix-popover-trigger-width)] overflow-hidden rounded-lg border border-border bg-popover p-1 text-popover-foreground shadow-elev-pop outline-none"
      >
        <div ref={listRef} className="max-h-60 overflow-y-auto">
          {filtered.map((s, i) => (
            <button
              key={s}
              type="button"
              data-index={i}
              // mousedown-select so choosing doesn't first blur the input.
              onMouseDown={(e) => {
                e.preventDefault()
                choose(s)
              }}
              onMouseMove={() => setActive(i)}
              className={cn(
                "flex w-full items-center rounded-md px-2 py-1.5 text-left text-sm outline-none",
                i === active && "bg-primary text-primary-foreground"
              )}
            >
              <span className="truncate">{s}</span>
            </button>
          ))}
        </div>
      </Popover.Content>
    </Popover.Root>
  )
}
