import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

// In dev, Vite serves the SPA with HMR and proxies the data + terminal routes
// to the running Go server (default :8090; override with LASSO_BACKEND). SSE
// (/api/events, /api/livereload) rides the plain HTTP proxy; the ttyd terminals
// need WebSocket upgrade (ws: true). The Go server still embeds the production
// build (web/dist) for the non-dev binary.
const backend = process.env.LASSO_BACKEND || "http://127.0.0.1:8090"

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    // Reached over tailscale by whatever MagicDNS name the machine has, which
    // can change — so don't hardcode it. `true` accepts any Host header, which
    // is safe here because the dev server only listens on the private tailnet
    // interface (see --host in mise.toml), not a public one.
    allowedHosts: true,
    proxy: {
      "/api": { target: backend, changeOrigin: true },
      "/terminal": { target: backend, changeOrigin: true, ws: true },
      "/shell": { target: backend, changeOrigin: true, ws: true },
      // The Grid tab's per-pane terminals: each cell embeds a ttyd under
      // /grid-term/<token>/ (HTTP page + WebSocket). Without this, Vite's SPA
      // fallback would serve index.html and the cell would render a nested lasso.
      "/grid-term": { target: backend, changeOrigin: true, ws: true },
    },
  },
})
