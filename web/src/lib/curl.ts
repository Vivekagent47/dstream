import type { EventDetail } from '#/lib/api'

// POSIX single-quote escape: wrap in single quotes (the shell treats everything
// inside them literally) and close/escape/reopen for any embedded quote. Required
// because url, headers and body are attacker-controlled webhook data — JSON
// double-quoting still lets the shell expand $(...), backticks and $VAR, so a
// pasted curl would run arbitrary commands on the operator's machine.
export function shq(s: string): string {
  return `'${s.replace(/'/g, "'\\''")}'`
}

// Reproduce the delivered request as a copy-pasteable curl. Body is passed raw
// via --data; headers replayed as sent.
export function buildCurl(ev: EventDetail): string {
  const url = ev.destination.url ?? ''
  const parts = [`curl -X ${shq(ev.request.method)} ${shq(url)}`]
  for (const [k, v] of Object.entries(ev.request.headers)) {
    parts.push(`  -H ${shq(`${k}: ${Array.isArray(v) ? v.join(', ') : v}`)}`)
  }
  if (ev.request.body) parts.push(`  --data ${shq(ev.request.body)}`)
  return parts.join(' \\\n')
}
