import * as React from "react"
import { toast } from "sonner"

import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { api } from "@/lib/api"
import { focusHerdrTerminal } from "@/lib/terminal"

// NewTerminalDialog: ⌘I → a single-field prompt for a workspace name that spins
// up a bare herdr terminal (no agent) and drops the user straight into it. The
// name field auto-focuses; Enter creates, Esc cancels. The backend focuses the
// new workspace in herdr server-side, so on success we only surface the Herdr
// tab and hand the keyboard to its terminal — the user can type commands at once.
export function NewTerminalDialog({
  open,
  onOpenChange,
  surfaceHerdr,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  surfaceHerdr: () => void
}) {
  const [name, setName] = React.useState("")
  const [creating, setCreating] = React.useState(false)
  // Set when the dialog closes because a terminal was just created, so the close
  // handler hands keyboard focus to the herdr terminal instead of letting Radix
  // restore it to whatever was focused before (which would steal the keyboard
  // back from the new pane). Mirrors CreateAgentDialog.
  const createdRef = React.useRef(false)

  // Reset the field + state each time the modal opens so it starts fresh.
  React.useEffect(() => {
    if (open) {
      setName("")
      setCreating(false)
    }
  }, [open])

  const create = async () => {
    if (creating) return
    setCreating(true)
    try {
      await api.createTerminal(name.trim())
      // The backend already focused the new workspace in herdr; surface the
      // Herdr tab, then close — onCloseAutoFocus hands the keyboard to its
      // terminal once Radix has finished unmounting the dialog.
      createdRef.current = true
      surfaceHerdr()
      onOpenChange(false)
    } catch (e) {
      toast.error(`Failed to create terminal: ${(e as Error).message}`)
      setCreating(false)
    }
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault()
      onOpenChange(false)
    } else if (e.key === "Enter") {
      e.preventDefault()
      create()
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        showCloseButton={false}
        className="top-[15%] translate-y-0 gap-0 p-0 sm:max-w-md"
        onOpenAutoFocus={(e) => {
          // Focus the name input rather than the dialog container.
          e.preventDefault()
          ;(e.currentTarget as HTMLElement | null)
            ?.querySelector("input")
            ?.focus()
        }}
        onCloseAutoFocus={(e) => {
          // On a create-driven close, send focus to the herdr terminal so the
          // user can type into the new pane immediately. Otherwise (cancel / Esc)
          // let Radix restore focus as usual.
          if (createdRef.current) {
            createdRef.current = false
            e.preventDefault()
            focusHerdrTerminal()
          }
        }}
      >
        <DialogHeader className="sr-only">
          <DialogTitle>New terminal</DialogTitle>
        </DialogHeader>
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={onKeyDown}
          disabled={creating}
          placeholder="Name this terminal, then press Enter…"
          className="w-full bg-transparent px-4 py-3 text-sm outline-none placeholder:text-muted-foreground disabled:opacity-50"
        />
      </DialogContent>
    </Dialog>
  )
}
