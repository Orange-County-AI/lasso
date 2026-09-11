import * as React from "react"
import { type OrbState, ThinkingOrb } from "thinking-orbs"
import {
  resolvedMode,
  subscribeAppearance,
  subscribeSystemScheme,
} from "@/lib/mode"

export type { OrbState }

// The scheme the CHROME is currently painted in. Only `on="accent"` needs it,
// and it has to be reactive: an appearance change arriving from another browser
// repaints the button under a mounted orb.
function useScheme(): "light" | "dark" {
  return React.useSyncExternalStore(
    React.useCallback((cb: () => void) => {
      const stopAppearance = subscribeAppearance(cb)
      const stopSystem = subscribeSystemScheme(cb)
      return () => {
        stopAppearance()
        stopSystem()
      }
    }, []),
    () => resolvedMode(),
    () => "dark"
  )
}

// Orb is lasso's one "work in flight" indicator — thinking-orbs' dotted canvas
// orb, in place of the spinning Loader2 and the CSS `.spinner` that used to do
// this job.
//
// The library ships nine animated states and an early cut of this used four of
// them, picked for what each wait actually was (weaving a create, connecting to
// a machine, searching a fetch). Compared side by side at these sizes that read
// as noise rather than as meaning: the distinctions are legible at 64px in a
// gallery and not at 14–16px in a dropdown row, so all the indicator told you
// was that lasso was busy — in four different costumes. `working` everywhere,
// deliberately. The prop stays because the states are genuinely distinct at
// chat-avatar scale, should anything here ever be that big.
// (They can be compared at https://orbs.orangecountyai.com.)
//
// Two things about this component are constraints rather than taste:
//
//  1. SIZE. The library ships exactly two hand-tuned designs, 20px and 64px,
//     with their own dot counts and speeds — neither is a scale factor for the
//     other. So `px` never picks a different design; it only paints the 20px
//     one into a smaller CSS box, leaving the canvas backing store at 20 * dpr.
//     That makes a 14px orb a downsample of a tuned design rather than a blur
//     of an untuned one. Anything wanting the big design should reach for
//     ThinkingOrb directly with size={64}.
//
//  2. COLOR. The orb is strictly monochrome — light ink on dark, dark ink on
//     light — and takes no palette. `theme="auto"` (the default) walks up for
//     the `dark`/`light` class that lib/mode.ts already puts on <html> and
//     re-resolves on a MutationObserver, so it follows an appearance change
//     from another browser for free. Don't try to tint it from the theme's
//     --h-* tokens; there is no prop for it.
//
//     Which is exactly why `on` exists. Auto-detection reads the DOCUMENT's
//     scheme, and a filled button is not painted in it: `--primary` is the
//     theme's own accent (`--h-accent`, #faa968 under Retro 82) and its text is
//     `--primary-foreground` (`--h-bg`). So under the dark chrome the orb chose
//     light ink and painted near-invisible dots on a bright amber button.
//     `on="accent"` pins the ink to the opposite of the resolved scheme, which
//     is the same contrast bet `--primary-foreground` already makes: a filled
//     button's surface is the accent, and an accent that did not contrast with
//     the background would have unreadable label text too.
//
// The canvas parks its rAF loop when scrolled out of view or the tab is
// hidden, and renders one static frame under prefers-reduced-motion, so a
// list of them (every probing host in the switcher) stays cheap.
export function Orb({
  state = "working",
  px = 20,
  on = "surface",
  className,
  label,
}: {
  state?: OrbState
  /** Painted size in CSS px. The design is tuned at 20; smaller downsamples. */
  px?: number
  /**
   * Which surface it sits on. "surface" is anything painted from --background
   * / --card / --popover, i.e. the document's own scheme. "accent" is a filled
   * primary Button, whose surface is the theme accent and therefore the
   * opposite lightness.
   */
  on?: "surface" | "accent"
  className?: string
  /** Overrides the per-state default ("Working…", "Connecting…", …). */
  label?: string
}) {
  const scheme = useScheme()
  return (
    <ThinkingOrb
      state={state}
      size={20}
      theme={on === "accent" ? (scheme === "dark" ? "light" : "dark") : "auto"}
      className={className ? `shrink-0 ${className}` : "shrink-0"}
      aria-label={label}
      style={px === 20 ? undefined : { width: px, height: px }}
    />
  )
}
