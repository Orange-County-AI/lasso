"use client"

import { Tooltip as TooltipPrimitive } from "radix-ui"
import * as React from "react"

import { cn } from "@/lib/utils"

function TooltipProvider({
  delayDuration = 0,
  ...props
}: React.ComponentProps<typeof TooltipPrimitive.Provider>) {
  return (
    <TooltipPrimitive.Provider
      data-slot="tooltip-provider"
      delayDuration={delayDuration}
      {...props}
    />
  )
}

function Tooltip({
  open: controlledOpen,
  defaultOpen,
  onOpenChange,
  ...props
}: React.ComponentProps<typeof TooltipPrimitive.Root>) {
  const [uncontrolledOpen, setUncontrolledOpen] = React.useState(
    defaultOpen ?? false
  )
  const open = controlledOpen ?? uncontrolledOpen
  const setOpen = React.useCallback(
    (nextOpen: boolean) => {
      if (controlledOpen === undefined) setUncontrolledOpen(nextOpen)
      onOpenChange?.(nextOpen)
    },
    [controlledOpen, onOpenChange]
  )

  // Radix handles pointer-downs in this document, but iframe events never reach
  // it. Listen inside same-origin frames while open; window blur covers the
  // first interaction with a cross-origin frame.
  React.useEffect(() => {
    if (!open) return
    const close = () => setOpen(false)
    const frameWindows: Window[] = []
    for (const frame of document.querySelectorAll("iframe")) {
      try {
        const frameWindow = frame.contentWindow
        if (!frameWindow) continue
        frameWindow.addEventListener("pointerdown", close, true)
        frameWindows.push(frameWindow)
      } catch {
        // Cross-origin frames are inaccessible; window blur is the safe fallback.
      }
    }
    window.addEventListener("blur", close)
    return () => {
      window.removeEventListener("blur", close)
      for (const frameWindow of frameWindows) {
        frameWindow.removeEventListener("pointerdown", close, true)
      }
    }
  }, [open, setOpen])

  return (
    <TooltipPrimitive.Root
      data-slot="tooltip"
      open={open}
      onOpenChange={setOpen}
      {...props}
    />
  )
}

function TooltipTrigger({
  ...props
}: React.ComponentProps<typeof TooltipPrimitive.Trigger>) {
  return <TooltipPrimitive.Trigger data-slot="tooltip-trigger" {...props} />
}

function TooltipContent({
  className,
  sideOffset = 0,
  children,
  ...props
}: React.ComponentProps<typeof TooltipPrimitive.Content>) {
  return (
    <TooltipPrimitive.Portal>
      <TooltipPrimitive.Content
        data-slot="tooltip-content"
        sideOffset={sideOffset}
        className={cn(
          "data-[side=bottom]:slide-in-from-top-2 data-[side=left]:slide-in-from-right-2 data-[side=right]:slide-in-from-left-2 data-[side=top]:slide-in-from-bottom-2 data-[state=delayed-open]:fade-in-0 data-[state=delayed-open]:zoom-in-95 data-open:fade-in-0 data-open:zoom-in-95 data-closed:fade-out-0 data-closed:zoom-out-95 z-50 inline-flex w-fit max-w-xs origin-(--radix-tooltip-content-transform-origin) items-center gap-1.5 rounded-md bg-foreground px-3 py-1.5 text-background text-xs has-data-[slot=kbd]:pr-1.5 data-[state=delayed-open]:animate-in data-closed:animate-out data-open:animate-in **:data-[slot=kbd]:relative **:data-[slot=kbd]:isolate **:data-[slot=kbd]:z-50 **:data-[slot=kbd]:rounded-sm",
          className
        )}
        {...props}
      >
        {children}
        <TooltipPrimitive.Arrow className="z-50 size-2.5 translate-y-[calc(-50%_-_2px)] rotate-45 rounded-[2px] bg-foreground fill-foreground" />
      </TooltipPrimitive.Content>
    </TooltipPrimitive.Portal>
  )
}

export { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger }
