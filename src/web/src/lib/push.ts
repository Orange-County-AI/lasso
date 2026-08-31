// Web Push, browser side.
//
// The whole reason this exists: on iOS, a notification can only reach a phone
// whose screen is off if the site is a Home Screen web app with a service
// worker holding a push subscription (iOS 16.4+). So the flow is necessarily
// three-legged — permission from a user gesture, a service worker registration,
// and a subscription handed to the server (lib/api.ts → /api/push/subscribe,
// stored by webpush.go).
//
// Everything here is careful about one thing: the browser and the server each
// hold half of the state. The browser owns the subscription (and can drop it
// when permission is revoked or its keys rotate); the server owns the row it
// pushes to. They are reconciled by re-posting the subscription every time the
// UI reads its state, which is cheap and idempotent (the endpoint is the
// primary key) and repairs a server whose db was restored from a backup.

import { api } from "./api"

// Why notifications may be unavailable — each needs a different sentence in the
// UI, so they are distinct rather than a single boolean.
export type PushSupport =
  // Push works here.
  | "ok"
  // iOS Safari in a normal tab: Apple only exposes push to Home Screen web
  // apps, so the user has to add lasso to their Home Screen first.
  | "needs-home-screen"
  // No service worker / PushManager at all (an old browser, or a non-secure
  // origin — the APIs are secure-context only, which https and localhost are).
  | "unsupported"

export interface PushState {
  support: PushSupport
  permission: NotificationPermission
  subscribed: boolean
}

const SW_URL = "/sw.js"

// iOS is the only platform that gates push behind installation, and it is the
// case this feature was built for, so it gets an explicit check. iPadOS reports
// itself as a Mac, hence the touch-point test.
function isIOS(): boolean {
  const ua = navigator.userAgent
  if (/iPad|iPhone|iPod/.test(ua)) return true
  return navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1
}

// Standalone = launched from the Home Screen rather than a browser tab.
// `navigator.standalone` is Apple's original signal and is still the reliable
// one on iOS; display-mode covers everything else.
function isStandalone(): boolean {
  // Not in the DOM typings (it is Apple's own), so it is narrowed rather than
  // asserted: `in` makes the read checked and typed unknown.
  if ("standalone" in navigator && navigator.standalone === true) return true
  return window.matchMedia("(display-mode: standalone)").matches
}

export function pushSupport(): PushSupport {
  const hasAPIs =
    "serviceWorker" in navigator &&
    "PushManager" in window &&
    "Notification" in window
  if (!hasAPIs) return "unsupported"
  if (isIOS() && !isStandalone()) return "needs-home-screen"
  return "ok"
}

// readPushState reports what the browser side looks like right now, without
// registering a worker or asking for permission — so opening Settings never
// prompts and never installs anything.
export async function readPushState(): Promise<PushState> {
  const support = pushSupport()
  if (support === "unsupported") {
    return { support, permission: "denied", subscribed: false }
  }
  const permission = Notification.permission
  const reg = await navigator.serviceWorker.getRegistration(SW_URL)
  const sub = await reg?.pushManager.getSubscription()
  if (sub && permission === "granted") {
    // Re-announce this device. The browser owns the subscription and the server
    // owns the row it pushes to, and the two can drift — a 410 from the push
    // service prunes the row, a restored db loses it — leaving the checkbox
    // showing "on" while lasso pushes nowhere. The upsert is keyed on the
    // endpoint, so this is idempotent, and a failure just leaves the state as
    // it was rather than blocking the read.
    await api
      .pushSubscribe(sub.toJSON(), window.location.origin)
      .catch(() => {})
  }
  return { support, permission, subscribed: !!sub }
}

// enablePush asks for permission, installs the worker, subscribes, and hands
// the subscription to lasso. MUST be called straight from a user gesture:
// Safari rejects requestPermission otherwise, and on iOS that rejection is
// silent enough to look like a bug.
export async function enablePush(publicKey: string): Promise<PushState> {
  const support = pushSupport()
  if (support === "needs-home-screen") {
    throw new Error(
      "On iOS, notifications only work once lasso is added to your Home Screen — open the Share sheet and choose “Add to Home Screen”, then turn this on from there."
    )
  }
  if (support === "unsupported") {
    throw new Error("This browser can't do Web Push notifications.")
  }
  const permission = await Notification.requestPermission()
  if (permission !== "granted") {
    throw new Error(
      permission === "denied"
        ? "Notifications are blocked for this site — allow them in your browser's site settings, then try again."
        : "Notification permission wasn't granted."
    )
  }
  const reg = await navigator.serviceWorker.register(SW_URL, { scope: "/" })
  // A registration that is still installing has no usable pushManager yet.
  await navigator.serviceWorker.ready
  const key = decodeKey(publicKey)
  let sub = await reg.pushManager.getSubscription()
  // A subscription pinned to a DIFFERENT server key (lasso's db was reset, or
  // this browser was pointed at another lasso) cannot be reused: subscribe()
  // would throw InvalidStateError. Drop it and mint a new one.
  if (sub && !sameKey(sub, key)) {
    await sub.unsubscribe()
    sub = null
  }
  if (!sub) {
    sub = await reg.pushManager.subscribe({
      // Required by every browser, and on iOS the promise silently never
      // resolves usefully without it: every push must raise a notification.
      userVisibleOnly: true,
      applicationServerKey: key,
    })
  }
  await api.pushSubscribe(sub.toJSON(), window.location.origin)
  return { support, permission, subscribed: true }
}

// disablePush drops both halves: the browser's subscription and lasso's row.
// The server row goes first — if the local unsubscribe succeeded and the POST
// then failed, lasso would keep pushing to an endpoint nothing listens on until
// the push service returned 410.
export async function disablePush(): Promise<PushState> {
  const reg = await navigator.serviceWorker.getRegistration(SW_URL)
  const sub = await reg?.pushManager.getSubscription()
  if (sub) {
    await api.pushUnsubscribe(sub.endpoint)
    await sub.unsubscribe()
  }
  return {
    support: pushSupport(),
    permission: Notification.permission,
    subscribed: false,
  }
}

// decodeKey turns the server's base64url VAPID public key into the raw 65-byte
// point the Push API wants. (Browsers accept only a BufferSource here — the
// base64 string that every server hands out is not a valid argument.) The
// buffer is allocated explicitly so the type is ArrayBuffer-backed: a
// SharedArrayBuffer-backed view is not a valid applicationServerKey.
function decodeKey(base64url: string): Uint8Array<ArrayBuffer> {
  const padded = base64url.replace(/-/g, "+").replace(/_/g, "/")
  const raw = atob(padded + "=".repeat((4 - (padded.length % 4)) % 4))
  const out = new Uint8Array(new ArrayBuffer(raw.length))
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i)
  return out
}

// sameKey compares an existing subscription's pinned application server key
// against ours. Browsers hand it back as an ArrayBuffer, so the comparison is
// byte-wise.
function sameKey(sub: PushSubscription, key: Uint8Array): boolean {
  const pinned = sub.options.applicationServerKey
  if (!pinned) return false
  const bytes = new Uint8Array(pinned)
  if (bytes.length !== key.length) return false
  return bytes.every((b, i) => b === key[i])
}
