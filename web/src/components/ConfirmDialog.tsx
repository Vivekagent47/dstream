import { type ReactNode, useState } from 'react'

import { Button } from '#/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '#/components/ui/dialog'

// ConfirmDialog is a themed replacement for window.confirm — destructive
// actions get a real modal with a configurable confirm button label and
// optional explanatory body. The trigger element is passed via `children`
// and given an onClick that opens the dialog.
//
// Why not just window.confirm: native confirms are unthemed, can be
// suppressed by aggressive browser settings, and have no way to vary the
// confirm-button label (so "Delete" and "Revoke" both show "OK").
export function ConfirmDialog({
  title,
  description,
  confirmLabel = 'Confirm',
  destructive = false,
  onConfirm,
  pending = false,
  children,
}: {
  title: string
  description?: ReactNode
  confirmLabel?: string
  destructive?: boolean
  onConfirm: () => void | Promise<void>
  pending?: boolean
  children: (open: () => void) => ReactNode
}) {
  const [open, setOpen] = useState(false)
  return (
    <>
      {children(() => setOpen(true))}
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            {description && <DialogDescription>{description}</DialogDescription>}
          </DialogHeader>
          <DialogFooter>
            <Button variant="ghost" onClick={() => setOpen(false)} disabled={pending}>
              Cancel
            </Button>
            <Button
              variant={destructive ? 'destructive' : 'default'}
              onClick={async () => {
                await onConfirm()
                setOpen(false)
              }}
              disabled={pending}
            >
              {pending ? 'Working…' : confirmLabel}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
