package metrics

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Vivekagent47/dstream/internal/store"
)

type fakeQ struct {
	conns uuid.UUID
	srcID uuid.UUID
	// eventsErr, when set, makes CountEventsByStatePerConnection fail so the
	// graceful-degradation path can be exercised.
	eventsErr error
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func (q fakeQ) CountEventsByStatePerConnection(context.Context) ([]store.CountEventsByStatePerConnectionRow, error) {
	if q.eventsErr != nil {
		return nil, q.eventsErr
	}
	return []store.CountEventsByStatePerConnectionRow{
		{ConnectionID: pgUUID(q.conns), Status: "queued", N: 3},
	}, nil
}
func (q fakeQ) ListSourceInfo(context.Context) ([]store.ListSourceInfoRow, error) {
	return []store.ListSourceInfoRow{{ID: pgUUID(q.srcID), Name: "stripe-prod"}}, nil
}
func (q fakeQ) ListDestinationInfo(context.Context) ([]store.ListDestinationInfoRow, error) {
	return nil, nil
}
func (q fakeQ) ListConnectionInfo(context.Context) ([]store.ListConnectionInfoRow, error) {
	return nil, nil
}

func TestCollectorEmitsGaugeAndInfo(t *testing.T) {
	connID := uuid.New()
	srcID := uuid.New()
	c := NewCollector(fakeQ{conns: connID, srcID: srcID}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	expected := `
# HELP dstream_events_in_state Events in a non-terminal state, by connection and status.
# TYPE dstream_events_in_state gauge
dstream_events_in_state{connection_id="` + connID.String() + `",status="queued"} 3
# HELP dstream_source_info Source id→name mapping (value always 1).
# TYPE dstream_source_info gauge
dstream_source_info{source_id="` + srcID.String() + `",source_name="stripe-prod"} 1
`
	// Compare both the gauge and source_info so a future label-count mismatch on
	// the _info descriptors (which would panic the whole scrape) is caught here.
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"dstream_events_in_state", "dstream_source_info"); err != nil {
		t.Errorf("collector output mismatch: %v", err)
	}
}

// A DB error on one query must not panic the scrape or drop the queries that
// still succeed — the failed series is simply absent (and counted separately in
// dstream_metrics_scrape_errors_total).
func TestCollectorDBErrorDegradesGracefully(t *testing.T) {
	c := NewCollector(
		fakeQ{conns: uuid.New(), srcID: uuid.New(), eventsErr: context.DeadlineExceeded},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	// events_in_state failed → absent; source_info still emitted.
	if n := testutil.CollectAndCount(c, "dstream_events_in_state"); n != 0 {
		t.Errorf("events_in_state should be absent on DB error, got %d samples", n)
	}
	if n := testutil.CollectAndCount(c, "dstream_source_info"); n != 1 {
		t.Errorf("source_info should still be emitted, got %d samples", n)
	}
}
