import { api, type GridPane } from "@/lib/api"

// Focusing a grid pane in the current single-viewport model: switch to its host
// if needed (so the sidebar tree re-scopes and contains the tab), then select its
// tab — the main center viewport (TabTerminal) points itself at that session,
// attaching the remote one over `ssh -tt` when the host is remote. Release the
// pane's own grid ttyd so we don't keep an extra attach alive once it's promoted
// to the main viewport.
export async function focusGridPane(
  p: GridPane,
  activeHost: string | null,
  selectTab: (tabId: string) => void
) {
  if (p.host !== (activeHost ?? "local")) {
    await api.switchHost(p.host)
  }
  void api.gridTermRelease(p.host, p.tab_id)
  selectTab(p.tab_id)
}
