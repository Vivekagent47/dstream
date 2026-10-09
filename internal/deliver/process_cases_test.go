package deliver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/store"
)

func TestNew_Defaults(t *testing.T) {
	q, _ := inboundDB(t)
	dq, rdb := inboundQueue(t)
	h := New(inboundLog(), q, rdb, nil, dq, false)
	if h.HTTP == nil || h.HTTP.Timeout != DeliveryTimeout || h.Limiter == nil || h.Queue != dq || h.Queries != q {
		t.Fatalf("New did not wire its dependencies: %+v", h)
	}
	if h.TransformTimeout != time.Second || h.TransformMaxOutput != 5<<20 {
		t.Errorf("transform defaults = %v / %d", h.TransformTimeout, h.TransformMaxOutput)
	}
	// allowPrivate=false must yield a guarded client.
	if _, err := h.HTTP.Get("http://127.0.0.1:1/"); err == nil || !strings.Contains(err.Error(), "ssrf-guard") {
		t.Errorf("New(allowPrivate=false) client reached loopback: %v", err)
	}
}

func TestEffTransformGuards(t *testing.T) {
	for _, c := range []struct {
		in         time.Duration
		out        time.Duration
		inMax, max int
	}{
		{0, time.Second, 0, 5 << 20},
		{-3 * time.Second, time.Second, -1, 5 << 20},
		{250 * time.Millisecond, 250 * time.Millisecond, 1024, 1024},
	} {
		h := &Handler{TransformTimeout: c.in, TransformMaxOutput: c.inMax}
		if got := h.effTransformTimeout(); got != c.out {
			t.Errorf("timeout(%v) = %v, want %v", c.in, got, c.out)
		}
		if got := h.effTransformMaxOutput(); got != c.max {
			t.Errorf("maxOutput(%d) = %d, want %d", c.inMax, got, c.max)
		}
	}
}

func TestIsTerminalStatus_Filtered(t *testing.T) {
	if !isTerminalStatus("filtered") {
		t.Error("filtered is terminal (events.sql MarkEventFiltered)")
	}
}

func TestFlattenAndUnmarshalHeaders(t *testing.T) {
	got := flattenHeaders(map[string][]string{
		"A": {"1"}, "B": {"x", "y", "z"}, "Empty": {}, "Nil": nil,
	})
	if len(got) != 2 || got["A"] != "1" || got["B"] != "x,y,z" {
		t.Errorf("flattenHeaders = %v", got)
	}
	if got := flattenHeaders(nil); got == nil || len(got) != 0 {
		t.Errorf("flattenHeaders(nil) = %v, want empty non-nil map", got)
	}

	if m, err := unmarshalHeaders(nil); m != nil || err != nil {
		t.Errorf("unmarshalHeaders(nil) = %v, %v", m, err)
	}
	m, err := unmarshalHeaders([]byte(`{"A":["1","2"]}`))
	if err != nil || len(m["A"]) != 2 || m["A"][1] != "2" {
		t.Errorf("unmarshalHeaders = %v, %v", m, err)
	}
	if _, err := unmarshalHeaders([]byte(`{"A":"not-a-list"}`)); err == nil {
		t.Error("unmarshalHeaders accepted a non-list value")
	}
	if _, err := unmarshalHeaders([]byte(`{`)); err == nil {
		t.Error("unmarshalHeaders accepted truncated JSON")
	}
}

func TestConnFromRow(t *testing.T) {
	sched := []byte(`[1,2]`)
	row := store.GetEventForDeliveryRow{
		MaxRetries: 4, RetryStrategy: "custom", RetryBaseMs: 11, RetryCapMs: 22,
		RetryJitterPct: 33, CustomRetrySchedule: sched,
		Status: "queued", DestinationType: "http", // must NOT leak into the projection
	}
	c := connFromRow(row)
	if c.MaxRetries != 4 || c.RetryStrategy != "custom" || c.RetryBaseMs != 11 || c.RetryCapMs != 22 ||
		c.RetryJitterPct != 33 || string(c.CustomRetrySchedule) != "[1,2]" {
		t.Errorf("connFromRow = %+v", c)
	}
	// The projection is what RetryDelay consumes: custom schedule honoured.
	if d := RetryDelay(c, 2); d < 0 || d > 22*time.Millisecond {
		t.Errorf("RetryDelay over projected conn = %v, want <= cap 22ms", d)
	}
}

// ---- Process: happy path, headers, attempt row --------------------------------

