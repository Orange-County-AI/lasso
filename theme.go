package main

// Onyx is lasso's single, fixed design system (dark-only). The old herdr-derived
// 18-palette theme switcher — the themes map, settings-backed selection, the
// /api/theme(s) endpoints and the SSE live-repaint — is gone; UI colors are now
// static Onyx tokens baked into web/src/index.css.
//
// The terminal still needs an xterm.js ITheme. onyxXtermJSON is passed to ttyd at
// spawn (`-t theme=…`) so a terminal starts on the Onyx palette instead of
// flashing ttyd's default light theme before the browser re-applies it. Keep this
// in sync with ONYX_XTERM_THEME in web/src/lib/theme.ts (the browser re-pins the
// same palette across ttyd reconnects).
const onyxXtermJSON = `{` +
	`"background":"#06070c","foreground":"#f4f5fa",` +
	`"cursor":"#7b7fff","cursorAccent":"#06070c",` +
	`"selectionBackground":"rgba(123, 127, 255, 0.30)",` +
	`"black":"#11131c","red":"#f2545b","green":"#4ade9a","yellow":"#f2b144",` +
	`"blue":"#7b7fff","magenta":"#9498ff","cyan":"#9498ff","white":"#b7bbc8",` +
	`"brightBlack":"#3b3f4e","brightRed":"#f57b80","brightGreen":"#74e6a8",` +
	`"brightYellow":"#f6c674","brightBlue":"#9498ff","brightMagenta":"#b3a4ff",` +
	`"brightCyan":"#b3a4ff","brightWhite":"#f4f5fa"` +
	`}`
