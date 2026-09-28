import { useMutation, useQueryClient } from "@tanstack/react-query"
import { Settings2, Trash2 } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { api, type BrowserProfileStatus } from "@/lib/api"
import { qk } from "@/lib/query"
import { cn } from "@/lib/utils"

// BrowserProfileBar is the strip along the bottom of the Agent browser: which
// profile this tab is looking at, and (on a server that has profiles) the way
// into creating, editing and deleting them. Each profile is a separate
// Chromium with its own cookies and optional proxy, so switching is a
// reconnect, not a filter over one browser's pages.

function proxyHost(proxy: string): string {
  try {
    return new URL(proxy).host || proxy
  } catch {
    return proxy
  }
}

export function BrowserProfileBar({
  profiles,
  current,
  onPick,
  manageable,
}: {
  profiles: BrowserProfileStatus[]
  current: string
  onPick: (id: string) => void
  // False on an older server with no profile API: the selector shows just
  // the one default profile and there is nothing to manage.
  manageable: boolean
}) {
  const [open, setOpen] = React.useState(false)
  const cur = profiles.find((p) => p.id === current)
  return (
    <div className="flex flex-shrink-0 items-center gap-1.5 border-border border-t bg-background px-2 py-1 text-[12px]">
      <span className="text-muted-foreground">Profile</span>
      <select
        aria-label="Browser profile"
        className="h-6 min-w-0 max-w-48 flex-shrink rounded-md border border-border bg-background px-1.5 text-[12px] text-foreground"
        value={current}
        disabled={profiles.length < 2}
        onChange={(e) => onPick(e.target.value)}
      >
        {profiles.map((p) => (
          <option key={p.id} value={p.id}>
            {p.running ? "● " : ""}
            {p.name}
          </option>
        ))}
      </select>
      {cur?.proxy && (
        <span
          className="min-w-0 truncate rounded-full border border-border px-1.5 font-mono text-[11px] text-muted-foreground"
          title={`Traffic goes through ${cur.proxy}`}
        >
          via {proxyHost(cur.proxy)}
        </span>
      )}
      {manageable && (
        <Button
          variant="ghost"
          size="icon"
          className="ml-auto size-6"
          title="Manage profiles"
          aria-label="Manage profiles"
          onClick={() => setOpen(true)}
        >
          <Settings2 className="size-3.5" />
        </Button>
      )}
      {manageable && (
        <ProfilesDialog
          open={open}
          onOpenChange={setOpen}
          current={cur}
          onPick={onPick}
        />
      )}
    </div>
  )
}

const fieldLabel = "text-[12px] font-medium text-muted-foreground"

