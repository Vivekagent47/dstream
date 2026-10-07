package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// watchdog tracks the last time the worker's consumer loop advanced. The
// consumer goroutines call beat() each iteration (including the idle
// WaitNotify path), so a healthy worker beats at least every couple of
// seconds. If every consumer wedges — a stuck Redis call, a deadlock — beats
// stop and healthy() goes false, which the liveness probe turns into a
// restart. It deliberately does NOT reflect "Redis is down": restarting a
// worker cannot fix an external outage, and flapping on one is worse than
// alerting on it.
type watchdog struct{ last atomic.Int64 }

func newWatchdog() *watchdog {
	wd := &watchdog{}
	wd.beat()
	return wd
}

func (w *watchdog) beat() { w.last.Store(time.Now().UnixNano()) }

func (w *watchdog) healthy(stall time.Duration) bool {
	return time.Since(time.Unix(0, w.last.Load())) < stall
}

// serveWorkerHealth runs a tiny HTTP server exposing /healthz, used as both
// the liveness and readiness source for the worker Deployment. Returns a stop
// func the caller runs on shutdown.
func serveWorkerHealth(ctx context.Context, addr string, wd *watchdog, stall time.Duration, log *slog.Logger) func() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if wd.healthy(stall) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("stalled"))
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("worker health server", "err", err)
		}
	}()
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}
}
