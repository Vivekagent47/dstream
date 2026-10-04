/**
 * Quota display rules, shared by the tenant usage page and the operator
 * console so the two can never disagree about what a number means.
 *
 * The one rule everything here turns on: **`0` means unlimited, per tier
 * independently.** It never means "no capacity". A tier left at 0 simply
 * never fires, which is how the `enterprise` plan is expressed and how an
 * operator uncaps a single org on `custom`.
 *
 * The subtle case, and the reason this is a function rather than two
 * inline ternaries: `soft > 0` with `hard === 0` is a legitimate
 * "warn me, never reject" configuration. `overSoft` must still fire in that
 * state, so it must never be computed behind a check on `hard`.
 */
export interface QuotaState {
  /** No hard ceiling, so there is nothing to scale a progress bar against. */
  noCeiling: boolean
  /** Neither tier is set — this metric is entirely unmetered for the org. */
  fullyUnlimited: boolean
  /** At or over the soft limit: accepted, but warned once per period. */
  overSoft: boolean
  /** At or over the hard ceiling: rejected with 429. */
  overHard: boolean
  /** 0-100, how full the hard ceiling is. Always 0 when there is no ceiling. */
  pct: number
}

export function quotaState(count: number, soft: number, hard: number): QuotaState {
  const noCeiling = hard === 0
  return {
    noCeiling,
    fullyUnlimited: noCeiling && soft === 0,
    // Deliberately not gated behind `hard`: see the note above.
    overSoft: soft > 0 && count >= soft,
    overHard: !noCeiling && count >= hard,
    pct: noCeiling ? 0 : Math.min(100, (count / hard) * 100),
  }
}

/**
 * "1,234 / 10,000", or "1,234 / ∞" when the tier is uncapped.
 *
 * Rendering "1,234 / 0" would read as a quota of zero, which is the opposite
 * of what 0 means.
 */
export function formatAgainst(count: number, hard: number): string {
  return hard === 0
    ? `${count.toLocaleString()} / ∞`
    : `${count.toLocaleString()} / ${hard.toLocaleString()}`
}
