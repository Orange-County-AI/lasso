import { useQuery } from "@tanstack/react-query"
import {
  Activity,
  Bell,
  Book,
  BookOpen,
  Bot,
  Box,
  Calendar,
  ChartBar,
  Clock,
  Cloud,
  Code,
  Cpu,
  Database,
  FileText,
  FlaskConical,
  Folder,
  Gauge,
  GitBranch,
  Globe,
  Hammer,
  Heart,
  Image,
  Layers,
  LayoutDashboard,
  Link,
  List,
  type LucideIcon,
  Mail,
  Map as MapIcon,
  MessageSquare,
  Music,
  NotebookPen,
  Package,
  Puzzle,
  Rocket,
  Search,
  Server,
  Shield,
  Sparkles,
  SquareTerminal,
  Star,
  Wrench,
  Zap,
} from "lucide-react"
import { toast } from "sonner"
import { agentName } from "@/lib/agents"
import {
  api,
  type PanesPayload,
  type Plugin,
  type PluginSourceInfo,
  type PluginsPayload,
  type PluginTabInfo,
} from "@/lib/api"
import { requestOpenFile } from "@/lib/open-file"
import { qk, queryClient } from "@/lib/query"
import { paletteColors } from "@/lib/theme"

// Plugins, on the client: the listing, the icons a manifest may name, and the
// postMessage bridge a plugin tab talks to lasso through.
//
// A plugin tab is an iframe onto `/plugins/<name>/<entry>`, served with a CSP
// `sandbox` (and framed with the same `sandbox` attribute) that deliberately
// lacks allow-same-origin. That makes the document an OPAQUE origin: it cannot
// read this page, call /api/* with the human's cookies, or touch another
// plugin. Everything it may do goes through the bridge below, one allowlisted
// method at a time.

// usePlugins is the plugin listing. Not polled: every state change on the
// server bumps plugins_rev, and app-store invalidates this key on the bump —
// the same arrangement as ui_state_rev. An older server without the endpoint
// answers an error, which reads as "no plugins" everywhere.
export function usePlugins() {
  return useQuery({
    queryKey: qk.plugins,
    queryFn: () => api.plugins(),
    staleTime: Number.POSITIVE_INFINITY,
    retry: false,
  })
}

// pluginTabsOf flattens the listing to the tabs lasso may render: those of
// enabled (and therefore approved) plugins. The server only fills `tabs` for
// those anyway; the state check is so a stale entry can never frame a tab the
// operator has not approved in its current form.
export function pluginTabsOf(data: PluginsPayload | undefined): {
  plugin: Plugin
  tab: PluginTabInfo
}[] {
  const out: { plugin: Plugin; tab: PluginTabInfo }[] = []
  for (const plugin of data?.plugins ?? []) {
    if (plugin.state !== "enabled") continue
    for (const tab of plugin.tabs ?? []) out.push({ plugin, tab })
  }
  return out
}

// pluginSourceOf reads where a plugin came from. A plugin with no install
// record — hand-placed, or listed by a server older than install/link — is
// "local", which is also what disables Update/Uninstall/Unlink for it.
export function pluginSourceOf(p: Plugin): PluginSourceInfo {
  const s = p.source
  if (s && typeof s === "object" && s.kind) return s
  return { kind: "local" }
}

const GH_PART = /^[A-Za-z0-9_.-]{1,100}$/

// githubRepoOf pulls owner/repo out of an install source in any of the forms
// the server accepts (owner/repo, owner/repo/sub/dir, a github.com URL), or
// null when it is none of them. Only used to build a link; lasso itself never
// contacts GitHub from the browser.
export function githubRepoOf(source: string | undefined): string | null {
  if (!source) return null
  let rest = source.trim()
  const url = rest.match(/^https:\/\/github\.com\/(.*)$/i)
  if (url) rest = url[1]
  const [owner, repo] = rest.split("/")
  if (!owner || !repo) return null
  const r = repo.replace(/\.git$/, "")
  for (const part of [owner, r]) {
    if (!GH_PART.test(part) || part === "." || part === "..") return null
  }
  return `${owner}/${r}`
}

// githubCommitURL is the commit page a github install is pinned to, or null
// when the source or commit cannot be read as one.
export function githubCommitURL(info: PluginSourceInfo): string | null {
  const repo = githubRepoOf(info.source)
  if (!repo || !info.commit || !/^[0-9a-f]{7,64}$/i.test(info.commit)) {
    return null
  }
  return `https://github.com/${repo}/commit/${info.commit}`
}

export function shortCommit(commit: string | undefined): string {
  return commit ? commit.slice(0, 7) : ""
}

