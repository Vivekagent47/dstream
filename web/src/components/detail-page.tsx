import type { ReactNode } from 'react'
import { toast } from 'sonner'
import { Copy } from 'lucide-react'

// Shared building blocks for the source/destination detail pages: labeled
// read-only rows and copy-to-clipboard values. (Metric cards moved to
// entity-metrics.tsx once the aggregation endpoints landed.)

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