func TestProcess_Delivers(t *testing.T) {
	d := newDest(t, "pong", 200)
	f := seed(t, seedOpt{
		url: d.url(),
		headers: `{"Content-Type":["application/json"],"X-Custom":["a","b"],"Authorization":["Bearer secret"],
			"Cookie":["s=1"],"Host":["evil"],"Dstream-Webhook-Hops":["3"],"X-Forwarded-For":["1.2.3.4"]}`,
	})
	p, raw := f.pick(0, false, 5*time.Second)
	if err := f.h.Process(context.Background(), p, raw); err != nil {
		t.Fatalf("process: %v", err)
	}

	calls := d.calls()
	if len(calls) != 1 {
		t.Fatalf("destination hit %d times, want 1", len(calls))
	}
	c := calls[0]
	if c.method != "POST" || string(c.body) != `{"amount":1}` {
		t.Errorf("request = %s %q", c.method, c.body)
	}
	if got := c.hdr.Values("X-Custom"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("X-Custom = %v, want [a b]", got)
	}
	for _, h := range []string{"Authorization", "Cookie", "X-Forwarded-For", "Traceparent", "Tracestate"} {
		if v := c.hdr.Get(h); v != "" {
			t.Errorf("%s leaked to destination: %q", h, v)
		}
	}
	if got := c.hdr.Values("Dstream-Webhook-Hops"); len(got) != 1 || got[0] != "4" {
		t.Errorf("Dstream-Webhook-Hops = %v, want exactly [4] (stored 3 + 1)", got)
	}
	if c.hdr.Get("Dstream-Event-Id") != f.ev.String() || c.hdr.Get("Dstream-Event-Attempt") != "1" {
		t.Errorf("event headers = %q / %q", c.hdr.Get("Dstream-Event-Id"), c.hdr.Get("Dstream-Event-Attempt"))
	}

	f.wantState("delivered", 1)
	if _, _, nr := f.state(); nr != nil {
		t.Errorf("next_retry_at = %v, want NULL once delivered", nr)
	}
	as := f.attempts()
	if len(as) != 1 {
		t.Fatalf("%d attempt rows, want 1", len(as))
	}
	a := as[0]
	if a.AttemptNum != 1 || a.ResponseStatus == nil || *a.ResponseStatus != 200 || a.ErrorMessage != nil ||
		string(a.ResponseBody) != "pong" {
		t.Errorf("attempt = %+v", a)
	}
	if a.QueuedInMs == nil || *a.QueuedInMs < 5000 {
		t.Errorf("queued_in_ms = %v, want >= 5000", a.QueuedInMs)
	}
	var hdr map[string][]string
	if err := json.Unmarshal(a.ResponseHeaders, &hdr); err != nil || hdr["X-Reply"][0] != "yes" {
		t.Errorf("stored response headers = %s (%v)", a.ResponseHeaders, err)
	}
	if n := len(f.lane("processing")) + len(f.lane("scheduled")) + len(f.lane("dead")); n != 0 {
		t.Errorf("queue not empty after success: %d entries", n)
	}
}

func TestProcess_ResponseBodyCappedAt1MiB(t *testing.T) {
	big := strings.Repeat("x", 2<<20)
	d := newDest(t, big, 200)
	f := seed(t, seedOpt{url: d.url()})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	if as := f.attempts(); len(as) != 1 || len(as[0].ResponseBody) != 1<<20 {
		t.Fatalf("stored response body is %d bytes, want exactly 1 MiB", len(as[0].ResponseBody))
	}
}

func TestProcess_FollowsRedirect(t *testing.T) {
	final := newDest(t, "ok", 200)
	hop := httptest.NewServer(http.RedirectHandler(final.URL+"/landed", http.StatusTemporaryRedirect))
	defer hop.Close()
	u := hop.URL
	f := seed(t, seedOpt{url: &u})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 1)
	if n := len(final.calls()); n != 1 {
		t.Fatalf("redirect target hit %d times", n)
	}
	if got := string(final.calls()[0].body); got != `{"amount":1}` {
		t.Errorf("307 must replay the body, got %q", got)
	}
}

// ---- Process: the retry ladder -------------------------------------------------

// failing status -> rescheduled at the policy delay; the attempt row, event
// row and queue entry are all asserted.
func TestProcess_FailureReschedulesWithPolicyDelay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opt      seedOpt
		attempt  int // p.Attempt going in
		wantNext int // p.Attempt coming out
		delay    time.Duration
	}{
		{"exponential first retry = base", seedOpt{strategy: "exponential", baseMs: 2000}, 0, 1, 2 * time.Second},
		{"exponential third retry = 4x", seedOpt{strategy: "exponential", baseMs: 2000}, 2, 3, 8 * time.Second},
		{"linear second retry = 2x", seedOpt{strategy: "linear", baseMs: 3000}, 1, 2, 6 * time.Second},
		{"fixed", seedOpt{strategy: "fixed", baseMs: 4000}, 2, 3, 4 * time.Second},
		{"custom", seedOpt{strategy: "custom", custom: []byte(`[1500,2500,9000]`)}, 1, 2, 2500 * time.Millisecond},
		{"clamped to cap", seedOpt{strategy: "exponential", baseMs: 2000, capMs: 5000}, 2, 3, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDest(t, "boom", 503)
			tc.opt.url = d.url()
			tc.opt.maxRetries = 5
			f := seed(t, tc.opt)
			before := time.Now()
			if err := f.process(tc.attempt); err != nil {
				t.Fatalf("process: %v", err)
			}
			after := time.Now()

			it := f.settled("scheduled")
			if it.Attempt != tc.wantNext {
				t.Errorf("rescheduled attempt = %d, want %d", it.Attempt, tc.wantNext)
			}
			within(t, "retry", it.NextRunMs, before, after, tc.delay)
			f.wantState("in_flight", 1) // not terminal; the queue owns the retry
			as := f.attempts()
			if len(as) != 1 || as[0].ResponseStatus == nil || *as[0].ResponseStatus != 503 ||
				string(as[0].ResponseBody) != "boom" || as[0].ErrorMessage != nil {
				t.Fatalf("attempt rows = %+v", as)
			}
		})
	}
}

