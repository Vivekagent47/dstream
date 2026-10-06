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
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
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

// config is the parsed command line.
type config struct {
	url      string
	rate     int
	dur      time.Duration
	conc     int
	db       string
	sink     bool
	sinkAddr string
	bodyPath string
}

// parseFlags parses args into a config, validating it. Usage and errors go to
// errOut. A missing -url also prints the flag usage.
func parseFlags(prog string, args []string, errOut io.Writer) (config, error) {
	var c config
	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&c.url, "url", "", "full ingest URL incl token, e.g. http://localhost:8080/e/<token> (required)")
	fs.IntVar(&c.rate, "rate", 100, "target requests/sec")
	fs.DurationVar(&c.dur, "dur", 60*time.Second, "how long to send")
	fs.IntVar(&c.conc, "conc", 50, "max concurrent in-flight requests")
	fs.StringVar(&c.db, "db", "", "Postgres URL; if set, query delivery-start p99 after the run")
	fs.BoolVar(&c.sink, "sink", false, "start a local HTTP sink that returns 200 OK for any request")
	fs.StringVar(&c.sinkAddr, "sink-addr", ":9099", "sink server listen address")
	fs.StringVar(&c.bodyPath, "body", "", "path to a JSON body file; empty uses a unique small JSON per request")
	if err := fs.Parse(args); err != nil {
		return c, err
	}

	if c.url == "" {
		fmt.Fprintln(errOut, "error: -url is required")
		fs.Usage()
		return c, errors.New("-url is required")
	}
	if c.rate < 1 {
		fmt.Fprintln(errOut, "error: -rate must be >= 1")
		return c, errors.New("-rate must be >= 1")
	}
	if c.conc < 1 {
		fmt.Fprintln(errOut, "error: -conc must be >= 1")
		return c, errors.New("-conc must be >= 1")
	}
	return c, nil
}

func main() {
	os.Exit(realMain(os.Args[0], os.Args[1:], os.Stdout, os.Stderr))
}

// realMain is main with injectable args and streams; it returns the exit code.
func realMain(prog string, args []string, out, errOut io.Writer) int {
	cfg, err := parseFlags(prog, args, errOut)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	if err := run(cfg, out, errOut); err != nil {
		fmt.Fprintf(errOut, "error: %v\n", err)
		return 2
	}
	return 0
}

// run executes one load test per cfg, writing the report to out.
func run(cfg config, out, errOut io.Writer) error {
	// Fixed body from a file, or nil to generate a unique body per request.
	// A unique body avoids dstream's per-source ingest dedup (60s window),
	// which would otherwise collapse the whole run into one delivered event.
	var fileBody []byte
	if cfg.bodyPath != "" {
		b, err := os.ReadFile(cfg.bodyPath)
		if err != nil {
			return fmt.Errorf("read -body %s: %w", cfg.bodyPath, err)
		}
		fileBody = b
	}

	if cfg.sink {
		stop, _ := startSink(cfg.sinkAddr, out, errOut)
		defer stop()
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        cfg.conc * 2,
			MaxIdleConnsPerHost: cfg.conc * 2,
			MaxConnsPerHost:     cfg.conc * 2,
		},
	}

	var (
		mu        sync.Mutex
		results   []result
		wg        sync.WaitGroup
		seq       int64
		runNonce  = time.Now().UnixNano()
		sem       = make(chan struct{}, cfg.conc)
		ticker    = time.NewTicker(time.Second / time.Duration(cfg.rate))
		startTime = time.Now() // DB window start; captured before the first send
		deadline  = time.After(cfg.dur)
	)
	defer ticker.Stop()

	fmt.Fprintf(out, "loadtest: %s @ %d req/s for %s, conc=%d\n", cfg.url, cfg.rate, cfg.dur, cfg.conc)

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
				r := doSend(client, cfg.url, body)

				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(startTime)

	report(out, results, elapsed)

	if cfg.db != "" {
		reportDelivery(out, cfg.db, startTime)
	}
	return nil
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
func report(out io.Writer, results []result, elapsed time.Duration) {
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

	fmt.Fprintln(out, "--- ingest ---")
	fmt.Fprintf(out, "sent          %d\n", len(results))
	fmt.Fprintf(out, "2xx           %d\n", twoxx)
	fmt.Fprintf(out, "non-2xx       %d\n", nonxx)
	fmt.Fprintf(out, "errors        %d\n", errs)
	fmt.Fprintf(out, "elapsed       %.1fs\n", elapsed.Seconds())
	fmt.Fprintf(out, "throughput    %.1f req/s (2xx/elapsed)\n", float64(twoxx)/elapsed.Seconds())
	fmt.Fprintf(out, "latency ms    p50=%.1f p95=%.1f p99=%.1f max=%.1f\n",
		percentile(lats, 0.50), percentile(lats, 0.95),
		percentile(lats, 0.99), percentile(lats, 1.0))
}

// reportDelivery queries the attempts recorded since the run start and prints
// the delivery-start p99 (queued_in_ms). Handles zero attempts and a NULL
// percentile (all-NULL / empty group) without panicking.
func reportDelivery(out io.Writer, dbURL string, since time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(out, "db: connect failed: %v\n", err)
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
		fmt.Fprintf(out, "db: query failed: %v\n", err)
		return
	}

	fmt.Fprintln(out, "--- delivery (attempts) ---")
	if total == 0 {
		fmt.Fprintln(out, "no attempts recorded in window (worker not running, or nothing delivered yet)")
		return
	}
	p99s := "n/a"
	if p99 != nil {
		p99s = fmt.Sprintf("%.1f ms", *p99)
	}
	fmt.Fprintf(out, "attempts in-window  %d\n", total)
	fmt.Fprintf(out, "delivered (2xx)     %d\n", delivered)
	fmt.Fprintf(out, "delivery-start p99  %s (queued_in_ms)\n", p99s)
}

// startSink runs a background HTTP server that returns 200 OK for any request,
// so a dstream HTTP destination can point at it. The listener is bound before
// it returns. It returns a stop func and the bound address (empty if the bind
// failed, which is reported on errOut and leaves the run going without a sink).
func startSink(addr string, out, errOut io.Writer) (stop func(), bound string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	listenAddr := addr
	if listenAddr == "" {
		listenAddr = ":http" // http.Server.ListenAndServe's default for ""
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(errOut, "sink: %v\n", err)
		return func() {}, ""
	}
	srv := &http.Server{Handler: mux}
	go serveSink(srv, ln, errOut)
	fmt.Fprintf(out, "sink: listening on %s (200 OK for any request)\n", addr)
	return func() { srv.Close() }, ln.Addr().String()
}

// serveSink serves ln until srv is closed, reporting any other failure.
func serveSink(srv *http.Server, ln net.Listener, errOut io.Writer) {
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(errOut, "sink: %v\n", err)
	}
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
