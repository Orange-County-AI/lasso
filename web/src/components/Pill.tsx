import type * as React from "react"
import { Badge } from "@/components/ui/badge"
import { cn } from "@/lib/utils"

type Tone = "muted" | "accent" | "good" | "bad" | "warn"

const toneClass: Record<Tone, string> = {
  muted: "text-muted-foreground border-border",
  accent: "text-primary border-primary/40",
  good: "text-good border-good",
  bad: "text-bad border-bad",
  warn: "text-warn border-warn",
}

// A rounded status pill — the original UI's `.pill`, rebuilt on shadcn's Badge
// so it inherits the live theme. Tones map to herdr's semantic palette.
export function Pill({
  tone = "muted",
  className,
  clickable,
  multiline,
  ...props
}: React.ComponentProps<"span"> & {
  tone?: Tone
  clickable?: boolean
  // Allow the pill to grow and wrap (e.g. a long repo path) instead of the
  // Badge's default single fixed-height line, which clips the overflow.
  multiline?: boolean
}) {
  return (
    <Badge
      asChild
      variant="outline"
      className={cn(
        "rounded-full px-2 py-px font-normal text-[13px]",
        multiline
          ? "h-auto items-start overflow-visible whitespace-normal break-all py-0.5 leading-snug"
          : "whitespace-nowrap",
        toneClass[tone],
        clickable && "cursor-pointer hover:bg-primary/15",
        className
      )}
    >
      <span {...props} />
    </Badge>
  )
}