// Full ladder: exhausts max_retries=2 -> three executions, delays base then
// 2*base, then dead (status failed) with the member on the dead list.
func TestProcess_LadderExhaustsToDead(t *testing.T) {
	d := newDest(t, "", 500)
	f := seed(t, seedOpt{url: d.url(), maxRetries: 2, baseMs: 1000})
	ctx := context.Background()
	wantDelay := []time.Duration{time.Second, 2 * time.Second}

	attempt := 0
	for i := 0; i < 3; i++ {
		before := time.Now()
		if err := f.process(attempt); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		after := time.Now()
		if i < 2 {
			it := f.settled("scheduled")
			if it.Attempt != i+1 {
				t.Fatalf("round %d: rescheduled attempt = %d", i, it.Attempt)
			}
			within(t, fmt.Sprintf("round %d", i), it.NextRunMs, before, after, wantDelay[i])
			// Drain the scheduled entry as the promoter would, ready for the next round.
			if n, err := f.dq.PromoteDue(ctx, time.Now().Add(time.Hour).UnixMilli(), 10); err != nil || n != 1 {
				t.Fatalf("promote = %d, %v", n, err)
			}
			raw, p, ok, err := f.dq.FairPick(ctx, 60000)
			if err != nil || !ok {
				t.Fatalf("repick: %v %v", ok, err)
			}
			if err := f.dq.Ack(ctx, raw); err != nil {
				t.Fatal(err)
			}
			attempt = p.Attempt
		}
	}

	dead := f.settled("dead")
	if dead.Attempt != 2 {
		t.Errorf("dead-lettered at attempt %d, want 2 (the last allowed retry)", dead.Attempt)
	}
	if n := len(f.lane("scheduled")); n != 0 {
		t.Errorf("exhausted event still scheduled: %d", n)
	}
	f.wantState("failed", 3)
	as := f.attempts()
	if len(as) != 3 {
		t.Fatalf("%d attempt rows, want 3 (1 try + 2 retries)", len(as))
	}
	for i, a := range as {
		if int(a.AttemptNum) != i+1 || a.ResponseStatus == nil || *a.ResponseStatus != 500 {
			t.Errorf("attempt %d = %+v", i, a)
		}
	}
	hdrs := []string{}
	for _, c := range d.calls() {
		hdrs = append(hdrs, c.hdr.Get("Dstream-Event-Attempt"))
	}
	if strings.Join(hdrs, ",") != "1,2,3" {
		t.Errorf("Dstream-Event-Attempt sequence = %v, want 1,2,3", hdrs)
	}
}

func TestProcess_ZeroRetriesDeadImmediately(t *testing.T) {
	d := newDest(t, "", 404)
	f := seed(t, seedOpt{url: d.url(), zeroRetries: true})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.settled("dead")
	f.wantState("failed", 1)
	if n := len(f.lane("scheduled")); n != 0 {
		t.Fatalf("scheduled = %d", n)
	}
}

// A failure followed by a success: the second execution delivers and clears
// next_retry_at; nothing is left on any lane.
func TestProcess_RecoversAfterFailure(t *testing.T) {
	d := newDest(t, "", 500, 204)
	f := seed(t, seedOpt{url: d.url()})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	it := f.settled("scheduled")
	if it.Attempt != 1 {
		t.Fatalf("attempt = %d", it.Attempt)
	}
	ctx := context.Background()
	if _, err := f.dq.PromoteDue(ctx, time.Now().Add(time.Hour).UnixMilli(), 10); err != nil {
		t.Fatal(err)
	}
	raw, p, ok, err := f.dq.FairPick(ctx, 60000)
	if err != nil || !ok || p.Attempt != 1 {
		t.Fatalf("repick ok=%v err=%v attempt=%d", ok, err, p.Attempt)
	}
	if err := f.h.Process(ctx, p, raw); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 2)
	as := f.attempts()
	if len(as) != 2 || *as[0].ResponseStatus != 500 || *as[1].ResponseStatus != 204 {
		t.Fatalf("attempts = %+v", as)
	}
	if n := len(f.lane("processing")) + len(f.lane("scheduled")) + len(f.lane("dead")); n != 0 {
		t.Errorf("leftover queue entries: %d", n)
	}
}

