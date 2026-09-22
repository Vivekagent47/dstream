// Command loadtest is a standalone ingest load-test harness for dstream.
//
// It fires ingest POSTs at a target rate for a fixed duration, reports ingest
// latency percentiles, and (optionally) queries the attempts table for the
// delivery-start p99. It is NOT imported by the dstream binary — stdlib + pgx
// only. See README.md for setup and usage.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

type result struct {
	latencyMs float64
	status    int
	err       error
}

func main() {
	var (
		url      = flag.String("url", "", "full ingest URL incl token, e.g. http://localhost:8080/e/<token> (required)")
		rate     = flag.Int("rate", 100, "target requests/sec")
		dur      = flag.Duration("dur", 60*time.Second, "how long to send")
		conc     = flag.Int("conc", 50, "max concurrent in-flight requests")
		db       = flag.String("db", "", "Postgres URL; if set, query delivery-start p99 after the run")
		sink     = flag.Bool("sink", false, "start a local HTTP sink that returns 200 OK for any request")
		sinkAddr = flag.String("sink-addr", ":9099", "sink server listen address")
		bodyPath = flag.String("body", "", "path to a JSON body file; empty uses a unique small JSON per request")
	)
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required")
		flag.Usage()
		os.Exit(2)
	}
	if *rate < 1 {
		fmt.Fprintln(os.Stderr, "error: -rate must be >= 1")
		os.Exit(2)
	}
	if *conc < 1 {
		fmt.Fprintln(os.Stderr, "error: -conc must be >= 1")
		os.Exit(2)
	}

	// Fixed body from a file, or nil to generate a unique body per request.
	// A unique body avoids dstream's per-source ingest dedup (60s window),
	// which would otherwise collapse the whole run into one delivered event.
	var fileBody []byte
	if *bodyPath != "" {
		b, err := os.ReadFile(*bodyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read -body %s: %v\n", *bodyPath, err)
			os.Exit(2)
		}
		fileBody = b
	}

	if *sink {
		startSink(*sinkAddr)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *conc * 2,
			MaxIdleConnsPerHost: *conc * 2,
			MaxConnsPerHost:     *conc * 2,
		},
	}

	var (
		mu        sync.Mutex
		results   []result
		wg        sync.WaitGroup
		seq       int64
		runNonce  = time.Now().UnixNano()
		sem       = make(chan struct{}, *conc)
		ticker    = time.NewTicker(time.Second / time.Duration(*rate))
		startTime = time.Now() // DB window start; captured before the first send
		deadline  = time.After(*dur)
	)
	defer ticker.Stop()

	fmt.Printf("loadtest: %s @ %d req/s for %s, conc=%d\n", *url, *rate, *dur, *conc)

loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ticker.C:
			// Bound concurrency, but still honor the deadline if saturated.
			select {
			case sem <- struct{}{}:
			case <-deadline:
				break loop
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				body := fileBody
				if body == nil {
					n := atomic.AddInt64(&seq, 1)
					body = fmt.Appendf(nil, `{"loadtest":true,"run":%d,"n":%d}`, runNonce, n)
				}
				r := doSend(client, *url, body)

				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(startTime)

	report(results, elapsed)

	if *db != "" {
		reportDelivery(*db, startTime)
	}
}

// doSend POSTs body to url and records the round-trip latency, status, and error.
func doSend(client *http.Client, url string, body []byte) result {
	t0 := time.Now()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result{latencyMs: msSince(t0), err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	lat := msSince(t0)
	if err != nil {
		return result{latencyMs: lat, err: err}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return result{latencyMs: lat, status: resp.StatusCode}
}

func msSince(t0 time.Time) float64 {
	return float64(time.Since(t0)) / float64(time.Millisecond)
}

// report prints send totals and ingest latency percentiles (2xx only).
func report(results []result, elapsed time.Duration) {
	var lats []float64
	var twoxx, nonxx, errs int
	for _, r := range results {
		switch {
		case r.err != nil:
			errs++
		case r.status >= 200 && r.status < 300:
			twoxx++
			lats = append(lats, r.latencyMs)
		default:
			nonxx++
		}
	}
	sort.Float64s(lats)

	fmt.Println("--- ingest ---")
	fmt.Printf("sent          %d\n", len(results))
	fmt.Printf("2xx           %d\n", twoxx)
	fmt.Printf("non-2xx       %d\n", nonxx)
	fmt.Printf("errors        %d\n", errs)
	fmt.Printf("elapsed       %.1fs\n", elapsed.Seconds())
	fmt.Printf("throughput    %.1f req/s (2xx/elapsed)\n", float64(twoxx)/elapsed.Seconds())
	fmt.Printf("latency ms    p50=%.1f p95=%.1f p99=%.1f max=%.1f\n",
		percentile(lats, 0.50), percentile(lats, 0.95),
		percentile(lats, 0.99), percentile(lats, 1.0))
}

// reportDelivery queries the attempts recorded since the run start and prints
// the delivery-start p99 (queued_in_ms). Handles zero attempts and a NULL
// percentile (all-NULL / empty group) without panicking.
func reportDelivery(dbURL string, since time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		fmt.Printf("db: connect failed: %v\n", err)
		return
	}
	defer conn.Close(ctx)

	// response_status is the attempts column (verified in db/schema/schema.sql
	// and internal/store/models.go). ::float8 makes the NULL/empty-group case
	// scan cleanly into *float64.
	const q = `SELECT count(*),
       count(*) FILTER (WHERE response_status BETWEEN 200 AND 299),
       percentile_disc(0.99) WITHIN GROUP (ORDER BY queued_in_ms)::float8
FROM attempts
WHERE attempted_at >= $1`

	var total, delivered int64
	var p99 *float64
	if err := conn.QueryRow(ctx, q, since).Scan(&total, &delivered, &p99); err != nil {
		fmt.Printf("db: query failed: %v\n", err)
		return
	}

	fmt.Println("--- delivery (attempts) ---")
	if total == 0 {
		fmt.Println("no attempts recorded in window (worker not running, or nothing delivered yet)")
		return
	}
	p99s := "n/a"
	if p99 != nil {
		p99s = fmt.Sprintf("%.1f ms", *p99)
	}
	fmt.Printf("attempts in-window  %d\n", total)
	fmt.Printf("delivered (2xx)     %d\n", delivered)
	fmt.Printf("delivery-start p99  %s (queued_in_ms)\n", p99s)
}

// startSink runs a background HTTP server that returns 200 OK for any request,
// so a dstream HTTP destination can point at it. Blocks briefly so the listener
// is bound before the run begins.
func startSink(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "sink: %v\n", err)
		}
	}()
	fmt.Printf("sink: listening on %s (200 OK for any request)\n", addr)
	time.Sleep(100 * time.Millisecond) // ponytail: let the listener bind; good enough for a manual tool
}

// percentile returns the q-quantile (0..1) of an ascending-sorted slice using
// the nearest-rank method: rank = ceil(q*n), value = sorted[rank-1]. A small
// epsilon guards ceil against a q*n that lands a hair above an integer due to
// float rounding. Empty slice returns 0.
func percentile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[n-1]
	}
	rank := int(math.Ceil(q*float64(n) - 1e-9))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}
