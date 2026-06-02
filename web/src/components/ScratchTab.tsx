import { EditorView } from "@codemirror/view"
import CodeMirror from "@uiw/react-codemirror"
import { ArrowRight, Save, Trash2 } from "lucide-react"
import * as React from "react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { api } from "@/lib/api"
import { lsGet, lsSet, useApp } from "@/lib/app-store"
import { editorTheme } from "@/lib/codemirror"
import { typeIntoHerdr } from "@/lib/terminal"

// A persistent scratch pad in the sidebar: jot text, send it to the herdr
// terminal without submitting (so the user reviews and presses Enter), save it
// to a file, or clear it. Content lives in localStorage so it survives reloads.
// Mirrors fulcrum's scratch editor, adapted to lasso's CodeMirror + API.
const STORAGE_KEY = "lasso-scratch"

export function ScratchTab() {
  const { activeCwd } = useApp()
  const [content, setContent] = React.useState(() => lsGet(STORAGE_KEY) ?? "")
  const [showSave, setShowSave] = React.useState(false)
  const [savePath, setSavePath] = React.useState("")
  const saveInputRef = React.useRef<HTMLInputElement>(null)

  // The editor (plain text — no language) themed to the live herdr palette.
  const extensions = React.useMemo(
    () => [editorTheme, EditorView.lineWrapping],
    []
  )

  // Persist on every change. lsSet is cheap and never throws.
  const handleChange = React.useCallback((v: string) => {
    setContent(v)
    lsSet(STORAGE_KEY, v)
  }, [])

  const defaultSavePath = activeCwd ? `${activeCwd}/scratch.txt` : "scratch.txt"

  const handleSend = React.useCallback(() => {
    if (!content) return
    typeIntoHerdr(content)
  }, [content])

  const handleClear = React.useCallback(() => {
    setContent("")
    lsSet(STORAGE_KEY, "")
  }, [])

  const openSave = React.useCallback(() => {
    setSavePath((p) => p || defaultSavePath)
    setShowSave((s) => !s)
  }, [defaultSavePath])

  React.useEffect(() => {
    if (showSave) {
      saveInputRef.current?.focus()
      saveInputRef.current?.select()
    }
  }, [showSave])

  const handleSave = React.useCallback(async () => {
    const path = savePath.trim()
    if (!path) return
    try {
      await api.writeFile(path, content)
      toast.success(`Saved to ${path}`)
      setShowSave(false)
    } catch (e) {
      toast.error((e as Error).message)
    }
  }, [savePath, content])

  const onSaveKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "Enter") {
      e.preventDefault()
      void handleSave()
    } else if (e.key === "Escape") {
      e.preventDefault()
      setShowSave(false)
    }
  }

  // Cmd/Ctrl+Enter sends the buffer to the terminal, mirroring fulcrum.
  const onEditorKeyDown = (e: React.KeyboardEvent) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
      e.preventDefault()
      e.stopPropagation()
      handleSend()
    }
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="flex flex-wrap items-center gap-1 border-border border-b bg-background px-2 py-1">
        <Button
          variant="ghost"
          size="sm"
          className="h-7"
          disabled={!content}
          title="send to the herdr terminal (⌘/Ctrl+Enter)"
          onClick={handleSend}
        >
          <ArrowRight />
          Send to Terminal
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="h-7"
          disabled={!content}
          title="save the buffer to a file"
          onClick={openSave}
        >
          <Save />
          Save
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="h-7"
          disabled={!content}
          title="clear the buffer"
          onClick={handleClear}
        >
          <Trash2 />
          Clear
        </Button>
      </header>

      {showSave && (
        <div className="flex items-center gap-2 border-border border-b bg-background px-2 py-1">
          <span className="shrink-0 text-[13px] text-muted-foreground">
            Save as:
          </span>
          <Input
            ref={saveInputRef}
            value={savePath}
            onChange={(e) => setSavePath(e.target.value)}
            onKeyDown={onSaveKeyDown}
            placeholder="/path/to/file.txt"
            className="h-7 font-mono text-[13px]"
          />
          <Button
            variant="outline"
            size="sm"
            className="h-7"
            disabled={!savePath.trim()}
            onClick={() => void handleSave()}
          >
            Save
          </Button>
          <Button
            variant="ghost"
            size="sm"
            className="h-7"
            onClick={() => setShowSave(false)}
          >
            Cancel
          </Button>
        </div>
      )}

      <div className="min-h-0 flex-1" onKeyDownCapture={onEditorKeyDown}>
        <CodeMirror
          value={content}
          onChange={handleChange}
          theme="none"
          // Use the browser's native selection (styled in lib/codemirror) instead
          // of CodeMirror's drawn one — the drawn band can't recolor selected text
          // and read as nearly invisible on light themes.
          basicSetup={{ drawSelection: false }}
          extensions={extensions}
          height="100%"
          className="cm-host"
        />
      </div>
    </div>
  )
}
