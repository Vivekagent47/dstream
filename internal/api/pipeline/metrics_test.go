package pipeline

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

// TestClampAfterCapsBucketCount covers audit #2: a far-past `after` (the DoS
// input) must be floored so generate_series spans at most maxHistogramBuckets
// of the chosen unit, for every bucket size.
func TestClampAfterCapsBucketCount(t *testing.T) {
	for _, bucket := range []string{"minute", "hour", "day", "week"} {
		far := pgtype.Timestamptz{Time: time.Now().Add(-100 * 365 * 24 * time.Hour), Valid: true}
		got := clampAfter(bucket, far)
		buckets := time.Since(got.Time) / bucketInterval(bucket)
		// +1 tolerance for the partial trailing bucket / elapsed test time.
		if buckets > maxHistogramBuckets+1 {
			t.Fatalf("bucket=%s: window spans %d buckets, want <= %d", bucket, buckets, maxHistogramBuckets)
		}
	}
}

// TestClampAfterLeavesSmallWindowUnchanged: an in-cap window and an absent
// bound must pass through untouched (clamp is invisible to the real UI).
func TestClampAfterLeavesSmallWindowUnchanged(t *testing.T) {
	recent := pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Hour), Valid: true}
	if got := clampAfter("hour", recent); !got.Time.Equal(recent.Time) {
		t.Fatalf("in-cap after was moved: got %v want %v", got.Time, recent.Time)
	}
	if got := clampAfter("hour", pgtype.Timestamptz{}); got.Valid {
		t.Fatal("invalid after should stay invalid")
	}
}

func metricsRoutes(r chi.Router, h Handlers) {
	r.Get("/destinations/{id}/metrics", h.DestinationMetrics)
	r.Get("/sources/{id}/metrics", h.SourceMetrics)
}

func TestBucketInterval(t *testing.T) {
	for bucket, want := range map[string]time.Duration{
		"minute": time.Minute, "hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour,
		"": time.Hour, "fortnight": time.Hour,
	} {
		if got := bucketInterval(bucket); got != want {
			t.Fatalf("bucketInterval(%q) = %v, want %v", bucket, got, want)
		}
	}
}

func TestParseMetricsWindow(t *testing.T) {
	wide := time.Now().Add(-100 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	explicit := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Second)
	cases := []struct {
		name, query, bucket string
		after               func(got time.Time) bool
	}{
		{"missing params default to hour/7d", "", "hour", func(g time.Time) bool { return near(g, time.Now().Add(-7*24*time.Hour)) }},
		{"minute default window is clamped to 1500 buckets", "bucket=minute", "minute", func(g time.Time) bool { return near(g, time.Now().Add(-maxHistogramBuckets*time.Minute)) }},
		{"hour", "bucket=hour", "hour", nil},
		{"day", "bucket=day", "day", nil},
		{"week", "bucket=week", "week", nil},
		{"unknown bucket falls back to hour", "bucket=month", "hour", nil},
		{"explicit after", "bucket=hour&after=" + url.QueryEscape(explicit.Format(time.RFC3339)), "hour", func(g time.Time) bool { return g.Equal(explicit) }},
		{"malformed after falls back to 7d", "after=yesterday", "hour", func(g time.Time) bool { return near(g, time.Now().Add(-7*24*time.Hour)) }},
		{"wide window is clamped", "bucket=minute&after=" + url.QueryEscape(wide), "minute", func(g time.Time) bool {
			return near(g, time.Now().Add(-maxHistogramBuckets*time.Minute))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bucket, after := parseMetricsWindow(httptest.NewRequest(http.MethodGet, "/?"+c.query, nil))
			mustEq(t, "bucket", bucket, c.bucket)
			if !after.Valid {
				t.Fatal("after must be valid")
			}
			if c.after != nil && !c.after(after.Time) {
				t.Fatalf("after = %v", after.Time)
			}
		})
	}
}

func near(a, b time.Time) bool { return a.Sub(b).Abs() < time.Minute }

// bucketCase is one bucket size the API accepts, with the seeds anchored to
// the start of the current bucket so a boundary crossing mid-test cannot move
// a row.
type bucketCase struct {
	unit  string
	width time.Duration
}

