import { describe, expect, test } from "bun:test"

import { isChunkLoadError } from "@/lib/chunk"

// The three engines' wordings for the same failure. Matching only Chromium's
// would leave a Safari reader (which is most of lasso's phone traffic) with the
// cryptic message this classification exists to replace.
describe("isChunkLoadError", () => {
  test("recognizes each engine's module-load failure", () => {
    for (const msg of [
      "Failed to fetch dynamically imported module: http://x/assets/mermaid-abc.js",
      "error loading dynamically imported module: http://x/assets/mermaid-abc.js",
      "Importing a module script failed.",
    ])
      expect(isChunkLoadError(new Error(msg))).toBe(true)
  })

  test("is case-insensitive", () => {
    expect(isChunkLoadError(new Error("FAILED TO FETCH DYNAMICALLY IMPORTED MODULE"))).toBe(
      true
    )
  })

  // The point of the classification: a diagram lasso could not parse must keep
  // reporting the parse error, not offer a reload that fixes nothing.
  test("leaves a mermaid parse error alone", () => {
    expect(
      isChunkLoadError(new Error("Parse error on line 2:\n  grph TD\n  ^"))
    ).toBe(false)
  })

  test("survives a non-Error throw", () => {
    expect(isChunkLoadError("Failed to fetch dynamically imported module")).toBe(
      true
    )
    expect(isChunkLoadError(undefined)).toBe(false)
    expect(isChunkLoadError({ message: "whatever" })).toBe(false)
  })
})
