package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestParseFlags(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		var buf bytes.Buffer
		c, err := parseFlags("prog", []string{"-url", "http://x/e/t"}, &buf)
		if err != nil {
			t.Fatal(err)
		}
		want := config{url: "http://x/e/t", rate: 100, dur: 60 * time.Second, conc: 50, sinkAddr: ":9099"}
		if c != want {
			t.Fatalf("cfg = %+v, want %+v", c, want)
		}
		if buf.Len() != 0 {
			t.Fatalf("unexpected output %q", buf.String())
		}
	})
	t.Run("all flags", func(t *testing.T) {
		c, err := parseFlags("prog", []string{"-url", "u", "-rate", "7", "-dur", "3s", "-conc", "4", "-db", "pg", "-sink", "-sink-addr", ":0", "-body", "b.json"}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		want := config{url: "u", rate: 7, dur: 3 * time.Second, conc: 4, db: "pg", sink: true, sinkAddr: ":0", bodyPath: "b.json"}
		if c != want {
			t.Fatalf("cfg = %+v, want %+v", c, want)
		}
	})
	t.Run("missing url prints error and usage", func(t *testing.T) {
		var buf bytes.Buffer
		_, err := parseFlags("prog", nil, &buf)
		if err == nil || err.Error() != "-url is required" {
			t.Fatalf("err = %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "error: -url is required") || !strings.Contains(out, "Usage of prog:") || !strings.Contains(out, "-sink-addr") {
			t.Fatalf("output lacks error+usage: %q", out)
		}
	})
	for _, tc := range []struct{ name, flag, val, msg string }{
		{"rate zero", "-rate", "0", "-rate must be >= 1"},
		{"rate negative", "-rate", "-5", "-rate must be >= 1"},
		{"conc zero", "-conc", "0", "-conc must be >= 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			_, err := parseFlags("prog", []string{"-url", "u", tc.flag, tc.val}, &buf)
			if err == nil || err.Error() != tc.msg {
				t.Fatalf("err = %v, want %q", err, tc.msg)
			}
			if !strings.Contains(buf.String(), "error: "+tc.msg) {
				t.Fatalf("output = %q", buf.String())
			}
		})
	}
	t.Run("unknown flag", func(t *testing.T) {
		var buf bytes.Buffer
		_, err := parseFlags("prog", []string{"-nope"}, &buf)
		if err == nil || !strings.Contains(buf.String(), "flag provided but not defined: -nope") {
			t.Fatalf("err=%v out=%q", err, buf.String())
		}
	})
	t.Run("help", func(t *testing.T) {
		_, err := parseFlags("prog", []string{"-h"}, io.Discard)
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("err = %v, want ErrHelp", err)
		}
	})
}

func TestDoSend(t *testing.T) {
	t.Run("200 reads body and sends json", func(t *testing.T) {
		var gotBody, gotCT, gotMethod string
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody, gotCT, gotMethod = string(b), r.Header.Get("Content-Type"), r.Method
			w.Write([]byte(strings.Repeat("x", 4096))) // response body must be drained
		}))
		var conns atomic.Int64
		srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				conns.Add(1)
			}
		}
		srv.Start()
		defer srv.Close()
		// Two sequential sends on one client reuse a connection only if the
		// first response body was drained and closed.
		client := &http.Client{Transport: &http.Transport{}}
		defer client.CloseIdleConnections()
		r := doSend(client, srv.URL, []byte(`{"a":1}`))
		if r.err != nil || r.status != 200 || r.latencyMs <= 0 {
			t.Fatalf("result = %+v", r)
		}
		if r2 := doSend(client, srv.URL, []byte(`{"a":1}`)); r2.err != nil || r2.status != 200 {
			t.Fatalf("second result = %+v", r2)
		}
		if n := conns.Load(); n != 1 {
			t.Fatalf("%d connections for 2 sequential sends, want 1 (body not drained)", n)
		}
		if gotBody != `{"a":1}` || gotCT != "application/json" || gotMethod != http.MethodPost {
			t.Fatalf("server saw %q %q %q", gotMethod, gotCT, gotBody)
		}
	})
	t.Run("non-2xx is a status, not an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		r := doSend(srv.Client(), srv.URL, nil)
		if r.err != nil || r.status != 500 || r.latencyMs <= 0 {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close() // nothing listens here any more
		r := doSend(http.DefaultClient, "http://"+addr, nil)
		if r.err == nil || r.status != 0 || r.latencyMs <= 0 {
			t.Fatalf("result = %+v", r)
		}
		if !strings.Contains(r.err.Error(), "refused") {
			t.Fatalf("err = %v, want connection refused", r.err)
		}
	})
	t.Run("bad url fails building the request", func(t *testing.T) {
		r := doSend(http.DefaultClient, "http://bad\x7furl", nil)
		if r.err == nil || r.status != 0 || r.latencyMs < 0 {
			t.Fatalf("result = %+v", r)
		}
		if !strings.Contains(r.err.Error(), "invalid control character in URL") {
			t.Fatalf("err = %v", r.err)
		}
	})
}