var bucketCases = []bucketCase{
	{"minute", time.Minute}, {"hour", time.Hour}, {"day", 24 * time.Hour}, {"week", 7 * 24 * time.Hour},
}

// bucketAt is the instant 1s into the bucket k widths before base.
func bucketAt(base time.Time, w time.Duration, k int) time.Time {
	return base.Add(-time.Duration(k) * w).Add(time.Second)
}

type seriesPoint struct {
	Ts     string           `json:"ts"`
	Counts map[string]int64 `json:"counts"`
	Total  int64            `json:"total"`
	Count  int64            `json:"count"`
}

func series(t *testing.T, m map[string]any) []seriesPoint {
	t.Helper()
	raw, ok := m["series"].([]any)
	if !ok {
		t.Fatalf("series must be a list: %v", m["series"])
	}
	out := make([]seriesPoint, 0, len(raw))
	for _, e := range raw {
		em := e.(map[string]any)
		sp := seriesPoint{Ts: em["ts"].(string)}
		if c, ok := em["count"].(float64); ok {
			sp.Count = int64(c)
		}
		if c, ok := em["total"].(float64); ok {
			sp.Total = int64(c)
		}
		if cm, ok := em["counts"].(map[string]any); ok {
			sp.Counts = map[string]int64{}
			for k, v := range cm {
				sp.Counts[k] = int64(v.(float64))
			}
		}
		out = append(out, sp)
	}
	return out
}

// checkAxis asserts the series starts at want0 and every point is exactly one
// width after the previous: alignment and spacing, whatever the bucket size.
func checkAxis(t *testing.T, ps []seriesPoint, want0 time.Time, w time.Duration) {
	t.Helper()
	if len(ps) < 3 {
		t.Fatalf("series has %d points, want at least 3", len(ps))
	}
	for i, p := range ps {
		want := want0.Add(time.Duration(i) * w).UTC().Format(time.RFC3339)
		if p.Ts != want {
			t.Fatalf("point %d ts = %s, want %s (series %v)", i, p.Ts, want, ps)
		}
	}
}

// seedSettled seeds an event and makes it terminal at once, so the global
// delivery reaper (another package, in parallel) never sees it queued.
func seedSettled(t *testing.T, pool *pgxpool.Pool, q *store.Queries, orgID, reqID uuid.UUID, conn store.Connection) store.Event {
	t.Helper()
	ev := seedEvent(t, q, orgID, reqID, conn, false)
	setEventAt(t, pool, ev.ID, "failed", time.Now())
	return ev
}