func TestProcess_StatusClassification(t *testing.T) {
	for _, tc := range []struct {
		code      int
		delivered bool
	}{{200, true}, {201, true}, {204, true}, {299, true}, {300, false}, {400, false}, {410, false}, {429, false}, {500, false}} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			d := newDest(t, "", tc.code)
			f := seed(t, seedOpt{url: d.url()})
			if err := f.process(0); err != nil {
				t.Fatal(err)
			}
			if tc.delivered {
				f.wantState("delivered", 1)
			} else {
				f.settled("scheduled")
				f.wantState("in_flight", 1)
			}
			if as := f.attempts(); len(as) != 1 || *as[0].ResponseStatus != int32(tc.code) {
				t.Fatalf("attempts = %+v", as)
			}
		})
	}
}

func TestProcess_DestinationTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)
	f := seed(t, seedOpt{url: &srv.URL})
	f.h.HTTP = newSafeHTTPClient(150*time.Millisecond, true)
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.settled("scheduled")
	as := f.attempts()
	if len(as) != 1 || as[0].ResponseStatus != nil || as[0].ErrorMessage == nil ||
		!(strings.Contains(*as[0].ErrorMessage, "deadline exceeded") || strings.Contains(*as[0].ErrorMessage, "Client.Timeout exceeded")) {
		t.Fatalf("attempt = %+v err=%q", as, *as[0].ErrorMessage)
	}
	if as[0].DurationMs == nil || *as[0].DurationMs < 100 {
		t.Errorf("duration_ms = %v", as[0].DurationMs)
	}
}

// ---- Process: SSRF at delivery time ---------------------------------------------

func TestProcess_SSRFGuardAtDialTime(t *testing.T) {
	d := newDest(t, "", 200)
	port := d.Listener.Addr().String()[strings.LastIndex(d.Listener.Addr().String(), ":")+1:]
	// Both destinations are structurally valid, so write-time checks pass them;
	// only the dial guard stops them. One is a hostname, to prove resolution.
	for _, u := range []string{d.URL, "http://localhost:" + port + "/hook", "http://169.254.169.254/latest/meta-data/"} {
		t.Run(u, func(t *testing.T) {
			f := seed(t, seedOpt{url: &u})
			f.h.HTTP = newSafeHTTPClient(5*time.Second, false) // production default
			if err := f.process(0); err != nil {
				t.Fatal(err)
			}
			f.settled("scheduled")
			as := f.attempts()
			if len(as) != 1 || as[0].ResponseStatus != nil || as[0].ErrorMessage == nil ||
				!strings.Contains(*as[0].ErrorMessage, "ssrf-guard: refusing to connect to non-public address") {
				t.Fatalf("attempt = %+v", as)
			}
		})
	}
	if n := len(d.calls()); n != 0 {
		t.Fatalf("destination reached %d times through the guard", n)
	}
	// And the opt-out genuinely opts out, through the same Process path.
	f := seed(t, seedOpt{url: d.url()})
	f.h.HTTP = newSafeHTTPClient(5*time.Second, true)
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 1)
	if n := len(d.calls()); n != 1 {
		t.Fatalf("opt-out: destination hit %d times, want 1", n)
	}
}

func TestProcess_BadSchemeTerminatesWithoutRetry(t *testing.T) {
	f := seed(t, seedOpt{url: strp("ftp://example.com/x")})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.settled("dead")
	f.wantState("failed", 0)
	as := f.attempts()
	if len(as) != 1 || as[0].AttemptNum != 1 || as[0].ErrorMessage == nil ||
		!strings.Contains(*as[0].ErrorMessage, `url scheme must be http or https, got "ftp"`) {
		t.Fatalf("attempts = %+v", as)
	}
}

// ValidateDestinationURL trims whitespace but the request is built from the
// untrimmed value, so a leading space passes validation and fails at build.
func TestProcess_RequestBuildFailureFailsAttempt(t *testing.T) {
	f := seed(t, seedOpt{url: strp(" http://example.com/x")})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.settled("scheduled")
	as := f.attempts()
	if len(as) != 1 || as[0].ResponseStatus != nil || as[0].ErrorMessage == nil ||
		!strings.Contains(*as[0].ErrorMessage, "first path segment in URL cannot contain colon") {
		t.Fatalf("attempts = %+v", as)
	}
}

