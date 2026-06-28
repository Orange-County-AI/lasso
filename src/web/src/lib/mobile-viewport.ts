// On iOS the on-screen keyboard OVERLAYS the page instead of shrinking the
// layout viewport, so our 100%-height app keeps its bottom rows — the terminal's
// input line (Claude Code's composer) — hidden behind the keyboard. There's no
// scroll-into-view escape hatch either: that row lives inside the terminal
// iframe's alt-screen, not the parent DOM.
//
// `window.visualViewport` DOES track the region actually visible above the
// keyboard. So we pin the document height to it and counter iOS's habit of
// scrolling the layout viewport up when the keyboard opens. Shrinking the app
// shrinks the terminal iframe, and ttyd/xterm auto-refit to the smaller box —
// the prompt ends up just above the keyboard, visible. On desktop this is a
// no-op (visualViewport.height ≈ innerHeight). Returns a disposer.
export function syncViewportHeight(): () => void {
  const vv = window.visualViewport
  if (!vv) return () => {}
  const root = document.documentElement
  const apply = () => {
    // Height of the area above the keyboard (full screen when it's closed).
    const h = Math.round(vv.height)
    root.style.height = `${h}px`
    // Expose it as a var so fixed-position overlays (e.g. the fullscreen ⌘K
    // sheet) can size to the visible area too — they're not constrained by the
    // documentElement height since fixed positioning is viewport-relative.
    root.style.setProperty("--vvh", `${h}px`)
    // iOS scrolls the layout viewport up to reveal the focused field when the
    // keyboard opens; undo it so the app stays pinned to the visible region's
    // top. Guarded so the forced scroll can't loop against our own scroll event.
    if (vv.offsetTop !== 0 || window.scrollY !== 0) window.scrollTo(0, 0)
  }
  apply()
  vv.addEventListener("resize", apply)
  vv.addEventListener("scroll", apply)
  return () => {
    vv.removeEventListener("resize", apply)
    vv.removeEventListener("scroll", apply)
    root.style.height = "" // back to the stylesheet's 100%
    root.style.removeProperty("--vvh")
  }
}
