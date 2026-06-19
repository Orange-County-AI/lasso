import * as React from "react"
import { Input } from "@/components/ui/input"
import { cn } from "@/lib/utils"

// The shared "go to path" input. One value backs both the Files and Diff
// subtabs (owned by FilesPanel) — typing a path re-roots/opens it in Files and
// filters the diff in Diff. Keeps itself scrolled to the path's tail (the
// useful end) whenever the value or layout changes, unless it's being edited.
export function PathInput({
  value,
  onChange,
  onSubmit,
  placeholder = "go to path…  (Enter)",
  className,
  inputRef: externalRef,
}: {
  value: string
  onChange: (value: string) => void
  onSubmit?: (value: string) => void
  placeholder?: string
  className?: string
  // Optional ref to the underlying <input>, so a parent can tell whether the
  // box is focused (e.g. to avoid overwriting the text mid-edit).
  inputRef?: React.RefObject<HTMLInputElement | null>
}) {
  const internalRef = React.useRef<HTMLInputElement>(null)
  const inputRef = externalRef ?? internalRef

  // biome-ignore lint/correctness/useExhaustiveDependencies: value is the intended trigger
  React.useEffect(() => {
    const el = inputRef.current
    if (!el) return
    const showTail = () => {
      if (document.activeElement !== el) el.scrollLeft = el.scrollWidth
    }
    showTail()
    const ro = new ResizeObserver(showTail)
    ro.observe(el)
    return () => ro.disconnect()
  }, [value])

  return (
    <Input
      ref={inputRef}
      value={value}
      spellCheck={false}
      autoComplete="off"
      placeholder={placeholder}
      className={cn("h-7 flex-1 text-[13px]", className)}
      onChange={(e) => onChange(e.target.value)}
      onKeyDown={(e) => {
        const v = e.currentTarget.value.trim()
        if (e.key === "Enter" && v) onSubmit?.(v)
      }}
      onBlur={(e) => {
        e.currentTarget.scrollLeft = e.currentTarget.scrollWidth
      }}
    />
  )
}
