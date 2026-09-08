import * as React from "react"

// React unmounts the whole tree when a render error reaches the root, so
// without a boundary anywhere one broken subtree blanks the entire app —
// which is what a 404'd lazy chunk did to the file viewer. A boundary keeps
// the failure the size of the thing that failed and offers the reload that
// fixes it.

type Props = {
  children: React.ReactNode
  // What to show in place of the subtree. Given the error and a reload, so a
  // caller can size the message to whatever it wrapped.
  fallback?: (err: Error, reload: () => void) => React.ReactNode
  // Where this boundary sits, for the console line. The error itself says
  // nothing about which subtree raised it.
  label?: string
}

type State = { err: Error | null }

export class ErrorBoundary extends React.Component<Props, State> {
  state: State = { err: null }

  static getDerivedStateFromError(err: Error): State {
    return { err }
  }

  componentDidCatch(err: Error, info: React.ErrorInfo) {
    console.error(`[${this.props.label ?? "app"}] render error`, err, info)
  }

  // Remounting the subtree is worth offering even though a reload is the
  // surer fix: a transient failure (a fetch that dropped) recovers without
  // losing terminal state, and the reload button is right there if it doesn't.
  private retry = () => this.setState({ err: null })

  render() {
    const { err } = this.state
    if (!err) return this.props.children
    if (this.props.fallback) return this.props.fallback(err, this.retry)
    return (
      <div className="flex flex-col items-center justify-center gap-2 p-4 text-center text-muted-foreground text-xs">
        <div>something broke here.</div>
        <button
          type="button"
          className="rounded border border-border px-2 py-1 hover:bg-accent"
          onClick={() => window.location.reload()}
        >
          reload
        </button>
      </div>
    )
  }
}
