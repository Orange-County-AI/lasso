import { QueryClientProvider } from "@tanstack/react-query"
import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import "./index.css"
import { App } from "@/App"
import { ErrorBoundary } from "@/components/ErrorBoundary"
import { queryClient } from "@/lib/query"

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      {/* A render error with nothing to catch it unmounts the whole tree, and
          a blank window says nothing about what happened or what to do. */}
      <ErrorBoundary
        label="root"
        fallback={(err) => (
          <div className="flex h-screen flex-col items-center justify-center gap-3 p-6 text-center text-muted-foreground text-xs">
            <div className="text-foreground text-sm">lasso hit an error.</div>
            <pre className="max-w-full overflow-auto text-[11px] opacity-70">
              {err.message}
            </pre>
            <button
              type="button"
              className="rounded border border-border px-3 py-1 hover:bg-accent"
              onClick={() => window.location.reload()}
            >
              reload
            </button>
          </div>
        )}
      >
        <App />
      </ErrorBoundary>
    </QueryClientProvider>
  </StrictMode>
)
