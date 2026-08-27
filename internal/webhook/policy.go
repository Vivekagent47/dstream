package webhook

import "time"

// retrySchedule is the fixed Svix-style backoff: the delay before retry N.
// len == 7 → 7 retries after the first send (8 sends total) before dead-letter.
var retrySchedule = []time.Duration{
	5 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	5 * time.Hour,
	10 * time.Hour,
	10 * time.Hour,
}

// nextDelay returns the backoff before the next attempt, given how many sends
// have already been made (attemptCount, 1-based). ok=false means the retry
// budget is exhausted and the delivery must be dead-lettered.
func nextDelay(attemptCount int) (time.Duration, bool) {
	if attemptCount < 1 || attemptCount > len(retrySchedule) {
		return 0, false
	}
	return retrySchedule[attemptCount-1], true
}
