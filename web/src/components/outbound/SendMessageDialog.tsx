import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { toast } from 'sonner'

import { api, qk } from '#/lib/api'
import { Button } from '#/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '#/components/ui/dialog'
import { Input } from '#/components/ui/input'
import { Label } from '#/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '#/components/ui/select'

// Shared textarea styling — there is no ui/textarea component and JSON needs a
// multi-line, monospace field. Mirrors ui/input's border/focus treatment.
const textareaClass =
  'flex min-h-[120px] w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

// Parse the payload JSON textarea. Payload is required, so blank is an error
// too; toasts and returns { ok: false } so the caller just aborts.
function parsePayload(raw: string): { ok: true; value: unknown } | { ok: false } {
  const trimmed = raw.trim()
  if (!trimmed) {
    toast.error('payload is required')
    return { ok: false }
  }
  try {
    return { ok: true, value: JSON.parse(trimmed) }
  } catch {
    toast.error('invalid payload JSON')
    return { ok: false }
  }
}

export function SendMessageDialog({
  appId,
  open,
  onOpenChange,
}: {
  appId: string
  open: boolean
  onOpenChange: (o: boolean) => void
}) {
  const qc = useQueryClient()
  const { data: eventTypes } = useQuery({
    queryKey: qk.eventTypes(),
    queryFn: () => api.listEventTypes(),
  })
  const [eventType, setEventType] = useState('')
  const [payload, setPayload] = useState('')
  const [eventId, setEventId] = useState('')

  const active = (eventTypes ?? []).filter((et) => !et.archived)

  const send = useMutation({
    mutationFn: (parsedPayload: unknown) =>
      api.sendMessage(appId, {
        event_type: eventType,
        payload: parsedPayload,
        ...(eventId.trim() ? { event_id: eventId.trim() } : {}),
      }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: qk.messages(appId) })
      toast.success(
        res.idempotent_replay
          ? 'Already sent (idempotent replay)'
          : `Message sent (${res.message_id})`,
      )
      onOpenChange(false)
      setEventType('')
      setPayload('')
      setEventId('')
    },
    onError: (e) => toast.error((e as Error).message),
  })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Send message</DialogTitle>
          <DialogDescription>
            Fan this payload out to every endpoint subscribed to the event type.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault()
            const parsed = parsePayload(payload)
            if (!parsed.ok) return
            send.mutate(parsed.value)
          }}
          className="space-y-4"
        >
          <div>
            <Label className="mb-2 block">Event type</Label>
            <Select value={eventType} onValueChange={(v) => setEventType(v ?? '')}>
              <SelectTrigger className="w-full">
                <SelectValue>{(v: string | null) => v || 'Select an event type'}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                {active.map((et) => (
                  <SelectItem key={et.id} value={et.name}>
                    {et.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div>
            <Label htmlFor="send-payload" className="mb-2 block">
              Payload JSON
            </Label>
            <textarea
              id="send-payload"
              className={textareaClass}
              value={payload}
              onChange={(e) => setPayload(e.target.value)}
              placeholder='{ "hello": "world" }'
            />
          </div>
          <div>
            <Label htmlFor="send-event-id" className="mb-2 block">
              Event ID <span className="text-muted-foreground">(optional)</span>
            </Label>
            <Input
              id="send-event-id"
              className="w-full font-mono"
              value={eventId}
              onChange={(e) => setEventId(e.target.value)}
              placeholder="evt_123 — dedupes replays"
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={send.isPending || !eventType || !payload.trim()}>
              {send.isPending ? 'Sending…' : 'Send message'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