function ProfilesDialog({
  open,
  onOpenChange,
  current,
  onPick,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  current: BrowserProfileStatus | undefined
  onPick: (id: string) => void
}) {
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: qk.browser })

  // The edit form is a draft of the selected profile, reset whenever the
  // dialog opens or the selection changes under it.
  const [name, setName] = React.useState("")
  const [proxy, setProxy] = React.useState("")
  const [editErr, setEditErr] = React.useState("")
  const curID = current?.id
  // biome-ignore lint/correctness/useExhaustiveDependencies: reset on open and on a different profile only, not on every status poll
  React.useEffect(() => {
    if (!open) return
    setName(current?.name ?? "")
    setProxy(current?.proxy ?? "")
    setEditErr("")
  }, [open, curID])

  const [newName, setNewName] = React.useState("")
  const [newProxy, setNewProxy] = React.useState("")
  const [createErr, setCreateErr] = React.useState("")
  const [confirmDelete, setConfirmDelete] = React.useState(false)

  const save = useMutation({
    mutationFn: () => {
      if (!current) throw new Error("no profile selected")
      const patch: { name?: string; proxy?: string } = {}
      if (name.trim() !== current.name) patch.name = name.trim()
      if (proxy.trim() !== current.proxy) patch.proxy = proxy.trim()
      return api.updateBrowserProfile(current.id, patch)
    },
    onSuccess: (p) => {
      setEditErr("")
      toast.success(`Saved ${p.name}`, { description: p.note })
    },
    // A 400 is the validation sentence; a 502 means it was saved but the
    // relaunch under the new proxy failed. Either belongs under the fields.
    onError: (e: Error) => setEditErr(e.message),
    onSettled: refresh,
  })

  const create = useMutation({
    mutationFn: () =>
      api.createBrowserProfile({
        name: newName.trim(),
        ...(newProxy.trim() ? { proxy: newProxy.trim() } : {}),
      }),
    onSuccess: (p) => {
      setCreateErr("")
      setNewName("")
      setNewProxy("")
      toast.success(`Created profile ${p.name}`)
      onPick(p.id)
    },
    onError: (e: Error) => setCreateErr(e.message),
    onSettled: refresh,
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.deleteBrowserProfile(id),
    onSuccess: () => {
      toast.success(`Deleted profile ${current?.name ?? ""}`)
      onPick("default")
    },
    onError: (e: Error) => setEditErr(e.message),
    onSettled: refresh,
  })

  const dirty =
    !!current &&
    (name.trim() !== current.name || proxy.trim() !== current.proxy)

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="max-h-[85dvh] overflow-y-auto sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Browser profiles</DialogTitle>
            <DialogDescription>
              Each profile is its own browser with its own cookies and logins,
              kept between restarts, and can send its traffic through a proxy.
            </DialogDescription>
          </DialogHeader>

          {current && (
            <form
              className="flex flex-col gap-1.5"
              onSubmit={(e) => {
                e.preventDefault()
                if (dirty) save.mutate()
              }}
            >
              <h3 className="font-medium text-sm">
                {current.name}
                {current.default && (
                  <span className="ml-1.5 font-normal text-muted-foreground text-xs">
                    (default)
                  </span>
                )}
              </h3>
              <label className={fieldLabel} htmlFor="bp-name">
                Name
              </label>
              <Input
                id="bp-name"
                value={name}
                className="h-8 text-[13px]"
                onChange={(e) => setName(e.target.value)}
              />
              <label className={fieldLabel} htmlFor="bp-proxy">
                Proxy
              </label>
              <Input
                id="bp-proxy"
                value={proxy}
                placeholder="socks5://host:1080 (empty = direct)"
                className="h-8 font-mono text-[13px]"
                onChange={(e) => setProxy(e.target.value)}
              />
              <p className="text-[11px] text-muted-foreground">
                Changing the proxy restarts this profile's browser; its open
                pages are reopened.
              </p>
              {editErr && (
                <p className="text-[12px] text-destructive [overflow-wrap:anywhere]">
                  {editErr}
                </p>
              )}
              <div className="flex items-center gap-1.5">
                <Button
                  type="submit"
                  size="sm"
                  disabled={!dirty || save.isPending}
                >
                  {save.isPending ? "Saving…" : "Save"}
                </Button>
                {!current.default && (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="ml-auto gap-1 text-destructive"
                    disabled={remove.isPending}
                    onClick={() => setConfirmDelete(true)}
                  >
                    <Trash2 className="size-3.5" />
                    Delete
                  </Button>
                )}
              </div>
            </form>
          )}

          <form
            className={cn(
              "flex flex-col gap-1.5",
              current && "border-border border-t pt-3"
            )}
            onSubmit={(e) => {
              e.preventDefault()
              if (newName.trim()) create.mutate()
            }}
          >
            <h3 className="font-medium text-sm">New profile</h3>
            <label className={fieldLabel} htmlFor="bp-new-name">
              Name
            </label>
            <Input
              id="bp-new-name"
              value={newName}
              placeholder="e.g. Work"
              className="h-8 text-[13px]"
              onChange={(e) => setNewName(e.target.value)}
            />
            <label className={fieldLabel} htmlFor="bp-new-proxy">
              Proxy (optional)
            </label>
            <Input
              id="bp-new-proxy"
              value={newProxy}
              placeholder="socks5://host:1080"
              className="h-8 font-mono text-[13px]"
              onChange={(e) => setNewProxy(e.target.value)}
            />
            {createErr && (
              <p className="text-[12px] text-destructive [overflow-wrap:anywhere]">
                {createErr}
              </p>
            )}
            <div>
              <Button
                type="submit"
                size="sm"
                disabled={!newName.trim() || create.isPending}
              >
                {create.isPending ? "Creating…" : "Create"}
              </Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {current?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              Its browser is stopped and its pages close. Its cookies, logins
              and everything else saved in it are deleted for good.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              onClick={() => {
                if (current) remove.mutate(current.id)
              }}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
