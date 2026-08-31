// lasso's service worker. It exists for exactly one reason: a Web Push message
// can only be delivered to a service worker, and on iOS that is the only way to
// reach a phone whose screen is off (16.4+, and only for a lasso added to the
// Home Screen).
//
// It deliberately has NO fetch handler and caches nothing. lasso is a live view
// of machines you are driving — proxied terminals, SSE, cross-host pane polls —
// so an offline shell of it would be a lie, and a stale cached bundle in front
// of a self-updating binary is a support burden with no upside. Registered
// lazily by lib/push.ts when notifications are switched on, so a user who never
// opts in never installs one.
//
// Served from web/public, i.e. copied verbatim into the build. It is plain JS on
// purpose: no bundler, no imports, so what lands at /sw.js is what is written
// here.

// Take over immediately rather than waiting for every tab to close. Safe here
// precisely because there is no fetch handler: a new worker cannot serve a
// different asset version to a page than the one it booted with.
self.addEventListener("install", () => self.skipWaiting())
self.addEventListener("activate", (event) =>
  event.waitUntil(self.clients.claim())
)

// The payload is the encrypted JSON from webpush.go's webPushPayload:
// {kind, title, body, tag, host}. Everything needed to render is in it — the
// worker cannot call back into lasso, which behind Cloudflare Access would be
// an unauthenticated request that returns a login page.
self.addEventListener("push", (event) => {
  let data = {}
  try {
    data = event.data ? event.data.json() : {}
  } catch {
    // A payload we can't parse still has to raise something: the push
    // subscription is userVisibleOnly, so a push that shows no notification is
    // a strike against the origin and eventually loses the permission.
    data = {}
  }
  const title = data.title || "lasso"
  const options = {
    body: data.body || "",
    icon: "/favicon-192.png",
    badge: "/favicon-192.png",
    data: { host: data.host || "", kind: data.kind || "" },
  }
  // A tag COLLAPSES: a later notification carrying it replaces the earlier one.
  // The server sends one per pane for a blocked agent (repeated "still blocked"
  // is one entry), and none for a message an agent deliberately sent — two of
  // those are two things it said, and folding them would hide the first. So an
  // absent tag must stay absent rather than defaulting to a shared constant.
  //
  // renotify rides WITH the tag and only with it: it means "alert again when
  // you replace", and the spec REJECTS renotify:true with an empty tag — which
  // is not a warning but a rejected showNotification, i.e. a push that raises
  // nothing and eventually costs the origin its permission. Measured in Chrome
  // before this was split: every untagged notification silently vanished.
  if (data.tag) {
    options.tag = data.tag
    options.renotify = true
  }
  event.waitUntil(self.registration.showNotification(title, options))
})

// Opening the notification lands you in lasso, on the host the event happened
// on. It deliberately does NOT focus the agent's pane: herdr's focus is one
// global per session, shared with the TUI and every other lasso client, so a
// link that moved it would move it for everyone (see lib/url.ts).
self.addEventListener("notificationclick", (event) => {
  event.notification.close()
  const host = (event.notification.data && event.notification.data.host) || ""
  const target = new URL(host ? `/?host=${encodeURIComponent(host)}` : "/", self.location.origin)
  event.waitUntil(
    (async () => {
      const windows = await self.clients.matchAll({
        type: "window",
        includeUncontrolled: true,
      })
      for (const client of windows) {
        if (new URL(client.url).origin !== target.origin) continue
        // An open lasso is reused rather than duplicated. Pointing it at the
        // right host is best-effort: navigate() is unavailable in some
        // standalone contexts, and a focused lasso on the wrong host is still
        // far better than a second window.
        try {
          if (host && new URL(client.url).searchParams.get("host") !== host) {
            await client.navigate(target.href)
          }
        } catch {}
        return client.focus()
      }
      return self.clients.openWindow(target.href)
    })()
  )
})
