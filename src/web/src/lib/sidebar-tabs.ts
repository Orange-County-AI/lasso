import type { PluginTabInfo, SidebarTabPref } from "@/lib/api"

// Which tabs the right sidebar shows, and in what order: lasso's built-ins
// plus every tab an enabled plugin provides, arranged by ui_state.sidebar_tabs.
//
// The stored list is the human's arrangement, not an inventory, so it is
// reconciled against what exists every time rather than trusted:
//
//   - An id nothing provides right now is KEPT in the stored list but not
//     rendered. A plugin disabled for an afternoon (or one whose manifest
//     changed and is waiting for re-approval) must come back where it was, and
//     dropping its entry on the first render without it would forget that.
//   - A tab that exists but is missing from the list is placed as if the list
//     had always had it: a built-in in its default position (after the nearest
//     built-in that precedes it by default), a plugin tab at the end, just
//     before Settings. So a lasso update adding a built-in, or a new plugin,
//     never lands somewhere arbitrary — and an empty list is the default order.
//   - Settings can never be hidden: it is where the switch to unhide
//     everything else lives. The server normalizes it too.

// The built-ins in their default order. Agents leads because below md this
// panel IS a phone's chrome; at md+ its tab is not rendered at all (App.tsx
// keeps it md:hidden whatever the arrangement says).
export const BUILTIN_TABS = [
  "agents",
  "files",
  "scratch",
  "browser",
  "terminal",
  "usage",
  "settings",
] as const

export type BuiltinTab = (typeof BUILTIN_TABS)[number]

export const BUILTIN_LABELS: Record<BuiltinTab, string> = {
  agents: "Agents",
  files: "Files",
  scratch: "Scratch",
  browser: "Browser",
  terminal: "Terminal",
  usage: "Usage",
  settings: "Settings",
}

export function isBuiltinTab(id: string): id is BuiltinTab {
  return (BUILTIN_TABS as readonly string[]).includes(id)
}

// arrangeTabs merges the stored list with what exists into ONE ordered list
// that still carries the stored entries nothing provides. That superset is
// what the Settings editor reorders — moving two visible rows swaps them in
// place, so an absent plugin's entry keeps its slot between them — and what it
// writes back.
export function arrangeTabs(
  stored: SidebarTabPref[] | undefined,
  pluginTabs: PluginTabInfo[]
): SidebarTabPref[] {
  const out: SidebarTabPref[] = []
  const seen = new Set<string>()
  for (const p of stored ?? []) {
    if (!p || typeof p.id !== "string" || seen.has(p.id)) continue
    seen.add(p.id)
    out.push({ id: p.id, hidden: p.id === "settings" ? false : !!p.hidden })
  }
  // Missing built-ins, in default order, each after the nearest default
  // predecessor already placed (or first, when none is).
  BUILTIN_TABS.forEach((id, i) => {
    if (seen.has(id)) return
    let at = 0
    for (let j = i - 1; j >= 0; j--) {
      const k = out.findIndex((e) => e.id === BUILTIN_TABS[j])
      if (k >= 0) {
        at = k + 1
        break
      }
    }
    out.splice(at, 0, { id, hidden: false })
    seen.add(id)
  })
  // Missing plugin tabs: at the end, before Settings, in listing order.
  for (const t of pluginTabs) {
    if (seen.has(t.global_id)) continue
    const s = out.findIndex((e) => e.id === "settings")
    out.splice(s >= 0 ? s : out.length, 0, { id: t.global_id, hidden: false })
    seen.add(t.global_id)
  }
  return out
}

// resolveSidebarTabs is the arrangement narrowed to what exists: every real
// tab in order, hidden ones included (the Settings editor lists those too).
// App renders the ones with hidden=false.
export function resolveSidebarTabs(
  stored: SidebarTabPref[] | undefined,
  pluginTabs: PluginTabInfo[]
): SidebarTabPref[] {
  const real = new Set<string>([
    ...BUILTIN_TABS,
    ...pluginTabs.map((t) => t.global_id),
  ])
  return arrangeTabs(stored, pluginTabs).filter((e) => real.has(e.id))
}

// moveTab swaps a real tab with its visible-list neighbour (`dir` -1 = up) in
// the full arrangement, leaving every stored-but-absent entry where it was.
// Returns null when there is no neighbour that way.
export function moveTab(
  arrangement: SidebarTabPref[],
  real: SidebarTabPref[],
  id: string,
  dir: -1 | 1
): SidebarTabPref[] | null {
  const i = real.findIndex((e) => e.id === id)
  const other = real[i + dir]
  if (i < 0 || !other) return null
  const next = arrangement.slice()
  const a = next.findIndex((e) => e.id === id)
  const b = next.findIndex((e) => e.id === other.id)
  ;[next[a], next[b]] = [next[b], next[a]]
  return next
}

// setTabHidden flips one entry's hidden flag. Settings is refused here as well
// as on the server, so the editor cannot offer a state that will not stick.
export function setTabHidden(
  arrangement: SidebarTabPref[],
  id: string,
  hidden: boolean
): SidebarTabPref[] {
  if (id === "settings") return arrangement
  return arrangement.map((e) => (e.id === id ? { ...e, hidden } : e))
}

// fallbackTab is where the sidebar goes when the selected tab stops existing
// (hidden, or its plugin disabled): Files if it is showing, otherwise the
// first visible tab that can actually render at this width — Agents is
// md:hidden, so at md+ picking it would leave a pane with no lit trigger (and
// App's breakpoint guard would bounce straight back here). Settings cannot be
// hidden, so there is always an answer.
export function fallbackTab(visible: string[], wide: boolean): string {
  if (visible.includes("files")) return "files"
  return visible.find((id) => !(wide && id === "agents")) ?? "settings"
}
