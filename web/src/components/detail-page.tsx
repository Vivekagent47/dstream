import type { ReactNode } from 'react'
import { toast } from 'sonner'
import { Copy } from 'lucide-react'

import { Card, CardContent, CardHeader, CardTitle } from '#/components/ui/card'

// Shared building blocks for the source/destination detail pages: labeled
// read-only rows, copy-to-clipboard values, and "no data" metric placeholders.

export function copyText(text: string, what: string) {
  navigator.clipboard
    .writeText(text)
    .then(() => toast.success(`${what} copied`))
    .catch(() => toast.error(`Couldn’t copy — select the ${what.toLowerCase()} manually`))
}

export function DetailRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="space-y-0.5">
      <div className="text-sm text-muted-foreground">{label}</div>
      <div className="text-sm">{children}</div>
    </div>
  )
}

export function CopyValue({ value, what, mono }: { value: string; what: string; mono?: boolean }) {
  return (
    <button
      type="button"
      onClick={() => copyText(value, what)}
      className={
        'group inline-flex max-w-full items-center gap-1.5 text-left hover:text-foreground ' +
        (mono ? 'font-mono text-xs' : '')
      }
      title={`Copy ${what.toLowerCase()}`}
    >
      <span className="truncate">{value}</span>
      <Copy className="h-3.5 w-3.5 shrink-0 opacity-40 group-hover:opacity-100" />
    </button>
  )
}

// ponytail: placeholder cards only — no metrics backend yet. Replace body
// with real charts when aggregation endpoints land.
export function MetricCard({ title, className }: { title: string; className?: string }) {
  return (
    <Card className={'flex flex-col ' + (className ?? '')}>
      <CardHeader className="px-5 py-3.5">
        <CardTitle className="text-sm font-medium">{title}</CardTitle>
      </CardHeader>
      <CardContent className="flex flex-1 items-center justify-center">
        <span className="text-sm text-muted-foreground">No data to display.</span>
      </CardContent>
    </Card>
  )
}