// A missing URL can never be delivered: terminal (attempt recorded, event
// failed, member dead-lettered, nil error) so the recoverer cannot re-promote it.
func TestProcess_NoURL(t *testing.T) {
	for name, u := range map[string]*string{"nil": nil, "empty": strp("")} {
		t.Run(name, func(t *testing.T) {
			f := seed(t, seedOpt{url: u})
			if err := f.process(0); err != nil {
				t.Fatalf("err = %v, want nil (terminal)", err)
			}
			f.settled("dead")
			f.wantState("failed", 0)
			as := f.attempts()
			if len(as) != 1 || as[0].ErrorMessage == nil || *as[0].ErrorMessage != "destination has no URL" {
				t.Fatalf("attempts = %+v", as)
			}
			if n, err := f.dq.Recover(context.Background(), time.Now().Add(time.Hour).UnixMilli()); err != nil || n != 0 {
				t.Fatalf("recover = %d, %v; dead-lettered member must not be re-injected", n, err)
			}
		})
	}
}

// destinations_type_check forbids anything but http/cli, so the "not
// implemented" guard is only reachable through a shadowed table. A TEMP table
// named destinations (resolved ahead of public) inside a tx on the real
// database stands in for a future destination type; nothing is committed.
func TestProcess_UnknownDestinationType(t *testing.T) {
	d := newDest(t, "", 200)
	f := seed(t, seedOpt{url: d.url()})
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE destinations ON COMMIT DROP AS SELECT * FROM public.destinations WHERE id=$1`, store.UUID(f.dest)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE destinations SET type='sms'`); err != nil {
		t.Fatal(err)
	}
	f.h.Queries = store.New(tx)
	p, raw := f.pick(0, false, 0)
	err = f.h.Process(ctx, p, raw)
	if err != nil {
		t.Fatalf("err = %v, want nil (terminal)", err)
	}
	f.settled("dead")
	as, err := store.New(tx).ListAttemptsByEvent(ctx, store.UUID(f.ev))
	if err != nil || len(as) != 1 || as[0].ErrorMessage == nil || *as[0].ErrorMessage != `delivery type "sms" not implemented` {
		t.Fatalf("attempts = %+v err=%v", as, err)
	}
	if n, err := f.dq.Recover(ctx, time.Now().Add(time.Hour).UnixMilli()); err != nil || n != 0 {
		t.Fatalf("recover = %d, %v; dead-lettered member must not be re-injected", n, err)
	}
	if len(d.calls()) != 0 {
		t.Fatal("unknown destination type must not be delivered")
	}
}

// ---- Process: guards before the send ---------------------------------------------

func TestProcess_EventGone_AcksMember(t *testing.T) {
	f := seed(t, seedOpt{url: strp("http://x.invalid")})
	f.ev = uuid.New() // never existed
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	if n := len(f.lane("processing")) + len(f.lane("pending")); n != 0 {
		t.Fatalf("orphan member left on queue (%d)", n)
	}
}

func TestProcess_TerminalEventNotRefired(t *testing.T) {
	for _, st := range []string{"delivered", "failed", "discarded", "filtered", "dead"} {
		t.Run(st, func(t *testing.T) {
			d := newDest(t, "", 200)
			f := seed(t, seedOpt{url: d.url()})
			if _, err := f.pool.Exec(context.Background(), `UPDATE events SET status=$1 WHERE id=$2`, st, store.UUID(f.ev)); err != nil {
				t.Fatal(err)
			}
			if err := f.process(0); err != nil {
				t.Fatal(err)
			}
			if n := len(d.calls()); n != 0 {
				t.Fatalf("terminal event re-fired %d times", n)
			}
			if n := len(f.lane("processing")); n != 0 {
				t.Fatalf("member not acked (%d)", n)
			}
			f.wantState(st, 0)
			if n := len(f.attempts()); n != 0 {
				t.Fatalf("%d attempt rows written for a terminal event", n)
			}
		})
	}
}

func TestProcess_MissingBodyCountsAgainstBudget(t *testing.T) {
	for name, ref := range map[string]string{
		"unknown ref scheme": "s3:whatever",
		"missing row":        "pg:" + uuid.NewString(),
	} {
		t.Run(name, func(t *testing.T) {
			d := newDest(t, "", 200)
			f := seed(t, seedOpt{url: d.url(), bodyRef: ref})
			if err := f.process(0); err != nil {
				t.Fatal(err)
			}
			f.settled("scheduled")
			as := f.attempts()
			if len(as) != 1 || as[0].ErrorMessage == nil || as[0].ResponseStatus != nil {
				t.Fatalf("attempts = %+v", as)
			}
			if len(d.calls()) != 0 {
				t.Fatal("sent without a body")
			}
		})
	}
}

func TestProcess_BodyPoisonDeadLetters(t *testing.T) {
	f := seed(t, seedOpt{url: strp("http://x.invalid"), bodyRef: "pg:" + uuid.NewString(), zeroRetries: true})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.settled("dead")
	f.wantState("failed", 1)
}

// ---- Process: filter / transform -----------------------------------------------

func TestProcess_FilterMatchDelivers(t *testing.T) {
	d := newDest(t, "", 200)
	f := seed(t, seedOpt{url: d.url(), filter: strp("payload.amount > 0")})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 1)
	if len(d.calls()) != 1 {
		t.Fatal("matching filter must deliver")
	}
}

