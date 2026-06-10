import * as React from "react"

import { HOST_CHANGED_EVENT } from "@/lib/app-store"
import { bootTermFrame, refitTerminal } from "@/lib/terminal"

// A ttyd terminal iframe (the terminal at /terminal/ or the shell at /shell/).
// It stays mounted across tab switches — only hidden via CSS — so the WebSocket
// never reconnects. When shown again, nudge xterm to refit.
export function TerminalFrame({
  id,
  src,
  title,
  suppressContext,
  hidden,
}: {
  id: string
  src: string
  title: string
  suppressContext: boolean
  hidden: boolean
}) {
  // Bump to remount the iframe onto a fresh ttyd session. We remount (fresh
  // element via React key) rather than reloading the existing frame's document:
  // reloading runs ttyd's beforeunload handler, which pops the browser's "Reload
  // site? Changes may not be saved." prompt, whereas unmounting the element just
  // tears the frame down with no prompt.
  const [reloadKey, setReloadKey] = React.useState(0)
  React.useEffect(() => {
    const onHostChange = () => setReloadKey((k) => k + 1)
    window.addEventListener(HOST_CHANGED_EVENT, onHostChange)
    return () => window.removeEventListener(HOST_CHANGED_EVENT, onHostChange)
  }, [])

  // Re-wire xterm whenever the iframe element is (re)created — on mount and on
  // each remount. reloadKey is a trigger-only dep: a new key means a fresh
  // iframe element, so bootTermFrame must re-run to wire the new one.
  // biome-ignore lint/correctness/useExhaustiveDependencies: reloadKey re-runs boot on iframe remount
  React.useEffect(
    () => bootTermFrame(id, suppressContext),
    [id, suppressContext, reloadKey]
  )

  React.useEffect(() => {
    if (!hidden) refitTerminal(id)
  }, [hidden, id])

  return (
    <iframe
      key={reloadKey}
      id={id}
      src={src}
      title={title}
      className="frame"
      style={{ display: hidden ? "none" : "block" }}
    />
  )
}
