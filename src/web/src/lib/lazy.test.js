import { beforeEach, describe, expect, mock, test } from "bun:test"

import { withChunkRecovery } from "@/lib/lazy"

let store
let reloads

beforeEach(() => {
  store = new Map()
  reloads = 0
  globalThis.sessionStorage = {
    getItem: (k) => store.get(k) ?? null,
    setItem: (k, v) => store.set(k, String(v)),
  }
  globalThis.window = {
    location: {
      reload: () => {
        reloads++
      },
    },
  }
})

// A loader that fails its first `fails` calls, then resolves.
const flaky = (fails, value = "chunk") => {
  let n = 0
  return mock(() =>
    n++ < fails
      ? Promise.reject(new Error("Failed to fetch dynamically imported module"))
      : Promise.resolve(value)
  )
}

describe("withChunkRecovery", () => {
  test("passes a successful load straight through", async () => {
    const load = flaky(0)
    expect(await withChunkRecovery(load)()).toBe("chunk")
    expect(load).toHaveBeenCalledTimes(1)
    expect(reloads).toBe(0)
  })

  test("a single transient failure recovers on the retry, without reloading", async () => {
    const load = flaky(1)
    expect(await withChunkRecovery(load)()).toBe("chunk")
    expect(load).toHaveBeenCalledTimes(2)
    expect(reloads).toBe(0)
  })

  test("a chunk that stays gone reloads the page once, and never settles", async () => {
    const load = flaky(Number.POSITIVE_INFINITY)
    let settled = false
    void withChunkRecovery(load)().then(
      () => {
        settled = true
      },
      () => {
        settled = true
      }
    )
    await Bun.sleep(5)
    expect(reloads).toBe(1)
    // The page is unloading; resolving or rejecting would paint a fallback in
    // the last frame before it goes.
    expect(settled).toBe(false)
  })

  test("does not reload twice — the second failure throws instead of looping", async () => {
    const load = flaky(Number.POSITIVE_INFINITY)
    void withChunkRecovery(load)()
    await Bun.sleep(5)
    expect(reloads).toBe(1)

    // The reload happened; the tab came back on the new bundle and the chunk
    // is STILL missing. That is not a stale bundle, so stop reloading.
    await expect(withChunkRecovery(load)()).rejects.toThrow(
      "Failed to fetch dynamically imported module"
    )
    expect(reloads).toBe(1)
  })

  test("the reload stamp expires, so a later staleness is recoverable again", async () => {
    store.set("lasso.chunk-reload", String(Date.now() - 61_000))
    const load = flaky(Number.POSITIVE_INFINITY)
    void withChunkRecovery(load)()
    await Bun.sleep(5)
    expect(reloads).toBe(1)
  })

  test("blocked storage refuses to reload rather than risk a loop", async () => {
    globalThis.sessionStorage = {
      getItem: () => {
        throw new Error("blocked")
      },
      setItem: () => {
        throw new Error("blocked")
      },
    }
    const load = flaky(Number.POSITIVE_INFINITY)
    await expect(withChunkRecovery(load)()).rejects.toThrow()
    expect(reloads).toBe(0)
  })
})