func TestProcess_FilterErrorFailsOpen(t *testing.T) {
	d := newDest(t, "", 200)
	f := seed(t, seedOpt{url: d.url(), filter: strp("this is ((( not an expression")})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 1)
	if len(d.calls()) != 1 {
		t.Fatal("a filter eval error must fail open and deliver")
	}
	if !strings.Contains(f.log.String(), "filter eval error; failing open") {
		t.Errorf("fail-open not logged: %s", f.log.String())
	}
}

func TestProcess_TransformErrorTerminates(t *testing.T) {
	d := newDest(t, "", 200)
	f := seed(t, seedOpt{url: d.url(), transform: strp(`function transform(){ throw new Error("boom") }`)})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.settled("dead")
	f.wantState("failed", 1)
	as := f.attempts()
	if len(as) != 1 || as[0].ErrorMessage == nil || !strings.HasPrefix(*as[0].ErrorMessage, "transform: ") ||
		!strings.Contains(*as[0].ErrorMessage, "boom") {
		t.Fatalf("attempts = %+v", as)
	}
	if len(d.calls()) != 0 {
		t.Fatal("a failed transform must not send")
	}
}

// ---- Process: gates ------------------------------------------------------------

func TestProcess_RateLimitDefers(t *testing.T) {
	for name, burst := range map[string]*int32{"burst defaults to rps": nil, "explicit burst": i32(1)} {
		t.Run(name, func(t *testing.T) {
			d := newDest(t, "", 200)
			f := seed(t, seedOpt{url: d.url(), rps: i32(1), burst: burst})
			ctx := context.Background()
			// Spend the whole burst so the next Allow is refused.
			if r, err := f.h.Limiter.Allow(ctx, "rl:dest:"+f.dest.String(), redis_rate.Limit{Rate: 1, Burst: 1, Period: time.Second}); err != nil || r.Allowed != 1 {
				t.Fatalf("priming: %+v %v", r, err)
			}
			before := time.Now()
			if err := f.process(0); err != nil {
				t.Fatal(err)
			}
			it := f.settled("scheduled")
			if it.Attempt != 0 {
				t.Errorf("a deferral must not spend retry budget; attempt = %d", it.Attempt)
			}
			if it.NextRunMs <= before.UnixMilli() || it.NextRunMs > before.Add(1500*time.Millisecond).UnixMilli() {
				t.Errorf("deferred to %d, want within ~1s of %d", it.NextRunMs, before.UnixMilli())
			}
			if it.EnqueuedAt < before.UnixMilli() {
				t.Errorf("EnqueuedAt not refreshed by defer_")
			}
			f.wantState("queued", 0)
			if len(d.calls()) != 0 || len(f.attempts()) != 0 {
				t.Fatal("a rate-limited event must not be sent or recorded")
			}
		})
	}
}

func TestProcess_RateLimitAllowsWithinBurst(t *testing.T) {
	d := newDest(t, "", 200)
	f := seed(t, seedOpt{url: d.url(), rps: i32(1), burst: i32(5)})
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 1)
}

func TestProcess_DestInflightGate(t *testing.T) {
	d := newDest(t, "", 200)
	f := seed(t, seedOpt{url: d.url(), maxInflight: i32(1)})
	ctx := context.Background()
	key := "inflight:dest:" + f.dest.String()

	// Another worker already holds the only slot.
	f.rdb.Set(ctx, key, 1, time.Minute)
	before := time.Now()
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	it := f.settled("scheduled")
	within(t, "dest inflight", it.NextRunMs, before, after, 250*time.Millisecond)
	if it.Attempt != 0 {
		t.Errorf("deferral spent retry budget: %d", it.Attempt)
	}
	if v, _ := f.rdb.Get(ctx, key).Int(); v != 1 {
		t.Errorf("our probe slot leaked: counter = %d, want 1 (the other holder)", v)
	}
	f.wantState("queued", 0)
	if len(d.calls()) != 0 {
		t.Fatal("deferred event was sent")
	}

	// Slot free: delivers and releases its own slot.
	f.rdb.Del(ctx, key)
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 1)
	if v, err := f.rdb.Get(ctx, key).Int(); err != nil || v != 0 {
		t.Errorf("slot not released: counter = %d (%v)", v, err)
	}
	if ttl := f.rdb.TTL(ctx, key).Val(); ttl <= 0 {
		t.Errorf("inflight key has no TTL (%v)", ttl)
	}
}

