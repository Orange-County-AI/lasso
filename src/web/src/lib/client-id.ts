// clientID identifies this TAB to the server's ownership claims — the sidebar
// layout (uilock.go's claimLayout) and the shared terminal's size (claimTerm) —
// and is what /api/clients lists a connected browser by.
//
// Per tab, not per browser: two tabs on one machine are two clients that can
// disagree, and sessionStorage is per tab by construction, the same reason the
// tab's host lives there (lib/host.ts). It survives a reload, which is what
// keeps a refreshing tab from silently dropping a lock it was holding.
//
// It lives in its own module rather than in lib/ui-state.ts because the terminal
// resize gate needs it too, and lib/terminal.ts has no business importing the
// ui-state write pipeline to learn its own name.

const CLIENT_KEY = "lasso-client-id"

let cachedClientID: string | null = null

// crypto.randomUUID exists only in a secure context, and lasso is routinely
// reached over plain http on a tailnet address — which is not one. Falling back
// to Math.random is fine here: this id only has to be unlikely to collide with
// the handful of other tabs open on the same server, and it authorizes nothing.
function newClientID(): string {
  try {
    return crypto.randomUUID()
  } catch {
    return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
  }
}

export function clientID(): string {
  if (cachedClientID) return cachedClientID
  let id = ""
  try {
    id = sessionStorage.getItem(CLIENT_KEY) ?? ""
    if (!id) {
      id = newClientID()
      sessionStorage.setItem(CLIENT_KEY, id)
    }
  } catch {
    // Private mode / storage disabled: a per-load id still distinguishes this
    // tab from the others for as long as it is open, which is all the claim
    // needs. It just can't survive a reload.
    id = newClientID()
  }
  cachedClientID = id
  return id
}