// The icons a manifest may name. A curated set rather than all of lucide:
// importing every icon by name would put the whole library in the bundle for
// one glyph per plugin tab. Anything else — a typo, a name from a newer lucide —
// falls back to Puzzle; an icon is decoration and never an error.
const PLUGIN_ICONS: Record<string, LucideIcon> = {
  activity: Activity,
  bell: Bell,
  book: Book,
  "book-open": BookOpen,
  bot: Bot,
  box: Box,
  calendar: Calendar,
  chart: ChartBar,
  clock: Clock,
  cloud: Cloud,
  code: Code,
  cpu: Cpu,
  database: Database,
  "file-text": FileText,
  flask: FlaskConical,
  folder: Folder,
  gauge: Gauge,
  "git-branch": GitBranch,
  globe: Globe,
  hammer: Hammer,
  heart: Heart,
  image: Image,
  layers: Layers,
  dashboard: LayoutDashboard,
  link: Link,
  list: List,
  mail: Mail,
  map: MapIcon,
  message: MessageSquare,
  music: Music,
  notebook: NotebookPen,
  package: Package,
  puzzle: Puzzle,
  rocket: Rocket,
  search: Search,
  server: Server,
  shield: Shield,
  sparkles: Sparkles,
  star: Star,
  terminal: SquareTerminal,
  wrench: Wrench,
  zap: Zap,
}

export function pluginIcon(name: string | undefined): LucideIcon {
  if (name && Object.hasOwn(PLUGIN_ICONS, name)) return PLUGIN_ICONS[name]
  return Puzzle
}

// ---------------------------------------------------------------------------
// The tab bridge (protocol lasso-plugin/1)
// ---------------------------------------------------------------------------

// What the app knows about the focused pane, handed to a plugin as context.
// All of it is already on the human's screen; none of it is a credential.
export interface PluginPaneContext {
  host: string | null
  cwd: string | null
  cwd_host: string | null
  pane_id: string | null
}

interface BridgeOptions {
  frame: HTMLIFrameElement
  plugin: string
  tab: string
  // Read at call time, so an answer is about the pane focused NOW.
  context: () => PluginPaneContext
  // Whether this tab is what the human is looking at. file.open rearranges the
  // screen, and a tab nobody can see must not do that — the same rule an
  // agent's open_file follows with document visibility.
  onScreen: () => boolean
}

export interface PluginBridge {
  push: (event: "context" | "theme", data: unknown) => void
  pushContext: () => void
  pushAll: () => void
  dispose: () => void
}

// The message caps. A plugin is untrusted code, so every text it can put in
// front of the human is bounded, and so is the work it can queue.
const TOAST_MAX_CHARS = 200
const TOAST_BURST = 5
const TOAST_WINDOW_MS = 10_000
const MAX_IN_FLIGHT = 8

type Params = Record<string, unknown>

class BridgeError extends Error {}

function isRecord(v: unknown): v is Params {
  return typeof v === "object" && v !== null && !Array.isArray(v)
}

// agentAt names the agent in a pane from whatever fleet listing this tab has
// already fetched (the agents list and the chat's sidebar keep it warm). A
// plugin asking for context must not start a fleet-wide poll of its own, so a
// cold cache answers null rather than fetching.
function agentAt(host: string | null, pane: string | null): string | null {
  if (!host || !pane) return null
  let best: { at: number; name: string } | null = null
  for (const [key, data] of queryClient.getQueriesData<PanesPayload>({
    queryKey: qk.allPanesAny,
  })) {
    const row = data?.panes?.find(
      (p) => p.host === host && p.pane_id === pane && p.has_agent !== false
    )
    if (!row?.agent) continue
    const at = queryClient.getQueryState(key)?.dataUpdatedAt ?? 0
    if (!best || at > best.at) best = { at, name: agentName(row) }
  }
  return best?.name ?? null
}

// themeSnapshot is what theme.get answers and the "theme" push carries: the
// scheme and the palette's hexes. Colors only — see lib/theme.ts:paletteColors.
export function themeSnapshot() {
  return {
    dark: document.documentElement.classList.contains("dark"),
    colors: paletteColors(),
  }
}

