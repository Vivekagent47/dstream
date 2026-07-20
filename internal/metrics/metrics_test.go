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
