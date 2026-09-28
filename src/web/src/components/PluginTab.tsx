import * as React from "react"

import type { PluginTabInfo } from "@/lib/api"
import { useApp } from "@/lib/app-store"
import {
  attachPluginBridge,
  type PluginBridge,
  type PluginPaneContext,
  themeSnapshot,
} from "@/lib/plugins"
import { onThemeApplied } from "@/lib/theme"

// What a plugin frame may do, and it must match the CSP `sandbox` the server
// stamps on every /plugins/ response. The attribute is belt and braces for
// that header — and the only sandbox at all for a `url` tab, whose server is
// not lasso. NEVER add allow-same-origin: a document served from lasso's own
// origin with it would be same-origin with this page and could call /api/file
// with the human's cookies. Nor allow-top-navigation: a plugin must not be
// able to navigate lasso itself away.
const PLUGIN_SANDBOX =
  "allow-scripts allow-forms allow-popups allow-modals allow-downloads"

// A plugin's sidebar tab: its page in a sandboxed iframe, and — for a page
// lasso serves — the postMessage bridge that is its only way to reach lasso
// (lib/plugins.ts).
export function PluginTab({
  plugin,
  tab,
  active,
}: {
  plugin: string
  tab: PluginTabInfo
  // On screen: this tab selected and the sidebar open. file.open is refused
  // otherwise, since a tab nobody can see must not rearrange the screen.
  active: boolean
}) {
  const frame = React.useRef<HTMLIFrameElement>(null)
  const bridge = React.useRef<PluginBridge | null>(null)
  const { host, activeCwd, cwdHost, activePaneID } = useApp()

  const ctx: PluginPaneContext = {
    host,
    cwd: activeCwd,
    cwd_host: cwdHost,
    pane_id: activePaneID,
  }
  // Read by the bridge at call time, so it never answers with the pane that
  // was focused when the frame mounted.
  const ctxRef = React.useRef(ctx)
  ctxRef.current = ctx
  const activeRef = React.useRef(active)
  activeRef.current = active

  // Only a page lasso itself serves gets the bridge. A `url` tab is someone
  // else's site framed as-is: it has no business calling this plugin's tools,
  // and nothing about it was written against the protocol. Anything that is
  // neither (the server validated it, but this is the page that frames it) is
  // not framed at all.
  const served = tab.src.startsWith("/plugins/")
  const framable = served || /^https?:\/\//i.test(tab.src)

  React.useEffect(() => {
    const el = frame.current
    if (!el || !served) return
    const b = attachPluginBridge({
      frame: el,
      plugin,
      tab: tab.id,
      context: () => ctxRef.current,
      onScreen: () =>
        activeRef.current && document.visibilityState === "visible",
    })
    bridge.current = b
    const offTheme = onThemeApplied(() => b.push("theme", themeSnapshot()))
    return () => {
      offTheme()
      b.dispose()
      bridge.current = null
    }
  }, [served, plugin, tab.id])

  // Push the context whenever the focused pane moves. Keyed on the values, not
  // on ctx's identity, which is new every render; the bridge reads them itself.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the values are the trigger, read through ctxRef
  React.useEffect(() => {
    bridge.current?.pushContext()
  }, [host, activeCwd, cwdHost, activePaneID])

  if (!framable) {
    return (
      <p className="p-4 text-[13px] text-muted-foreground">
        This tab's address is not something lasso will frame.
      </p>
    )
  }

  return (
    <iframe
      ref={frame}
      src={tab.src}
      title={`${plugin}: ${tab.label}`}
      sandbox={PLUGIN_SANDBOX}
      referrerPolicy="no-referrer"
      className="h-full w-full flex-1 border-0 bg-transparent"
      // Right after the page loads, it is told where it is and how lasso looks
      // — the protocol's two pushes, so a page need not ask before painting.
      onLoad={() => bridge.current?.pushAll()}
    />
  )
}
