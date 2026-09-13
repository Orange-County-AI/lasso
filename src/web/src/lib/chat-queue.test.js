import { describe, expect, test } from "bun:test"

import { newestUserID, reconcileQueued } from "@/lib/chat-queue"

const user = (id) => ({ id, kind: "user", text: "hi" })
const agent = (id) => ({ id, kind: "agent", text: "hello" })

describe("newestUserID", () => {
  test("is the last user turn, not the last row", () => {
    expect(newestUserID([user("u1"), agent("a1"), agent("a2")])).toBe("u1")
  })

  test("is empty for a transcript with no user turn", () => {
    expect(newestUserID([agent("a1"), agent("a2")])).toBe("")
    expect(newestUserID([])).toBe("")
  })
})

describe("reconcileQueued", () => {
  let seq = 0
  const echo = (after) => ({
    id: seq++,
    target: "h\u0000p1",
    text: "hello",
    after,
  })

  test("keeps a message the transcript has not written yet", () => {
    const prev = [echo("u1")]
    expect(reconcileQueued(prev, "h\u0000p1", "u1")).toBe(prev)
  })

  test("retires it once a later user turn lands", () => {
    expect(reconcileQueued([echo("u1")], "h\u0000p1", "u2")).toEqual([])
  })

  test("retires two pending sends one apiece, not both at once", () => {
    // Two messages sent before either landed both snapshot the same turn, so a
    // single landing must not clear both — the second would go missing until the
    // transcript caught up with it, which is the bug the echo exists to fix.
    const afterFirst = reconcileQueued([echo("u1"), echo("u1")], "h\u0000p1", "u2")
    expect(afterFirst).toHaveLength(1)
    expect(afterFirst[0].after).toBe("u2")
    expect(reconcileQueued(afterFirst, "h\u0000p1", "u3")).toEqual([])
  })

  test("leaves another pane's echoes alone", () => {
    const other = { id: 99, target: "h\u0000p9", text: "elsewhere", after: "u1" }
    expect(reconcileQueued([other], "h\u0000p1", "u2")).toEqual([other])
    // …and it survives while its own pane's echo is retired.
    expect(reconcileQueued([echo("u1"), other], "h\u0000p1", "u2")).toEqual([other])
  })
})
