package ingest

import (
	"testing"
	"time"
)

// TestSweepExpiredSources guards the source-cache memory bound: the periodic
// sweep must physically delete expired entries (the unauthenticated bad-token
// flood path only ever expired them logically) while leaving live ones intact.
func TestSweepExpiredSources(t *testing.T) {
	var h Handler
	now := time.Now()

	h.sourceCache.Store("expired-neg", sourceCacheEntry{expires: now.Add(-time.Second), notFound: true})
	h.sourceCache.Store("expired-pos", sourceCacheEntry{expires: now.Add(-time.Hour)})
	h.sourceCache.Store("live", sourceCacheEntry{expires: now.Add(time.Minute)})

	h.sweepExpiredSources()

	if _, ok := h.sourceCache.Load("expired-neg"); ok {
		t.Error("expired negative entry survived the sweep")
	}
	if _, ok := h.sourceCache.Load("expired-pos"); ok {
		t.Error("expired positive entry survived the sweep")
	}
	if _, ok := h.sourceCache.Load("live"); !ok {
		t.Error("live entry was wrongly swept")
	}
}