func TestMsSince(t *testing.T) {
	got := msSince(time.Now().Add(-1500 * time.Millisecond))
	if got < 1500 || got > 5000 {
		t.Fatalf("msSince(-1.5s) = %v, want in [1500,5000]", got)
	}
}

func TestReport(t *testing.T) {
	results := []result{
		{latencyMs: 10, status: 200},
		{latencyMs: 20, status: 201},
		{latencyMs: 30, status: 204},
		{latencyMs: 40, status: 299},
		{latencyMs: 999, status: 500}, // excluded from latency percentiles
		{latencyMs: 5, status: 404},
		{latencyMs: 6, status: 300}, // first non-2xx above the range
		{latencyMs: 4, status: 199}, // last non-2xx below the range
		{latencyMs: 7, err: errors.New("boom")},
	}
	var buf bytes.Buffer
	report(&buf, results, 2*time.Second)
	want := "--- ingest ---\n" +
		"sent          9\n" +
		"2xx           4\n" +
		"non-2xx       4\n" +
		"errors        1\n" +
		"elapsed       2.0s\n" +
		"throughput    2.0 req/s (2xx/elapsed)\n" +
		"latency ms    p50=20.0 p95=40.0 p99=40.0 max=40.0\n"
	if buf.String() != want {
		t.Fatalf("report =\n%s\nwant\n%s", buf.String(), want)
	}

	buf.Reset()
	report(&buf, nil, time.Second)
	if !strings.Contains(buf.String(), "sent          0\n") || !strings.Contains(buf.String(), "p50=0.0 p95=0.0 p99=0.0 max=0.0") {
		t.Fatalf("empty report = %s", buf.String())
	}
}

// runB runs run in a goroutine and fails (instead of hanging to the package
// timeout) if it does not return within 10s.
func runB(t *testing.T, cfg config, out, errOut io.Writer) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run(cfg, out, errOut) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s of its deadline")
		return nil
	}
}

func TestStartSink(t *testing.T) {
	var out, errBuf bytes.Buffer
	stop, bound := startSink("127.0.0.1:0", &out, &errBuf)
	if bound == "" || strings.HasSuffix(bound, ":0") {
		t.Fatalf("bound = %q", bound)
	}
	if !strings.Contains(out.String(), "sink: listening on 127.0.0.1:0 (200 OK for any request)") {
		t.Fatalf("out = %q", out.String())
	}
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(m, "http://"+bound+"/any/path?q=1", strings.NewReader("payload"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", m, resp.StatusCode)
		}
	}
	stop()
	if _, err := http.Get("http://" + bound + "/"); err == nil {
		t.Fatal("sink still serving after stop")
	}
	if errBuf.Len() != 0 {
		t.Fatalf("stderr = %q, want empty (ErrServerClosed is not an error)", errBuf.String())
	}
}