func setEventAt(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, status string, ts time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE events SET status=$2, created_at=$3 WHERE id=$1`, id, status, ts); err != nil {
		t.Fatalf("place event: %v", err)
	}
}

func TestDestinationMetrics(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	r := newRouter(q, metricsRoutes)
	src := newSource(t, q, oid)
	dst := seedDest(t, q, oid, "http://example.test/hook")
	conn := seedConn(t, q, src, dst)
	did := store.GoUUID(dst.ID).String()

	// Three events inside the window: bucket -3 holds a delivered and a failed
	// one, bucket -2 is empty, bucket -1 holds one delivered.
	var evs []store.Event
	for i := 0; i < 3; i++ {
		reqID, _ := seedRequestOn(t, q, src)
		evs = append(evs, seedSettled(t, pool, q, oid, reqID, conn))
	}
	for _, d := range []int32{100, 300} {
		if _, err := q.CreateAttempt(context.Background(), store.CreateAttemptParams{
			EventID: evs[0].ID, AttemptNum: d / 100, DurationMs: &d,
		}); err != nil {
			t.Fatalf("seed attempt: %v", err)
		}
	}

	for _, bc := range bucketCases {
		t.Run(bc.unit, func(t *testing.T) {
			base := dbTrunc(t, pool, bc.unit, time.Now())
			setEventAt(t, pool, evs[0].ID, "delivered", bucketAt(base, bc.width, 3))
			setEventAt(t, pool, evs[1].ID, "failed", bucketAt(base, bc.width, 3))
			setEventAt(t, pool, evs[2].ID, "delivered", bucketAt(base, bc.width, 1))
			after := base.Add(-3 * bc.width)

			rec := do(t, r, http.MethodGet, "/api/destinations/"+did+"/metrics?bucket="+bc.unit+"&after="+url.QueryEscape(after.Format(time.RFC3339)), uid, oid, nil)
			wantStatus(t, rec, http.StatusOK)
			m := decodeMap(t, rec)
			mustEq(t, "bucket", m["bucket"], bc.unit)
			ps := series(t, m)
			checkAxis(t, ps, after, bc.width)

			if ps[0].Total != 2 || ps[0].Counts["delivered"] != 1 || ps[0].Counts["failed"] != 1 {
				t.Fatalf("bucket -3: %+v", ps[0])
			}
			if ps[1].Total != 0 || len(ps[1].Counts) != 0 {
				t.Fatalf("gap bucket must be empty, got %+v", ps[1])
			}
			if ps[2].Total != 1 || ps[2].Counts["delivered"] != 1 || len(ps[2].Counts) != 1 {
				t.Fatalf("bucket -1: %+v", ps[2])
			}
			mustEq(t, "total", m["total"], float64(3))
			mustEq(t, "delivered", m["delivered"], float64(2))
			if rate := m["delivery_rate"].(float64); math.Abs(rate-2.0/3) > 1e-9 {
				t.Fatalf("delivery_rate = %v", rate)
			}
			mustEq(t, "avg_latency_ms", m["avg_latency_ms"], float64(200))
		})
	}

	// Events older than the window are excluded from series and totals.
	t.Run("window excludes older events", func(t *testing.T) {
		base := dbTrunc(t, pool, "hour", time.Now())
		setEventAt(t, pool, evs[0].ID, "delivered", bucketAt(base, time.Hour, 3))
		setEventAt(t, pool, evs[1].ID, "failed", bucketAt(base, time.Hour, 3))
		setEventAt(t, pool, evs[2].ID, "delivered", bucketAt(base, time.Hour, 1))
		after := base.Add(-2 * time.Hour)
		rec := do(t, r, http.MethodGet, "/api/destinations/"+did+"/metrics?after="+url.QueryEscape(after.Format(time.RFC3339)), uid, oid, nil)
		wantStatus(t, rec, http.StatusOK)
		m := decodeMap(t, rec)
		mustEq(t, "total", m["total"], float64(1))
		mustEq(t, "delivered", m["delivered"], float64(1))
		// The attempts were written "now", so latency still sees both samples.
		mustEq(t, "avg_latency_ms", m["avg_latency_ms"], float64(200))
	})
}

func TestDestinationMetricsDefaultsAndEmpty(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	r := newRouter(q, metricsRoutes)
	src := newSource(t, q, oid)
	dst := seedDest(t, q, oid, "http://example.test/hook")
	did := store.GoUUID(dst.ID).String()

	// No traffic, no params: hour buckets over ~7d, every bucket empty, rate
	// and latency null (not 0).
	before := dbTrunc(t, pool, "hour", time.Now().Add(-7*24*time.Hour))
	rec := do(t, r, http.MethodGet, "/api/destinations/"+did+"/metrics", uid, oid, nil)
	wantStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	mustEq(t, "bucket", m["bucket"], "hour")
	ps := series(t, m)
	first, err := time.Parse(time.RFC3339, ps[0].Ts)
	if err != nil || first.Before(before) || first.After(before.Add(time.Hour)) {
		t.Fatalf("first bucket %s not the hour 7d ago (%v)", ps[0].Ts, before)
	}
	checkAxis(t, ps, first, time.Hour)
	if len(ps) < 168 {
		t.Fatalf("7d of hours = %d points", len(ps))
	}
	for _, p := range ps {
		if p.Total != 0 || len(p.Counts) != 0 {
			t.Fatalf("empty destination has traffic: %+v", p)
		}
	}
	mustEq(t, "total", m["total"], float64(0))
	mustEq(t, "delivered", m["delivered"], float64(0))
	mustEq(t, "delivery_rate", m["delivery_rate"], nil)
	mustEq(t, "avg_latency_ms", m["avg_latency_ms"], nil)

	// An event with no finished attempt: rate is real, latency stays null.
	conn := seedConn(t, q, src, dst)
	reqID, _ := seedRequestOn(t, q, src)
	seedSettled(t, pool, q, oid, reqID, conn)
	m = decodeMap(t, do(t, r, http.MethodGet, "/api/destinations/"+did+"/metrics", uid, oid, nil))
	mustEq(t, "total", m["total"], float64(1))
	mustEq(t, "delivery_rate", m["delivery_rate"], float64(0))
	mustEq(t, "avg_latency_ms", m["avg_latency_ms"], nil)

	// A window wider than the cap is clamped to 1500 buckets, not millions.
	rec = do(t, r, http.MethodGet, "/api/destinations/"+did+"/metrics?bucket=minute&after=2000-01-01T00:00:00Z", uid, oid, nil)
	wantStatus(t, rec, http.StatusOK)
	if n := len(series(t, decodeMap(t, rec))); n < maxHistogramBuckets || n > maxHistogramBuckets+2 {
		t.Fatalf("clamped series has %d points, want ~%d", n, maxHistogramBuckets)
	}
}

func TestDestinationMetricsRefusals(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uidA, oidA := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	dropEvents(t, pool, oidA)
	r := newRouter(q, metricsRoutes)
	src := newSource(t, q, oidA)
	dst := seedDest(t, q, oidA, "http://example.test/hook")
	conn := seedConn(t, q, src, dst)
	reqID, _ := seedRequestOn(t, q, src)
	seedSettled(t, pool, q, oidA, reqID, conn)
	did := store.GoUUID(dst.ID).String()

	wantErr(t, do(t, r, http.MethodGet, "/api/destinations/"+did+"/metrics", uidB, oidB, nil), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodGet, "/api/destinations/"+uuid.NewString()+"/metrics", uidA, oidA, nil), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodGet, "/api/destinations/nope/metrics", uidA, oidA, nil), http.StatusBadRequest, "invalid id")
	// The owner still sees its own traffic, so the refusals changed nothing.
	mustEq(t, "owner total", decodeMap(t, do(t, r, http.MethodGet, "/api/destinations/"+did+"/metrics", uidA, oidA, nil))["total"], float64(1))

	h := Handlers{Log: discardLog(), Queries: q}
	for _, p := range []*auth.Principal{nil, {UserID: uuid.New()}} {
		for _, fn := range []http.HandlerFunc{h.DestinationMetrics, h.SourceMetrics} {
			wantErr(t, direct(context.Background(), fn, http.MethodGet, p, did, ""), http.StatusUnauthorized, "active org required")
		}
	}

	p := &auth.Principal{UserID: uidA, OrgID: oidA}
	for _, marker := range []string{"name: DestinationDeliveryHistogram", "name: DestinationDeliveryStats"} {
		hf := Handlers{Log: discardLog(), Queries: failingQueries(t, marker)}
		wantErr(t, direct(context.Background(), hf.DestinationMetrics, http.MethodGet, p, did, ""), http.StatusInternalServerError, "metrics")
	}
}

func TestSourceMetrics(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	dropEvents(t, pool, oid)
	r := newRouter(q, metricsRoutes)
	src := newSource(t, q, oid)
	dst := seedDest(t, q, oid, "http://example.test/hook")
	conn := seedConn(t, q, src, dst)
	sid := store.GoUUID(src.ID).String()

	// Three requests; the first fans out to one event. A request on another
	// source must not leak in.
	var reqs []uuid.UUID
	for i := 0; i < 3; i++ {
		id, _ := seedRequestOn(t, q, src)
		reqs = append(reqs, id)
	}
	seedSettled(t, pool, q, oid, reqs[0], conn)
	seedRequestOn(t, q, newSource(t, q, oid))

	for _, bc := range bucketCases {
		t.Run(bc.unit, func(t *testing.T) {
			base := dbTrunc(t, pool, bc.unit, time.Now())
			for i, k := range []int{3, 3, 1} {
				if _, err := pool.Exec(context.Background(), `UPDATE requests SET received_at=$2 WHERE id=$1`, reqs[i], bucketAt(base, bc.width, k)); err != nil {
					t.Fatalf("place request: %v", err)
				}
			}
			after := base.Add(-3 * bc.width)
			lo := math.Max(time.Since(after).Hours()/24, 1)
			rec := do(t, r, http.MethodGet, "/api/sources/"+sid+"/metrics?bucket="+bc.unit+"&after="+url.QueryEscape(after.Format(time.RFC3339)), uid, oid, nil)
			hi := math.Max(time.Since(after).Hours()/24, 1)
			wantStatus(t, rec, http.StatusOK)
			m := decodeMap(t, rec)
			mustEq(t, "bucket", m["bucket"], bc.unit)
			ps := series(t, m)
			checkAxis(t, ps, after, bc.width)
			if ps[0].Count != 2 || ps[1].Count != 0 || ps[2].Count != 1 {
				t.Fatalf("counts per bucket: %+v", ps[:3])
			}
			mustEq(t, "requests", m["requests"], float64(3))
			mustEq(t, "events", m["events"], float64(1))
			if v := m["avg_events_per_request"].(float64); math.Abs(v-1.0/3) > 1e-9 {
				t.Fatalf("avg_events_per_request = %v", v)
			}
			// req/day = requests / max(window days, 1); the window grows while the request runs.
			if rate := m["requests_rate"].(float64); rate < 3/hi-1e-9 || rate > 3/lo+1e-9 {
				t.Fatalf("requests_rate = %v, want in [%v, %v]", rate, 3/hi, 3/lo)
			}
		})
	}
}

func TestSourceMetricsDefaultsAndEmpty(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uid, oid := seedOrg(t, q)
	r := newRouter(q, metricsRoutes)
	src := newSource(t, q, oid)
	sid := store.GoUUID(src.ID).String()

	rec := do(t, r, http.MethodGet, "/api/sources/"+sid+"/metrics", uid, oid, nil)
	wantStatus(t, rec, http.StatusOK)
	m := decodeMap(t, rec)
	mustEq(t, "bucket", m["bucket"], "hour")
	ps := series(t, m)
	first, err := time.Parse(time.RFC3339, ps[0].Ts)
	if err != nil {
		t.Fatalf("ts: %v", err)
	}
	checkAxis(t, ps, first, time.Hour)
	if len(ps) < 168 {
		t.Fatalf("7d of hours = %d points", len(ps))
	}
	for _, p := range ps {
		if p.Count != 0 {
			t.Fatalf("empty source has traffic: %+v", p)
		}
	}
	mustEq(t, "requests", m["requests"], float64(0))
	mustEq(t, "events", m["events"], float64(0))
	mustEq(t, "requests_rate", m["requests_rate"], float64(0))
	mustEq(t, "avg_events_per_request", m["avg_events_per_request"], nil)

	// A window under a day divides by one day, not by a fraction of one.
	seedRequestOn(t, q, src)
	after := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	m = decodeMap(t, do(t, r, http.MethodGet, "/api/sources/"+sid+"/metrics?bucket=minute&after="+url.QueryEscape(after), uid, oid, nil))
	mustEq(t, "requests", m["requests"], float64(1))
	mustEq(t, "requests_rate", m["requests_rate"], float64(1))
}

func TestSourceMetricsRefusals(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	uidA, oidA := seedOrg(t, q)
	uidB, oidB := seedOrg(t, q)
	r := newRouter(q, metricsRoutes)
	src := newSource(t, q, oidA)
	seedRequestOn(t, q, src)
	sid := store.GoUUID(src.ID).String()

	wantErr(t, do(t, r, http.MethodGet, "/api/sources/"+sid+"/metrics", uidB, oidB, nil), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodGet, "/api/sources/"+uuid.NewString()+"/metrics", uidA, oidA, nil), http.StatusNotFound, "not found")
	wantErr(t, do(t, r, http.MethodGet, "/api/sources/nope/metrics", uidA, oidA, nil), http.StatusBadRequest, "invalid id")
	mustEq(t, "owner requests", decodeMap(t, do(t, r, http.MethodGet, "/api/sources/"+sid+"/metrics", uidA, oidA, nil))["requests"], float64(1))

	p := &auth.Principal{UserID: uidA, OrgID: oidA}
	for _, marker := range []string{"name: SourceRequestHistogram", "name: SourceRequestStats"} {
		hf := Handlers{Log: discardLog(), Queries: failingQueries(t, marker)}
		wantErr(t, direct(context.Background(), hf.SourceMetrics, http.MethodGet, p, sid, ""), http.StatusInternalServerError, "metrics")
	}
}
