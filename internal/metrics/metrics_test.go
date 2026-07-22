package metrics

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestIngestRequestIncrements(t *testing.T) {
	sid := uuid.New()
	IngestRequest(sid, false)
	IngestRequest(sid, true)
	if got := testutil.ToFloat64(ingestRequests.WithLabelValues(sid.String(), "false")); got != 1 {
		t.Errorf("deduped=false: got %v want 1", got)
	}
	if got := testutil.ToFloat64(ingestRequests.WithLabelValues(sid.String(), "true")); got != 1 {
		t.Errorf("deduped=true: got %v want 1", got)
	}
}

func TestDeliveryAndDuration(t *testing.T) {
	d, c := uuid.New(), uuid.New()
	Delivery(d, c, "delivered")
	DeliveryDuration(d, 250*time.Millisecond)
	if got := testutil.ToFloat64(deliveries.WithLabelValues(d.String(), c.String(), "delivered")); got != 1 {
		t.Errorf("deliveries: got %v want 1", got)
	}
	if got := testutil.CollectAndCount(deliveryDuration); got == 0 {
		t.Errorf("delivery_duration: expected samples")
	}
}

func TestCLISessionGauge(t *testing.T) {
	start := testutil.ToFloat64(cliSessionsActive)
	CLIConnected()
	if got := testutil.ToFloat64(cliSessionsActive); got != start+1 {
		t.Errorf("after connect: got %v want %v", got, start+1)
	}
	CLIDisconnected("closed")
	if got := testutil.ToFloat64(cliSessionsActive); got != start {
		t.Errorf("after disconnect: got %v want %v", got, start)
	}
}

// TestPruneStaleSeries covers audit #18: counter series for an entity that no
// longer exists are dropped so label cardinality can't grow for the process
// lifetime — but only when that dimension's collector query succeeded.
func TestPruneStaleSeries(t *testing.T) {
	dest, conn, src := uuid.New(), uuid.New(), uuid.New()
	Delivery(dest, conn, "delivered")
	IngestRequest(src, false)

	if testutil.CollectAndCount(deliveries) == 0 {
		t.Fatal("expected a deliveries series after recording")
	}

	// A failed dest/conn query (ok=false) must NOT prune — a transient DB error
	// isn't "no live entities".
	pruneStaleSeries(true, map[string]struct{}{}, false, nil, false, nil)
	if testutil.CollectAndCount(deliveries) == 0 {
		t.Fatal("deliveries wrongly pruned when destOK/connOK were false")
	}

	// All queries succeeded and the entities are absent → their series drop.
	pruneStaleSeries(true, map[string]struct{}{}, true, map[string]struct{}{}, true, map[string]struct{}{})
	if c := testutil.CollectAndCount(deliveries); c != 0 {
		t.Fatalf("deliveries not pruned: %d series remain", c)
	}
	if c := testutil.CollectAndCount(ingestRequests); c != 0 {
		t.Fatalf("ingestRequests not pruned: %d series remain", c)
	}
}
