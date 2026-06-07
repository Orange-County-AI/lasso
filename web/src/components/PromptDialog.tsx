import * as React from "react"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"

// A tiny single-field name prompt — our own modal in place of window.prompt, used
// for naming a new tab/workspace and renaming a tab. Controlled by the caller via
// open/onOpenChange; onSubmit fires with the trimmed value on Enter or the
// submit button. The field seeds from defaultValue each time it opens.
export function PromptDialog({
  open,
  onOpenChange,
  title,
  description,
  placeholder,
  defaultValue = "",
  submitLabel = "Create",
  onSubmit,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: string
  placeholder?: string
  defaultValue?: string
  submitLabel?: string
  onSubmit: (value: string) => void
}) {
  const [value, setValue] = React.useState(defaultValue)

  // Re-seed the field whenever the dialog (re)opens, not on every defaultValue
  // identity change while closed.
  React.useEffect(() => {
    if (open) setValue(defaultValue)
  }, [open, defaultValue])

  const trimmed = value.trim()
  const submit = () => {
    if (!trimmed) return
    onOpenChange(false)
    onSubmit(trimmed)
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description && <DialogDescription>{description}</DialogDescription>}
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            submit()
          }}
        >
          <Input
            autoFocus
            value={value}
            placeholder={placeholder}
            onChange={(e) => setValue(e.target.value)}
          />
          <DialogFooter className="mt-4">
            <Button
              type="button"
              variant="ghost"
              onClick={() => onOpenChange(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={!trimmed}>
              {submitLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