func TestProcess_OrgInflightGate(t *testing.T) {
	d := newDest(t, "", 200)
	f := seed(t, seedOpt{url: d.url()})
	f.h.PerOrgMaxInflight = 1
	ctx := context.Background()
	key := "inflight:org:" + f.org.String()

	f.rdb.Set(ctx, key, 1, time.Minute)
	before := time.Now()
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	it := f.settled("scheduled")
	within(t, "org inflight", it.NextRunMs, before, after, 500*time.Millisecond)
	if v, _ := f.rdb.Get(ctx, key).Int(); v != 1 {
		t.Errorf("org counter = %d, want 1", v)
	}
	if len(d.calls()) != 0 {
		t.Fatal("org-capped event was sent")
	}

	f.rdb.Del(ctx, key)
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 1)
	if v, err := f.rdb.Get(ctx, key).Int(); err != nil || v != 0 {
		t.Errorf("org slot not released: %d (%v)", v, err)
	}

	// Disabled (0): the gate never touches Redis.
	f2 := seed(t, seedOpt{url: d.url()})
	f2.h.PerOrgMaxInflight = 0
	if err := f2.process(0); err != nil {
		t.Fatal(err)
	}
	if n, _ := f2.rdb.Exists(ctx, "inflight:org:"+f2.org.String()).Result(); n != 0 {
		t.Error("PerOrgMaxInflight=0 must not use the counter")
	}
}

// ---- Process: infrastructure errors ----------------------------------------------

func TestProcess_DBErrorsSurface(t *testing.T) {
	for _, tc := range []struct {
		query string
		opt   seedOpt
		want  string
		held  bool // member stays leased
	}{
		{"GetEventForDelivery", seedOpt{}, "load event: " + errInjected.Error(), true},
		{"MarkEventInFlight", seedOpt{}, "mark in-flight: " + errInjected.Error(), true},
		{"MarkEventFiltered", seedOpt{filter: strp("payload.amount > 1000")}, "mark filtered: " + errInjected.Error(), true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			d := newDest(t, "", 200)
			tc.opt.url = d.url()
			f := seed(t, tc.opt)
			f.failQuery(tc.query)
			err := f.process(0)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if n := len(f.lane("processing")); n != 1 {
				t.Fatalf("processing = %d; member must stay leased for the recoverer", n)
			}
			if len(d.calls()) != 0 {
				t.Fatal("sent despite the DB failure")
			}
		})
	}
}

// A failing attempt-row write is logged and never blocks the delivery outcome.
func TestProcess_CreateAttemptFailureIsLoggedNotFatal(t *testing.T) {
	d := newDest(t, "", 200)
	f := seed(t, seedOpt{url: d.url()})
	f.failQuery("CreateAttempt")
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	f.wantState("delivered", 1)
	if n := len(f.attempts()); n != 0 {
		t.Fatalf("attempt rows = %d", n)
	}
	if !strings.Contains(f.log.String(), "deliver: record attempt") || !strings.Contains(f.log.String(), errInjected.Error()) {
		t.Errorf("failure not logged: %s", f.log.String())
	}
}

