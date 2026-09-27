import * as React from "react"

import { useApp } from "@/lib/app-store"
import {
  bootTermFrame,
  HERDR_REATTACH_EVENT,
  refitTerminal,
  type TerminalInputMode,
  terminalPasteHost,
} from "@/lib/terminal"
import { termDocParams } from "@/lib/theme"

// A ttyd terminal iframe (the herdr terminal under /terminal/ or the
// out-of-herdr shell under /shell/). It stays mounted across tab switches — only
// hidden via CSS — so the WebSocket never reconnects. When shown again, nudge
// xterm to refit.
//
// `base` is the role's path prefix; the host this tab is on supplies the rest,
// so the frame loads /terminal/<slug>/ and two tabs on two machines each get
// their own machine's terminal from the same origin.
export function TerminalFrame({
  id,
  base,
  title,
  suppressContext,
  inputMode,
  hidden,
}: {
  id: string
  base: "/terminal" | "/shell"
  title: string
  suppressContext: boolean
  inputMode: TerminalInputMode
  hidden: boolean
}) {
  const { host, cwdHost, hostSlug } = useApp()
  // Focus can move between an ordinary local pane and a nested remote attach
  // without remounting ttyd. Keep the upload target live behind the one paste
  // listener installed in the iframe document.
  const pasteHostRef = React.useRef<string | undefined>(undefined)
  pasteHostRef.current = terminalPasteHost(inputMode, host, cwdHost)

  // The slug is the React key as well as the path, so moving this tab to
  // another host swaps in a fresh <iframe> element pointed at that host's ttyd.
  // We remount rather than reload the existing frame's document: reloading runs
  // ttyd's beforeunload handler, which pops the browser's "Reload site? Changes
  // may not be saved." prompt, whereas unmounting the element just tears the
  // frame down with no prompt.
  //
  // The document is served with the palette named in its own URL, so the
  // terminal boots on this tab's canvas — see lib/theme.ts:termDocParams for
  // why pinning it afterwards is too late, and main.go:ttydDocTheme for what
  // the server does with it.
  //
  // Captured ONCE per host, and deliberately never updated: src is the iframe's
  // React key, so folding a live re-theme into it would remount every terminal
  // (and drop its websocket) on each appearance change. applyTermTheme re-pins
  // an open terminal in place; this URL only has to be right at boot.
  const frame = React.useRef<{ slug: string; params: string } | null>(null)
  const params = termDocParams()
  // Null until the first /api/active answer lands — rendering the frame before
  // then would load the role's bare prefix, which addresses no instance — and
  // until the palette has settled, for the same reason: a URL guessed before
  // then boots the terminal on a canvas nobody chose.
  if (hostSlug && params !== null && frame.current?.slug !== hostSlug)
    frame.current = { slug: hostSlug, params }
  // Read back through the LIVE slug, so a host that goes away unmounts the
  // frame as it always did rather than leaving the last host's terminal up.
  const pinned = frame.current?.slug === hostSlug ? frame.current : null
  const src = pinned ? `${base}/${pinned.slug}/?${pinned.params}` : null

  // Bumped to respawn the herdr client in place (reattachHerdrTerminal), which
  // is how a terminal showing a saved machine comes back to Local. Part of the
  // key, so it remounts the same way a host move does — no beforeunload prompt.
  const [attach, setAttach] = React.useState(0)
  React.useEffect(() => {
    if (base !== "/terminal") return
    const bump = () => setAttach((n) => n + 1)
    window.addEventListener(HERDR_REATTACH_EVENT, bump)
    return () => window.removeEventListener(HERDR_REATTACH_EVENT, bump)
  }, [base])
  const frameKey = src ? `${src}#${attach}` : null

  // Re-wire xterm whenever the iframe element is (re)created — on mount and on
  // each host-move or reattach remount. A new key means a fresh iframe element,
  // so bootTermFrame must re-run to wire the new one.
  React.useEffect(() => {
    if (!frameKey) return
    return bootTermFrame(
      id,
      suppressContext,
      inputMode,
      () => pasteHostRef.current
    )
  }, [id, suppressContext, inputMode, frameKey])

  React.useEffect(() => {
    if (!hidden) refitTerminal(id)
  }, [hidden, id])

  if (!src) return null
  return (
    <iframe
      key={frameKey}
      id={id}
      src={src}
      title={title}
      className="frame"
      style={{ display: hidden ? "none" : "block" }}
    />
  )
}