// attachPluginBridge wires one plugin iframe to lasso.
//
// IDENTITY IS event.source. A sandboxed frame's origin is the string "null" —
// the same for every plugin — so an origin check tells nothing apart. The
// window a message came from does: each bridge answers only messages whose
// source is ITS frame's contentWindow (a WindowProxy that survives the frame
// navigating), which is what stops plugin A calling as plugin B, and a page A
// framed or opened from calling as A. The plugin name a request acts for comes
// from this closure, never from the message.
//
// Replies and pushes go out with targetOrigin "*", because an opaque origin
// cannot be named any other way. So nothing sent here may be secret: it is
// context the human can already see, a theme, and the plugin's own tool
// results.
export function attachPluginBridge(opts: BridgeOptions): PluginBridge {
  const { frame, plugin, tab } = opts
  let disposed = false
  let inFlight = 0
  const toastTimes: number[] = []

  const post = (msg: Record<string, unknown>) => {
    if (disposed) return
    frame.contentWindow?.postMessage({ lasso: 1, ...msg }, "*")
  }

  const context = () => {
    const c = opts.context()
    return {
      ...c,
      agent: agentAt(c.host, c.pane_id),
      plugin,
      tab,
    }
  }

  // The allowlist. A null-prototype object looked up with Object.hasOwn, so
  // "constructor", "__proto__" and friends are unknown methods, not calls.
  const methods: Record<string, (p: Params) => unknown> = Object.assign(
    Object.create(null),
    {
      "context.get": () => context(),
      "theme.get": () => themeSnapshot(),
      "file.open": (p: Params) => {
        const path = p.path
        if (typeof path !== "string" || !path.startsWith("/"))
          throw new BridgeError("path must be an absolute path")
        const line = p.line
        if (
          line !== undefined &&
          !(typeof line === "number" && Number.isInteger(line) && line > 0)
        )
          throw new BridgeError("line must be a positive integer")
        if (p.host !== undefined && (typeof p.host !== "string" || !p.host))
          throw new BridgeError("host must be a non-empty string")
        if (!opts.onScreen())
          throw new BridgeError(
            "file.open is only honoured while the tab is on screen"
          )
        const c = opts.context()
        const host =
          (p.host as string | undefined) ?? c.cwd_host ?? c.host ?? "local"
        // The agent path's own entry point, so a dirty editor is protected
        // exactly as it is from open_file: parked behind a toast, never
        // replaced. The toast names the plugin as who asked.
        const delivered = requestOpenFile({
          path,
          host,
          line: line as number | undefined,
          from: `Plugin ${plugin}`,
        })
        return { delivered }
      },
      "tool.call": async (p: Params) => {
        if (typeof p.tool !== "string" || !p.tool)
          throw new BridgeError("tool must be a non-empty string")
        const args = p.arguments ?? {}
        if (!isRecord(args))
          throw new BridgeError("arguments must be an object")
        // The plugin is this bridge's, not the message's: the server then
        // refuses any tool that is not that plugin's own.
        return api.pluginCall(plugin, p.tool, args)
      },
      toast: (p: Params) => {
        if (typeof p.message !== "string" || !p.message.trim())
          throw new BridgeError("message must be a non-empty string")
        const now = Date.now()
        while (toastTimes.length && now - toastTimes[0] > TOAST_WINDOW_MS)
          toastTimes.shift()
        if (toastTimes.length >= TOAST_BURST)
          throw new BridgeError("too many toasts; slow down")
        toastTimes.push(now)
        let text = p.message.trim()
        if (text.length > TOAST_MAX_CHARS)
          text = `${text.slice(0, TOAST_MAX_CHARS)}…`
        // A string child renders as text, never markup.
        toast(`${plugin}: ${text}`)
        return { ok: true }
      },
    }
  )

  const onMessage = async (e: MessageEvent) => {
    const win = frame.contentWindow
    if (disposed || !win || e.source !== win) return
    const d = e.data
    // Not ours (a plugin may use postMessage for its own purposes): ignore
    // rather than answer, since there is no id to answer to.
    if (!isRecord(d) || d.lasso !== 1 || "event" in d) return
    const id = d.id
    if (typeof id !== "string" && typeof id !== "number") return
    const method = d.method
    if (typeof method !== "string" || !Object.hasOwn(methods, method)) {
      post({ id, error: "unknown method" })
      return
    }
    const params = d.params ?? {}
    if (!isRecord(params)) {
      post({ id, error: "params must be an object" })
      return
    }
    if (inFlight >= MAX_IN_FLIGHT) {
      post({ id, error: "too many requests in flight" })
      return
    }
    inFlight++
    try {
      const result = await methods[method](params)
      post({ id, result: result ?? null })
    } catch (err) {
      post({ id, error: err instanceof Error ? err.message : String(err) })
    } finally {
      inFlight--
    }
  }

  window.addEventListener("message", onMessage)

  const push = (event: "context" | "theme", data: unknown) =>
    post({ event, data })

  return {
    push,
    pushContext: () => push("context", context()),
    pushAll: () => {
      push("context", context())
      push("theme", themeSnapshot())
    },
    dispose: () => {
      disposed = true
      window.removeEventListener("message", onMessage)
    },
  }
}
