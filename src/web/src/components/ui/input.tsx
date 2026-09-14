import type * as React from "react"

import { cn } from "@/lib/utils"

// Phones rewrite what you type unless told not to: autocorrect substitutes whole
// words, autocapitalize capitalizes the first letter, spellcheck draws squiggles
// under every path and identifier, and autocomplete offers the device's own
// dictionary over the field.
//
// Every input in this app holds machine-ish text — a path, a URL, a glob, a
// shell command, a prompt — so the four are off by DEFAULT here, spread before
// the caller's props so a field that genuinely wants suggestions can still ask
// for them. It is exported for the raw <textarea>/<input> elements that wear
// these classes instead of using the component.
//
// The terminal's own textarea needs none of this: xterm.js sets all four
// itself. What none of it can do is remove the QuickType suggestion bar above
// the keyboard — that is an iOS keyboard setting, not a page one.
export const NO_AUTOCORRECT = {
  autoComplete: "off",
  autoCorrect: "off",
  autoCapitalize: "off",
  spellCheck: false,
} as const

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      {...NO_AUTOCORRECT}
      className={cn(
        "h-8 w-full min-w-0 rounded-lg border border-input bg-transparent px-2.5 py-1 text-base shadow-well outline-none transition-colors file:inline-flex file:h-6 file:border-0 file:bg-transparent file:font-medium file:text-foreground file:text-sm placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:pointer-events-none disabled:cursor-not-allowed disabled:bg-input/50 disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 md:text-sm dark:bg-input/30 dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40 dark:disabled:bg-input/80",
        className
      )}
      {...props}
    />
  )
}

export { Input }
