import { RotateCw } from "lucide-react"
import * as React from "react"
import { Pill } from "@/components/Pill"
import { Button } from "@/components/ui/button"
import { api, type VersionInfo } from "@/lib/api"

// The Settings tab: herdr's installed + latest version. When an update is
// available the latest pill becomes a shortcut that opens the Terminal tab with
// `herdr update` pre-typed (handled by the parent via onOpenUpdate).
export function SettingsView({
  active,
  onOpenUpdate,
}: {
  active: boolean
  onOpenUpdate: () => void
}) {
  const [info, setInfo] = React.useState<VersionInfo | null>(null)
  const [state, setState] = React.useState<"idle" | "loading" | "error">("idle")
  const loadedOnce = React.useRef(false)

  const load = React.useCallback(async () => {
    setState("loading")
    try {
      setInfo(await api.version())
      setState("idle")
    } catch {
      setInfo(null)
      setState("error")
    }
  }, [])

  // Lazily load on first open, like the original initSettings().
  React.useEffect(() => {
    if (active && !loadedOnce.current) {
      loadedOnce.current = true
      load()
    }
  }, [active, load])

  const loading = state === "loading"
  const errored = state === "error"

  let latest: React.ReactNode
  if (loading) {
    latest = <Pill>latest …</Pill>
  } else if (errored || !info) {
    latest = <Pill>latest unavailable</Pill>
  } else if (info.latest) {
    const suffix = info.update_available
      ? " · update available"
      : " · up to date"
    latest = (
      <Pill
        tone={info.update_available ? "warn" : "good"}
        clickable={info.update_available}
        onClick={info.update_available ? onOpenUpdate : undefined}
        title={
          info.update_available
            ? "open the Terminal with `herdr update` ready (press Enter to run)"
            : undefined
        }
      >
        latest {info.latest}
        {suffix}
      </Pill>
    )
  } else {
    latest = <Pill title={info.latest_error || ""}>latest unavailable</Pill>
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="border-border border-b bg-background px-3 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="mr-0.5 text-muted-foreground text-xs tracking-wide">
            herdr
          </span>
          <Pill>
            installed{" "}
            {loading
              ? "…"
              : errored || !info
                ? "unavailable"
                : info.installed || "unknown"}
          </Pill>
          {latest}
          <Button
            variant="outline"
            size="icon"
            className="ml-auto size-7"
            title="check for updates"
            onClick={load}
          >
            <RotateCw />
          </Button>
        </div>
      </header>
    </div>
  )
}