func TestServeSinkReportsServeError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve's Accept fails with a non-ErrServerClosed error
	var errBuf bytes.Buffer
	serveSink(&http.Server{}, ln, &errBuf)
	if !strings.HasPrefix(errBuf.String(), "sink: ") || !strings.Contains(errBuf.String(), "use of closed network connection") {
		t.Fatalf("stderr = %q", errBuf.String())
	}
}

func TestStartSinkEmptyAddrIsHTTPPort(t *testing.T) {
	// "" used to mean :80 via ListenAndServe; it must not become a random port.
	var out, errBuf bytes.Buffer
	stop, bound := startSink("", &out, &errBuf)
	stop()
	if bound == "" {
		if !strings.Contains(errBuf.String(), ":80:") {
			t.Fatalf("stderr = %q, want a bind error naming :80", errBuf.String())
		}
	} else if !strings.HasSuffix(bound, ":80") {
		t.Fatalf("bound = %q, want port 80", bound)
	}
}

func TestStartSinkBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var out, errBuf bytes.Buffer
	stop, bound := startSink(ln.Addr().String(), &out, &errBuf)
	stop() // must be a safe no-op
	if bound != "" || out.Len() != 0 || !strings.HasPrefix(errBuf.String(), "sink: listen tcp") || !strings.Contains(errBuf.String(), "address already in use") {
		t.Fatalf("bound=%q out=%q err=%q", bound, out.String(), errBuf.String())
	}
}

