import { type GridPane, api } from "@/lib/api"
import { focusHerdrTerminal } from "@/lib/terminal"

// Open + focus a pane in the Herdr tab. If it's on another host, switch there
// first (which reloads the Herdr terminal onto that host), then focus its tab.
// Release the pane's grid terminal *before* surfacing Herdr so the only client
// left on the pane is the full-width Herdr terminal — otherwise herdr keeps the
// pane clamped to the grid cell's narrow width and a full-screen TUI renders
// thin. surfaceHerdr() switches the left view to the Herdr tab (callers push a
// history entry so Back returns to where they came from). Finally hand the
// keyboard to xterm so the user can type without clicking first.
//
// Shared by the Grid tab (header click) and the Cmd+U pane switcher.
export async function focusPaneInHerdr(
  p: GridPane,
  activeHost: string | null,
  surfaceHerdr: () => void
) {
  if (p.host !== activeHost) await api.switchHost(p.host)
  if (p.workspace_id && p.tab_id) await api.focus(p.workspace_id, p.tab_id)
  await api.gridTermRelease(p.host, p.terminal_id)
  surfaceHerdr()
  focusHerdrTerminal()
}
