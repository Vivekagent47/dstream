import { describe, expect, it } from 'vitest'

import { formatAgainst, quotaState } from './quota'

describe('quotaState', () => {
  it('treats 0 as unlimited, not as zero capacity', () => {
    const s = quotaState(5_000_000, 0, 0)
    expect(s.fullyUnlimited).toBe(true)
    expect(s.noCeiling).toBe(true)
    expect(s.overHard).toBe(false)
    expect(s.overSoft).toBe(false)
    // No ceiling to scale against, so a bar must not claim to be full.
    expect(s.pct).toBe(0)
  })

  // The case a previous implementation got wrong by computing overSoft behind
  // a check on `hard`: "warn me but never reject" is a real configuration,
  // and the warning has to fire in it.
  it('fires overSoft when soft is set but hard is unlimited', () => {
    const s = quotaState(9_000, 8_000, 0)
    expect(s.overSoft).toBe(true)
    expect(s.overHard).toBe(false)
    expect(s.noCeiling).toBe(true)
    expect(s.fullyUnlimited).toBe(false)
  })

  it('rejects at the hard ceiling, inclusive', () => {
    expect(quotaState(9_999, 8_000, 10_000).overHard).toBe(false)
    expect(quotaState(10_000, 8_000, 10_000).overHard).toBe(true)
    expect(quotaState(10_001, 8_000, 10_000).overHard).toBe(true)
  })

  it('warns at the soft limit, inclusive, without rejecting', () => {
    const under = quotaState(7_999, 8_000, 10_000)
    expect(under.overSoft).toBe(false)

    const at = quotaState(8_000, 8_000, 10_000)
    expect(at.overSoft).toBe(true)
    expect(at.overHard).toBe(false)
  })

  it('clamps pct so an over-limit org cannot overflow the bar', () => {
    expect(quotaState(5_000, 0, 10_000).pct).toBe(50)
    expect(quotaState(25_000, 0, 10_000).pct).toBe(100)
  })

  it('reports the free preset at rest', () => {
    const s = quotaState(0, 8_000, 10_000)
    expect(s).toEqual({
      noCeiling: false,
      fullyUnlimited: false,
      overSoft: false,
      overHard: false,
      pct: 0,
    })
  })
})

describe('formatAgainst', () => {
  it('shows ∞ rather than a misleading / 0', () => {
    expect(formatAgainst(1_234, 0)).toBe('1,234 / ∞')
  })

  it('shows the ceiling when there is one', () => {
    expect(formatAgainst(1_234, 10_000)).toBe('1,234 / 10,000')
  })
})
