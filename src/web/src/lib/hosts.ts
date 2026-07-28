import type { HostInfo } from "@/lib/api"

// Grouping for the ssh-host pickers (the nav HostSwitcher and the new-agent
// dialog). Hosts cluster two ways:
//
//   1. By alias family — `visiquate-stephan` / `visiquate-jessica` share the
//      "visiquate" prefix, the convention for several accounts (or agents) on
//      one box. The prefix names the group; the part after the first "-" names
//      the member. A bare `visiquate` alias joins its own family.
//   2. By the physical box an alias resolves to, for everything else, so plain
//      aliases pointing at one HostName still cluster.
//
// Loopback aliases (HostName localhost/127.*/::1) are the very machine lasso
// runs on, so they fold under the local host rather than forming their own
// group.

// Synthetic key for the local machine. NUL-prefixed so it can never collide
// with a real hostname or alias (neither can contain one) — it shares the group
// map with resolved hostnames.
export const LOCAL_KEY = "\u0000local"

export function isLoopback(host?: string): boolean {
  if (!host) return false
  const h = host.toLowerCase()
  return h === "localhost" || h === "::1" || h.startsWith("127.")
}

// splitAlias splits "visiquate-jessica" into the family name and the member
// name shown inside it. Returns null when there's no "-", or when it sits at
// either end (a split would leave an empty half, so the alias isn't a family
// member — it's just a name with a dash).
export function splitAlias(
  alias: string
): { family: string; member: string } | null {
  const i = alias.indexOf("-")
  if (i <= 0 || i === alias.length - 1) return null
  return { family: alias.slice(0, i), member: alias.slice(i + 1) }
}

// families returns the alias prefixes worth grouping on: a prefix shared by two
// or more dashed aliases, or one dashed alias plus the bare alias it extends
// (`visiquate` + `visiquate-jessica`). A lone `norm-game` forms no family, so
// it keeps grouping by its physical host as before instead of splitting off
// into a group of one.
function families(hosts: readonly HostInfo[]): ReadonlySet<string> {
  const counts = new Map<string, number>()
  const bare = new Set<string>()
  for (const h of hosts) {
    const s = splitAlias(h.alias)
    if (s) counts.set(s.family, (counts.get(s.family) ?? 0) + 1)
    else bare.add(h.alias)
  }
  const out = new Set<string>()
  for (const [name, n] of counts) {
    if (n > 1 || bare.has(name)) out.add(name)
  }
  return out
}

// memberLabel names a host inside a group: the part after its alias's first "-"
// for a family member, else the ssh user (the group header already names the
// box), else the alias.
export function memberLabel(h: HostInfo, fams: ReadonlySet<string>): string {
  const s = splitAlias(h.alias)
  if (s && fams.has(s.family)) return s.member
  return h.user || h.alias
}

export interface HostGroup {
  // LOCAL_KEY for the local machine, else the family name or resolved hostname.
  key: string
  // Display name for the group header/submenu trigger.
  label: string
  hosts: HostInfo[]
}

export interface GroupedHosts {
  groups: HostGroup[]
  // Aliases that fold under the local machine (loopback), split out because the
  // local session row leads that group.
  localMates: HostInfo[]
  // The alias families this grouping keyed on — pass to memberLabel.
  families: ReadonlySet<string>
}

// groupHosts buckets the remote hosts, preserving the order they arrived in
// (both for the groups themselves and within each group).
export function groupHosts(hosts: readonly HostInfo[]): GroupedHosts {
  const fams = families(hosts)
  const key = (h: HostInfo): string => {
    if (isLoopback(h.hostname)) return LOCAL_KEY
    const s = splitAlias(h.alias)
    if (s && fams.has(s.family)) return s.family
    if (fams.has(h.alias)) return h.alias
    // Fall back to the alias as its own key when the resolver returned no
    // hostname, so an unresolved alias still renders (as a lone flat row).
    return h.hostname || h.alias
  }
  const groups: HostGroup[] = []
  const byKey = new Map<string, HostGroup>()
  const localMates: HostInfo[] = []
  for (const h of hosts) {
    const k = key(h)
    if (k === LOCAL_KEY) {
      localMates.push(h)
      continue
    }
    let g = byKey.get(k)
    if (!g) {
      g = { key: k, label: k, hosts: [] }
      byKey.set(k, g)
      groups.push(g)
    }
    g.hosts.push(h)
  }
  return { groups, localMates, families: fams }
}
