import { describe, expect, test } from "bun:test"

import { api } from "@/lib/api"
import { terminalPasteHost } from "@/lib/terminal"

describe("terminalPasteHost", () => {
  test("keeps local terminal and shell pastes on the active host", () => {
    expect(terminalPasteHost("herdr", "local", "local")).toBe("local")
    expect(terminalPasteHost("shell", "local", "local")).toBe("local")
  })

  test("routes a native nested attach to its pane filesystem host", () => {
    expect(terminalPasteHost("herdr", "local", "ticket500")).toBe("ticket500")
    // The separate shell iframe still runs outside herdr on the active backend.
    expect(terminalPasteHost("shell", "local", "ticket500")).toBe("local")
  })

  test("falls back to the active host until focused-pane metadata arrives", () => {
    expect(terminalPasteHost("herdr", "ticket500", null)).toBe("ticket500")
    expect(terminalPasteHost("herdr", null, null)).toBeUndefined()
  })
})

test("pasteFile sends the selected terminal host and the file's name", async () => {
  const originalFetch = globalThis.fetch
  let requested = ""
  globalThis.fetch = (input) => {
    requested = String(input)
    return Promise.resolve(
      new Response(
        JSON.stringify({
          path: "/home/stephan/.lasso/uploads/dropped-files/notes.pdf",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } }
      )
    )
  }

  try {
    const result = await api.pasteFile(
      new Blob(["pdf bytes"], { type: "application/pdf" }),
      "ticket500",
      "Q3 notes.pdf"
    )
    expect(requested).toBe("/api/paste-file?name=Q3%20notes.pdf&host=ticket500")
    expect(result.path).toStartWith("/home/stephan/")
  } finally {
    globalThis.fetch = originalFetch
  }
})

test("pasteFile without a name leaves the server to stamp one", async () => {
  const originalFetch = globalThis.fetch
  let requested = ""
  globalThis.fetch = (input) => {
    requested = String(input)
    return Promise.resolve(
      new Response(JSON.stringify({ path: "/tmp/clipboard.png" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      })
    )
  }

  try {
    await api.pasteFile(new Blob(["png bytes"], { type: "image/png" }))
    expect(requested).toBe("/api/paste-file")
  } finally {
    globalThis.fetch = originalFetch
  }
})