// reportInt reads the integer on a "name   N" report line.
func reportInt(t *testing.T, report, name string) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s+(\d+)$`).FindStringSubmatch(report)
	if m == nil {
		t.Fatalf("no %q line in %s", name, report)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// snapshot returns a copy of the bodies seen so far.
func (cs *countingServer) snapshot() []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]string(nil), cs.bodies...)
}

// countingServer records request count, bodies and peak in-flight requests.
type countingServer struct {
	*httptest.Server
	n, inflight, peak atomic.Int64
	mu                sync.Mutex
	bodies            []string
}

func newCountingServer(hold time.Duration, status int) *countingServer {
	cs := &countingServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := cs.inflight.Add(1)
		for {
			p := cs.peak.Load()
			if cur <= p || cs.peak.CompareAndSwap(p, cur) {
				break
			}
		}
		b, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.bodies = append(cs.bodies, string(b))
		cs.mu.Unlock()
		time.Sleep(hold) // simulated server work
		cs.n.Add(1)
		cs.inflight.Add(-1)
		w.WriteHeader(status)
	}))
	return cs
}

func TestRunSendLoop(t *testing.T) {
	cs := newCountingServer(0, 200)
	defer cs.Close()
	var out bytes.Buffer
	start := time.Now()
	err := runB(t, config{url: cs.URL, rate: 50, dur: 400 * time.Millisecond, conc: 5}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("run took %v, did not stop at the duration", el)
	}
	n := int(cs.n.Load())
	// 50/s * 0.4s = 20 ticks; allow scheduler slack below, one tick above.
	if n < 8 || n > 21 {
		t.Fatalf("server saw %d requests, want within [8,21]", n)
	}
	s := out.String()
	if !strings.Contains(s, "loadtest: "+cs.URL+" @ 50 req/s for 400ms, conc=5\n") {
		t.Fatalf("missing header: %s", s)
	}
	if !strings.Contains(s, "sent          "+strconv.Itoa(n)+"\n") || !strings.Contains(s, "2xx           "+strconv.Itoa(n)+"\n") ||
		!strings.Contains(s, "non-2xx       0\n") || !strings.Contains(s, "errors        0\n") {
		t.Fatalf("report disagrees with server count %d: %s", n, s)
	}
	// Unique bodies: no two requests identical (avoids ingest dedup).
	seen := map[string]bool{}
	for _, b := range cs.snapshot() {
		if !strings.HasPrefix(b, `{"loadtest":true,"run":`) || seen[b] {
			t.Fatalf("body %q not unique/well-formed", b)
		}
		seen[b] = true
	}
}

func TestRunConcurrencyBounded(t *testing.T) {
	cs := newCountingServer(120*time.Millisecond, 200)
	defer cs.Close()
	err := runB(t, config{url: cs.URL, rate: 200, dur: 350 * time.Millisecond, conc: 3}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if p := cs.peak.Load(); p != 3 {
		t.Fatalf("peak in-flight = %d, want exactly conc=3 (saturated, never more)", p)
	}
	// 3 slots, 120ms each, 350ms window: well under the 100 ticks offered.
	if n := cs.n.Load(); n < 3 || n > 20 {
		t.Fatalf("completed %d, want within [3,20] (throttled by conc)", n)
	}
}

func TestRunStopsAtDeadlineWhenSaturated(t *testing.T) {
	cs := newCountingServer(300*time.Millisecond, 200)
	defer cs.Close()
	start := time.Now()
	err := runB(t, config{url: cs.URL, rate: 100, dur: 100 * time.Millisecond, conc: 1}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if n := cs.n.Load(); n != 1 {
		t.Fatalf("completed %d requests, want exactly 1 (conc=1 held past the deadline)", n)
	}
	if el := time.Since(start); el < 250*time.Millisecond || el > 3*time.Second {
		t.Fatalf("run took %v: should wait for the in-flight request only", el)
	}
}

func TestRunNon2xxAndErrors(t *testing.T) {
	cs := newCountingServer(0, 503)
	defer cs.Close()
	var out bytes.Buffer
	if err := runB(t, config{url: cs.URL, rate: 50, dur: 200 * time.Millisecond, conc: 4}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	n := int(cs.n.Load())
	if n < 3 || !strings.Contains(s, "2xx           0\n") || !strings.Contains(s, "non-2xx       "+strconv.Itoa(n)+"\n") {
		t.Fatalf("n=%d report: %s", n, s)
	}

	// Unreachable target: every send is an error.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	out.Reset()
	if err := runB(t, config{url: "http://" + addr, rate: 50, dur: 200 * time.Millisecond, conc: 4}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	s = out.String()
	sent, errs := reportInt(t, s, "sent"), reportInt(t, s, "errors")
	if sent < 3 || errs != sent || !strings.Contains(s, "2xx           0\n") || !strings.Contains(s, "non-2xx       0\n") {
		t.Fatalf("unreachable report (want errors == sent >= 3): %s", s)
	}
}

func TestRunBodyFile(t *testing.T) {
	cs := newCountingServer(0, 200)
	defer cs.Close()
	p := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(p, []byte(`{"fixed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runB(t, config{url: cs.URL, rate: 50, dur: 200 * time.Millisecond, conc: 2, bodyPath: p}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	bodies := cs.snapshot()
	if len(bodies) < 3 {
		t.Fatalf("only %d requests", len(bodies))
	}
	for _, b := range bodies {
		if b != `{"fixed":true}` {
			t.Fatalf("body = %q, want the file contents", b)
		}
	}

	err := runB(t, config{url: cs.URL, rate: 1, dur: time.Second, conc: 1, bodyPath: filepath.Join(t.TempDir(), "missing.json")}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "read -body ") || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunWithSink(t *testing.T) {
	var out bytes.Buffer
	// :0 bind; run sends to the sink-less target below, only the banner matters here.
	cs := newCountingServer(0, 200)
	defer cs.Close()
	if err := runB(t, config{url: cs.URL, rate: 20, dur: 100 * time.Millisecond, conc: 2, sink: true, sinkAddr: "127.0.0.1:0"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sink: listening on 127.0.0.1:0 (200 OK for any request)\n") {
		t.Fatalf("out = %s", out.String())
	}
}

func TestRunWithSinkAsTarget(t *testing.T) {
	// Point the loader at its own sink: pick a free port, hand it to both.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	var out bytes.Buffer
	if err := runB(t, config{url: "http://" + addr + "/x", rate: 20, dur: 200 * time.Millisecond, conc: 2, sink: true, sinkAddr: addr}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "errors        0\n") || strings.Contains(s, "2xx           0\n") {
		t.Fatalf("sink did not answer the loader: %s", s)
	}
	if _, err := http.Get("http://" + addr + "/"); err == nil {
		t.Fatal("sink still serving after run returned (defer stop() missing)")
	}
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DSTREAM_TEST_DB_URL")
	if dsn == "" {
		t.Skip("DSTREAM_TEST_DB_URL not set")
	}
	return dsn
}

func TestReportDelivery(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// If seeding never gets as far as registering the org cleanup, still close.
	seeded := false
	t.Cleanup(func() {
		if !seeded {
			conn.Close(ctx)
		}
	})

	// A window in the far future so concurrent suites' attempts never match.
	since := time.Now().UTC().Add(24 * 365 * 20 * time.Hour)

	t.Run("no attempts in window", func(t *testing.T) {
		var out bytes.Buffer
		reportDelivery(&out, dsn, since)
		want := "--- delivery (attempts) ---\nno attempts recorded in window (worker not running, or nothing delivered yet)\n"
		if out.String() != want {
			t.Fatalf("out = %q", out.String())
		}
	})

	// Seed org -> source -> destination -> connection -> request -> event.
	var orgID, eventID string
	if err := conn.QueryRow(ctx, `INSERT INTO organizations (name, slug) VALUES ('loadtest', 'loadtest-'||uuidv7()::text) RETURNING id`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	// Delete before closing: cleanups run after the test body's defers, so the
	// connection is closed here, last, and the delete error is asserted.
	seeded = true
	t.Cleanup(func() {
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, orgID); err != nil {
			t.Errorf("cleanup: delete seeded org: %v", err)
		}
	})
	var srcID, dstID, connID, reqID string
	for _, s := range []struct {
		dst *string
		sql string
		arg []any
	}{
		{&srcID, `INSERT INTO sources (org_id, name, type, ingest_token) VALUES ($1,'s','generic','tok-'||uuidv7()::text) RETURNING id`, []any{orgID}},
		{&dstID, `INSERT INTO destinations (org_id, name, type) VALUES ($1,'d','cli') RETURNING id`, []any{orgID}},
	} {
		if err := conn.QueryRow(ctx, s.sql, s.arg...).Scan(s.dst); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.QueryRow(ctx, `INSERT INTO connections (source_id, destination_id) VALUES ($1,$2) RETURNING id`, srcID, dstID).Scan(&connID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO requests (source_id, http_method, http_path, body_hash, body_ref, body_size) VALUES ($1,'POST','/','h','r',1) RETURNING id`, srcID).Scan(&reqID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO events (request_id, connection_id, org_id) VALUES ($1,$2,$3) RETURNING id`, reqID, connID, orgID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}

	ins := func(num int, status any, queued any) {
		t.Helper()
		if _, err := conn.Exec(ctx, `INSERT INTO attempts (event_id, attempt_num, response_status, queued_in_ms, attempted_at) VALUES ($1,$2,$3,$4,$5)`,
			eventID, num, status, queued, since.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("all-NULL queued_in_ms prints n/a", func(t *testing.T) {
		ins(1, 200, nil)
		var out bytes.Buffer
		reportDelivery(&out, dsn, since)
		want := "--- delivery (attempts) ---\nattempts in-window  1\ndelivered (2xx)     1\ndelivery-start p99  n/a (queued_in_ms)\n"
		if out.String() != want {
			t.Fatalf("out = %q", out.String())
		}
	})

	t.Run("p99 over seeded attempts", func(t *testing.T) {
		// attempts 2..5: queued 10, 20, 30, 400; 2 delivered, 1 non-2xx, 1 no response.
		ins(2, 200, 10)
		ins(3, 204, 20)
		ins(4, 500, 30)
		ins(5, nil, 400)
		var out bytes.Buffer
		reportDelivery(&out, dsn, since)
		// percentile_disc(0.99) over {10,20,30,400} = 400
		want := "--- delivery (attempts) ---\nattempts in-window  5\ndelivered (2xx)     3\ndelivery-start p99  400.0 ms (queued_in_ms)\n"
		if out.String() != want {
			t.Fatalf("out = %q", out.String())
		}
	})
}

func TestReportDeliveryDBErrors(t *testing.T) {
	t.Run("unreachable dsn", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		ln.Close()
		var out bytes.Buffer
		reportDelivery(&out, "postgres://u:p@"+addr+"/db?sslmode=disable", time.Now())
		if !strings.HasPrefix(out.String(), "db: connect failed: ") || !strings.Contains(out.String(), "refused") || strings.Contains(out.String(), "attempts") {
			t.Fatalf("out = %q", out.String())
		}
	})
	t.Run("malformed dsn", func(t *testing.T) {
		var out bytes.Buffer
		reportDelivery(&out, "not a url ::", time.Now())
		if !strings.HasPrefix(out.String(), "db: connect failed: ") {
			t.Fatalf("out = %q", out.String())
		}
	})
	t.Run("query failure", func(t *testing.T) {
		// A real connection whose search_path hides the attempts table.
		dsn := testDSN(t)
		alt := dsn + "&options=-c%20search_path%3Dpg_catalog"
		var out bytes.Buffer
		reportDelivery(&out, alt, time.Now())
		if !strings.HasPrefix(out.String(), "db: query failed: ") || !strings.Contains(out.String(), `relation "attempts" does not exist`) {
			t.Fatalf("out = %q", out.String())
		}
	})
}

func TestRunWithDB(t *testing.T) {
	dsn := testDSN(t)
	cs := newCountingServer(0, 200)
	defer cs.Close()
	var out bytes.Buffer
	if err := runB(t, config{url: cs.URL, rate: 20, dur: 100 * time.Millisecond, conc: 2, db: dsn}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	// The window starts at this run; nothing in the repo is delivering to it,
	// but the section header proves reportDelivery ran against the real DB.
	if !strings.Contains(out.String(), "--- delivery (attempts) ---\n") {
		t.Fatalf("no delivery section: %s", out.String())
	}
}

func TestRealMain(t *testing.T) {
	cs := newCountingServer(0, 200)
	defer cs.Close()
	cases := []struct {
		name    string
		args    []string
		code    int
		outHas  string
		errHas  string
		errNone bool
	}{
		{"help exits 0", []string{"-h"}, 0, "", "Usage of prog:", false},
		{"missing url exits 2 with usage", nil, 2, "", "error: -url is required\nUsage of prog:", false},
		{"bad flag exits 2", []string{"-nope"}, 2, "", "flag provided but not defined: -nope", false},
		{"bad rate exits 2", []string{"-url", "u", "-rate", "0"}, 2, "", "error: -rate must be >= 1", false},
		{"run error exits 2", []string{"-url", cs.URL, "-body", "/nonexistent/body.json"}, 2, "", "error: read -body /nonexistent/body.json: ", false},
		{"success exits 0", []string{"-url", cs.URL, "-rate", "20", "-dur", "100ms", "-conc", "2"}, 0, "--- ingest ---\n", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := realMain("prog", c.args, &out, &errOut); code != c.code {
				t.Fatalf("code = %d, want %d (out=%q err=%q)", code, c.code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), c.outHas) || !strings.Contains(errOut.String(), c.errHas) || (c.errNone && errOut.Len() != 0) {
				t.Fatalf("out=%q err=%q", out.String(), errOut.String())
			}
		})
	}
}