func TestProcess_QueueErrorsLeaveMemberLeased(t *testing.T) {
	ctx := context.Background()
	// Each case fails one queue write; the error must reach the caller.
	run := func(t *testing.T, o seedOpt, code int, wantSub string) {
		d := newDest(t, "", code)
		if o.url == nil {
			o.url = d.url()
		}
		f := seed(t, o)
		p, raw := f.pick(0, false, 0)
		f.h.Queue = deadQueue(t)
		err := f.h.Process(ctx, p, raw)
		if err == nil || !strings.Contains(err.Error(), wantSub) {
			t.Fatalf("err = %v, want it to contain %q", err, wantSub)
		}
		if n := len(f.lane("processing")); n != 1 {
			t.Fatalf("real queue processing = %d; member must remain leased", n)
		}
	}
	t.Run("ack after success", func(t *testing.T) { run(t, seedOpt{}, 200, "redis: client is closed") })
	t.Run("schedule retry", func(t *testing.T) { run(t, seedOpt{}, 500, "redis: client is closed") })
	t.Run("deadletter exhausted", func(t *testing.T) { run(t, seedOpt{zeroRetries: true}, 500, "redis: client is closed") })
	t.Run("deadletter bad scheme", func(t *testing.T) {
		run(t, seedOpt{url: strp("ftp://x/y")}, 200, "redis: client is closed")
	})
	t.Run("deadletter transform", func(t *testing.T) {
		run(t, seedOpt{transform: strp(`function transform(){ throw new Error("x") }`)}, 200, "redis: client is closed")
	})
	t.Run("ack filtered", func(t *testing.T) {
		run(t, seedOpt{filter: strp("payload.amount > 1000")}, 200, "redis: client is closed")
	})
	t.Run("ack orphan", func(t *testing.T) {
		d := newDest(t, "", 200)
		f := seed(t, seedOpt{url: d.url()})
		f.ev = uuid.New()
		p, raw := f.pick(0, false, 0)
		f.h.Queue = deadQueue(t)
		if err := f.h.Process(ctx, p, raw); err == nil || !strings.Contains(err.Error(), "redis: client is closed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("defer reschedule", func(t *testing.T) {
		d := newDest(t, "", 200)
		f := seed(t, seedOpt{url: d.url(), rps: i32(1)})
		f.h.Limiter.Allow(ctx, "rl:dest:"+f.dest.String(), redis_rate.Limit{Rate: 1, Burst: 1, Period: time.Second})
		p, raw := f.pick(0, false, 0)
		f.h.Queue = deadQueue(t)
		err := f.h.Process(ctx, p, raw)
		if err == nil || !strings.HasPrefix(err.Error(), "rate-limit reschedule: ") {
			t.Fatalf("err = %v", err)
		}
	})
}

// ---- CLI destinations ------------------------------------------------------------

func cliSeed(t *testing.T) *fx { return seed(t, seedOpt{destType: "cli"}) }

func TestCLI_LiveTunnelHandsOff(t *testing.T) {
	f := cliSeed(t)
	ctx := context.Background()
	f.rdb.Set(ctx, "cli:source:"+f.src.String(), "1", time.Minute)
	if err := f.process(0); err != nil {
		t.Fatal(err)
	}
	if n := len(f.lane("processing")); n != 0 {
		t.Fatalf("processing = %d", n)
	}
	got, err := f.rdb.LRange(ctx, "cli:dispatch:"+f.src.String(), 0, -1).Result()
	if err != nil || len(got) != 1 {
		t.Fatalf("dispatch list = %v (%v)", got, err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(got[0]), &m); err != nil || m["event_id"] != f.ev.String() {
		t.Fatalf("dispatch payload = %s", got[0])
	}
	f.wantState("queued", 0) // the CLI WS path owns the status from here
}

func TestCLI_NoTunnel(t *testing.T) {
	ctx := context.Background()
	t.Run("grace window reschedules", func(t *testing.T) {
		f := cliSeed(t)
		before := time.Now()
		if err := f.process(0); err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		it := f.settled("scheduled")
		within(t, "cli grace", it.NextRunMs, before, after, 15*time.Second)
		if it.Attempt != 0 {
			t.Errorf("attempt = %d, not a retry", it.Attempt)
		}
		s, _, nr := f.state()
		if s != "queued" || nr == nil || nr.Before(before.Add(14*time.Second)) || nr.After(after.Add(16*time.Second)) {
			t.Errorf("state = %s next_retry_at=%v", s, nr)
		}
	})
	t.Run("manual retry discards immediately", func(t *testing.T) {
		f := cliSeed(t)
		p, raw := f.pick(0, true, 0)
		if err := f.h.Process(ctx, p, raw); err != nil {
			t.Fatal(err)
		}
		f.wantState("discarded", 0)
		if n := len(f.lane("processing")) + len(f.lane("scheduled")); n != 0 {
			t.Fatalf("queue entries left: %d", n)
		}
	})
	t.Run("past the wait window discards", func(t *testing.T) {
		f := cliSeed(t)
		if _, err := f.pool.Exec(ctx, `UPDATE events SET created_at = now() - interval '3 minutes' WHERE id=$1`, store.UUID(f.ev)); err != nil {
			t.Fatal(err)
		}
		if err := f.process(0); err != nil {
			t.Fatal(err)
		}
		f.wantState("discarded", 0)
		if n := len(f.lane("processing")) + len(f.lane("scheduled")); n != 0 {
			t.Fatalf("queue entries left: %d", n)
		}
	})
	t.Run("discard write failure is logged and still acks", func(t *testing.T) {
		f := cliSeed(t)
		f.failQuery("MarkEventDiscarded")
		p, raw := f.pick(0, true, 0)
		if err := f.h.Process(ctx, p, raw); err != nil {
			t.Fatal(err)
		}
		if n := len(f.lane("processing")); n != 0 {
			t.Fatalf("processing = %d", n)
		}
		if !strings.Contains(f.log.String(), "cli discard") || !strings.Contains(f.log.String(), errInjected.Error()) {
			t.Errorf("not logged: %s", f.log.String())
		}
		f.wantState("queued", 0)
	})
}

func TestCLI_RedisErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("session check", func(t *testing.T) {
		f := cliSeed(t)
		p, raw := f.pick(0, false, 0)
		f.h.Redis = redisClosed(f.rdb.Options())
		err := f.h.Process(ctx, p, raw)
		if err == nil || !strings.HasPrefix(err.Error(), "cli session check: ") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rpush", func(t *testing.T) {
		f := cliSeed(t)
		f.rdb.Set(ctx, "cli:source:"+f.src.String(), "1", time.Minute)
		f.rdb.Set(ctx, "cli:dispatch:"+f.src.String(), "not-a-list", time.Minute) // RPUSH -> WRONGTYPE
		err := f.process(0)
		if err == nil || !strings.HasPrefix(err.Error(), "cli rpush: ") || !strings.Contains(err.Error(), "WRONGTYPE") {
			t.Fatalf("err = %v", err)
		}
		if n := len(f.lane("processing")); n != 1 {
			t.Fatalf("processing = %d; must stay leased", n)
		}
	})
}
