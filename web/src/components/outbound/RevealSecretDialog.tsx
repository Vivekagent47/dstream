import { useState } from 'react'
import { toast } from 'sonner'

import { Button } from '#/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '#/components/ui/dialog'
import { Label } from '#/components/ui/label'

// One-time reveal of an endpoint signing secret. Mirrors the API-key reveal
// dialog: the plaintext is shown exactly once, so the dialog must NOT close on
// Esc or outside-click — a stray Esc would lose it forever. Only the
// "I've saved it" button closes it (via onClose).
export function RevealSecretDialog({
  secret,
  onClose,
}: {
  secret: string | null
  onClose: () => void
}) {
  const [copied, setCopied] = useState(false)

  function copy() {
    if (!secret) return
    navigator.clipboard
      .writeText(secret)
      .then(() => {
        setCopied(true)
        window.setTimeout(() => setCopied(false), 2000)
      })
      .catch(() =>
        toast.error('Couldn’t access the clipboard. Select the secret and copy it manually.'),
      )
  }

  return (
    <Dialog open={!!secret}>
      <DialogContent
        onKeyDown={(e) => {
          if (e.key === 'Escape') e.preventDefault()
        }}
      >
        <DialogHeader>
          <DialogTitle>Endpoint secret</DialogTitle>
          <DialogDescription>
            Save this signing secret now — it won&rsquo;t be shown again. Use it to verify the
            signature on webhooks delivered to this endpoint.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-2">
          <Label className="text-xs tracking-wide text-muted-foreground uppercase">
            Signing secret
          </Label>
          <div className="flex items-center gap-2">
            <code className="flex-1 overflow-x-auto rounded border bg-muted px-3 py-2 text-xs">
              {secret}
            </code>
            <Button size="sm" variant="outline" onClick={copy}>
              {copied ? 'Copied' : 'Copy'}
            </Button>
          </div>
        </div>
        <DialogFooter>
          <Button onClick={onClose}>I&rsquo;ve saved it</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
