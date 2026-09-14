import { Orb } from "@/components/ui/orb"
import { cn } from "@/lib/utils"

// The one place an agent's status is drawn. Two surfaces list agents — the
// docked column and the phone's sheet — and the vocabulary has to read the same
// in both, since the same agent can be seen in either depending on the screen.
//
// Only the two states a human acts on get a word: an agent that is working is
// the one being waited on, and one that is blocked has stopped for an answer.
// idle and done are rest, and naming them on every row turns a list you scan
// into a list you read.
export function AgentStatus({
  status,
  className,
}: {
  status?: string
  className?: string
}) {
  if (!status) return null
  const speaking = status === "working" || status === "blocked"
  return (
    <span
      className={cn(
        "flex shrink-0 items-center gap-1 text-[11px]",
        status === "blocked"
          ? "text-destructive"
          : status === "working"
            ? "text-primary"
            : "text-muted-foreground",
        className
      )}
      title={status}
    >
      {status === "working" ? (
        <Orb state="working" px={12} />
      ) : (
        <span className="size-1.5 rounded-full bg-current opacity-70" />
      )}
      {speaking && status}
    </span>
  )
}
