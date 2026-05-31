import * as React from "react"

import { bootTermFrame, refitTerminal } from "@/lib/terminal"

// A ttyd terminal iframe (the herdr terminal at /terminal/ or the out-of-herdr
// shell at /shell/). It stays mounted across tab switches — only hidden via CSS
// — so the WebSocket never reconnects. When shown again, nudge xterm to refit.
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
  React.useEffect(
    () => bootTermFrame(id, suppressContext),
    [id, suppressContext]
  )

  React.useEffect(() => {
    if (!hidden) refitTerminal(id)
  }, [hidden, id])

  return (
    <iframe
      id={id}
      src={src}
      title={title}
      className="frame"
      style={{ display: hidden ? "none" : "block" }}
    />
  )
}
