import { cn } from "@/lib/utils"

// At-a-glance git working-tree status. Shows the uncommitted-change count in a
// gold pill when dirty, or a small green dot when clean. Renders nothing until
// the status is known (no repo / still loading) so it never flashes a stale
// indicator. `ready` is whether we have a definitive answer for the active repo.
export function GitStatusBadge({
  dirty,
  ready,
  className,
  textClassName,
}: {
  dirty: number
  ready: boolean
  // Extra classes for positioning (e.g. ml-1.5) on whichever element renders.
  className?: string
  // Extra classes applied only to the count pill (e.g. its font size).
  textClassName?: string
}) {
  if (!ready) return null
  if (dirty > 0) {
    return (
      <span
        className={cn(
          "rounded-full bg-warn px-1.5 font-semibold text-background",
          textClassName,
          className
        )}
        title={`${dirty} uncommitted change${dirty === 1 ? "" : "s"}`}
      >
        {dirty}
      </span>
    )
  }
  // The theme's "good" token (a teal/foam) as the "clean" signal, so the dot
  // tracks the live herdr theme rather than a hardcoded green.
  return (
    <span
      className={cn(
        "size-2 shrink-0 self-center rounded-full bg-good",
        className
      )}
      title="working tree clean"
    />
  )
}
