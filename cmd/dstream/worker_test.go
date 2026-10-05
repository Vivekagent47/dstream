package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"

	"github.com/Vivekagent47/dstream/internal/config"
	"github.com/Vivekagent47/dstream/internal/dqueue"
	"github.com/Vivekagent47/dstream/internal/mailer"
)

// waitFor polls cond until it holds or the deadline passes. Every wait in
// this file goes through it so a failed wait fails the test, never hangs it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// workerCfg loads a config pointed at the test database and Redis.
func workerCfg(t *testing.T) config.Config {
	t.Helper()
	useTestDB(t)
	testRedis(t) // skips when Redis is down
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	t.Setenv("DSTREAM_REDIS_ADDR", addr)
	t.Setenv("DSTREAM_LOG_LEVEL", "error")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Worker.Concurrency = 2
	return cfg
}

// testWorker builds the real worker wiring, then scopes its queue to a unique
// Redis prefix so a run never sees (or steals) another test's jobs.
func testWorker(t *testing.T, cfg config.Config) *workerDeps {
	t.Helper()
	w, cleanup, err := buildWorker(context.Background(), cfg, quiet())
	if err != nil {
		t.Fatalf("buildWorker: %v", err)
	}
	t.Cleanup(cleanup)
	prefix := "wktest-" + uuid.NewString()
	w.dq = w.dq.WithPrefix(prefix)
	w.h.Queue = w.dq
	t.Cleanup(func() {
		ctx := context.Background()
		if keys, _ := w.rdb.Keys(ctx, prefix+":*").Result(); len(keys) > 0 {
			w.rdb.Del(ctx, keys...)
		}
	})
	return w
}

// startWorker runs runWorker in the background. stop cancels the signal ctx and
// returns once runWorker has drained and returned (failing the test if it does
// not within the deadline).
func startWorker(t *testing.T, cfg config.Config, w *workerDeps, drain time.Duration) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runWorker(ctx, cfg, w, drain, workerRecoverInterval) }()
	var once sync.Once
	stop = func() {
		once.Do(cancel)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runWorker did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return stop
}

// stubSender is a mailer.Sender the test controls.
type stubSender struct {
	sent      chan mailer.Message // every Send announces itself here
	block     chan struct{}       // when non-nil, Send waits for close(block) or ctx cancel
	doPanic   bool
	ctxErrOut chan error // ctx.Err() observed when a blocked Send resumes
}

func newStubSender() *stubSender {
	return &stubSender{sent: make(chan mailer.Message, 8), ctxErrOut: make(chan error, 8)}
}

func (s *stubSender) Send(ctx context.Context, m mailer.Message) error {
	if s.doPanic {
		panic("boom")
	}
	s.sent <- m
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
		}
		s.ctxErrOut <- ctx.Err()
	}
	return nil
}

func recvMsg(t *testing.T, ch <-chan mailer.Message) mailer.Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the handler to run")
		return mailer.Message{}
	}
}

func enqueueMail(t *testing.T, w *workerDeps, to string) {
	t.Helper()
	if err := mailer.Enqueue(context.Background(), w.dq, "magic_link", to, map[string]any{"Link": "https://example.test/x"}, uuid.Nil); err != nil {
		t.Fatalf("enqueue mail: %v", err)
	}
}

// queueDrained is true once nothing is pending, leased, scheduled or dead.
func queueDrained(t *testing.T, w *workerDeps) func() bool {
	return func() bool {
		s, err := w.dq.Stats(context.Background())
		return err == nil && s.Pending == 0 && s.Processing == 0 && s.Scheduled == 0 && s.Dead == 0
	}
}

// --- buildWorker ---

// The three knobs below are silent-failure settings: a dropped assignment
// would throttle or hang production with no error anywhere.
func TestBuildWorkerWiresConfigKnobs(t *testing.T) {
	cfg := workerCfg(t)
	cfg.Worker.PerOrgMaxInflight = 7
	cfg.TransformTimeout = 3 * time.Second
	cfg.TransformMaxOutput = 12345
	cfg.EndpointMaxConsecutiveFailures = 9
	cfg.DevMode = true
	w := testWorker(t, cfg)

	if w.h.PerOrgMaxInflight != 7 || w.outbound.PerOrgMaxInflight != 7 {
		t.Errorf("PerOrgMaxInflight deliver=%d outbound=%d, want 7 on both", w.h.PerOrgMaxInflight, w.outbound.PerOrgMaxInflight)
	}
	if w.h.TransformTimeout != 3*time.Second || w.outbound.TransformTimeout != 3*time.Second {
		t.Errorf("TransformTimeout deliver=%v outbound=%v, want 3s on both", w.h.TransformTimeout, w.outbound.TransformTimeout)
	}
	if w.h.TransformMaxOutput != 12345 || w.outbound.TransformMaxOutput != 12345 {
		t.Errorf("TransformMaxOutput deliver=%d outbound=%d, want 12345 on both", w.h.TransformMaxOutput, w.outbound.TransformMaxOutput)
	}
	if w.outbound.MaxConsecutiveFailures != 9 {
		t.Errorf("MaxConsecutiveFailures = %d, want 9", w.outbound.MaxConsecutiveFailures)
	}
	if !w.email.DevMode {
		t.Error("email handler lost DevMode")
	}
	if w.email.Sender != nil {
		t.Errorf("Sender = %v, want nil when SMTP is not configured", w.email.Sender)
	}
	// The built pieces are live, not just populated.
	if err := w.rdb.Ping(context.Background()).Err(); err != nil {
		t.Errorf("redis ping: %v", err)
	}
	if _, err := w.q.ListOrgQuotas(context.Background()); err != nil {
		t.Errorf("query through built pool: %v", err)
	}
}

func TestBuildWorkerWiresSMTPSender(t *testing.T) {
	cfg := workerCfg(t)
	cfg.SMTP = config.SMTPConfig{Host: "smtp.example.test", Port: 587, From: "a@example.test"}
	w := testWorker(t, cfg)
	if w.email.Sender == nil {
		t.Fatal("Sender is nil, want an SMTP sender when a host is configured")
	}
}

// pgConns counts server-side connections tagged with an application_name, via
// a separate connection, so a test can see whether a build left any open.
func pgConns(t *testing.T, app string) int {
	t.Helper()
	var n int
	err := testPool(t).QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1`, app).Scan(&n)
	if err != nil {
		t.Fatalf("pg_stat_activity: %v", err)
	}
	return n
}

// urlSep picks the separator for appending a query parameter to a DSN.
func urlSep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

// Note: a failed build returns no handle, and Redis dials lazily, so the
// release of Redis and tracing on unwind is not observable from outside; the
// DB pool is. Redis release on cleanup is asserted below.
func TestBuildWorkerUnwindsWhenLaterStepFails(t *testing.T) {
	cfg := workerCfg(t)
	app := "wk-unwind-" + uuid.NewString()
	cfg.DB.URL += urlSep(cfg.DB.URL) + "application_name=" + app
	cfg.SMTP = config.SMTPConfig{Host: "smtp.example.test", Port: 70000} // pool and redis are already open here

	w, cleanup, err := buildWorker(context.Background(), cfg, quiet())
	if err == nil {
		cleanup()
		t.Fatal("buildWorker succeeded with an invalid SMTP port")
	}
	if !strings.Contains(err.Error(), "init mailer:") {
		t.Errorf("err = %v, want it to name the mailer step", err)
	}
	if w != nil || cleanup != nil {
		t.Error("a failed build must return no deps and no cleanup")
	}
	// The pool was opened before the failure; it must be closed again.
	waitFor(t, "failed build to release its DB connections", func() bool { return pgConns(t, app) == 0 })
}

func TestBuildWorkerSuccessHoldsConnectionsUntilCleanup(t *testing.T) {
	cfg := workerCfg(t)
	app := "wk-hold-" + uuid.NewString()
	cfg.DB.URL += urlSep(cfg.DB.URL) + "application_name=" + app
	w, cleanup, err := buildWorker(context.Background(), cfg, quiet())
	if err != nil {
		t.Fatalf("buildWorker: %v", err)
	}
	if err := w.rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping while open: %v", err)
	}
	if n := pgConns(t, app); n == 0 {
		t.Fatal("a successful build holds no DB connection; the unwind test above proves nothing")
	}
	cleanup()
	waitFor(t, "cleanup to release the DB connections", func() bool { return pgConns(t, app) == 0 })
	if err := w.rdb.Ping(context.Background()).Err(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("redis ping after cleanup = %v, want a closed-client error", err)
	}
}

func TestBuildWorkerUnreachableDatabase(t *testing.T) {
	cfg := workerCfg(t)
	cfg.DB.URL = "postgres://dstream:dstream@127.0.0.1:1/none?sslmode=disable"
	w, cleanup, err := buildWorker(context.Background(), cfg, quiet())
	if err == nil {
		cleanup()
		t.Fatal("buildWorker succeeded against an unreachable database")
	}
	if !strings.Contains(err.Error(), "ping db") {
		t.Errorf("err = %v, want a ping db failure", err)
	}
	if w != nil || cleanup != nil {
		t.Error("a failed build must return no deps and no cleanup")
	}
}

// --- runWorker ---

func TestWorkerConsumesEmailJob(t *testing.T) {
	cfg := workerCfg(t)
	w := testWorker(t, cfg)
	s := newStubSender()
	w.email.Sender = s
	stop := startWorker(t, cfg, w, time.Second)

	to := "consume-" + uuid.NewString() + "@example.test"
	enqueueMail(t, w, to)
	got := recvMsg(t, s.sent)
	if got.To != to {
		t.Errorf("handler got To=%q, want %q", got.To, to)
	}
	// Ack follows the send; once it lands the queue is empty and nothing dead.
	waitFor(t, "the job to be acked", queueDrained(t, w))
	stop()
}

// A job scheduled for "now" only reaches the handler if the 1s promoter tick
// moves it from the scheduled set into the pending ring.
func TestWorkerPromotesScheduledJob(t *testing.T) {
	cfg := workerCfg(t)
	w := testWorker(t, cfg)
	s := newStubSender()
	w.email.Sender = s

	data, _ := json.Marshal(map[string]any{"template": "magic_link", "to": "sched-" + uuid.NewString() + "@example.test", "vars": map[string]any{"Link": "https://example.test/x"}})
	p := dqueue.Payload{Kind: "email", OrgID: uuid.Nil, Data: data}
	if err := w.dq.Schedule(context.Background(), p, time.Now().UnixMilli()); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if st, _ := w.dq.Stats(context.Background()); st.Scheduled != 1 {
		t.Fatalf("scheduled = %d before the worker starts, want 1", st.Scheduled)
	}
	startWorker(t, cfg, w, time.Second)
	recvMsg(t, s.sent)
	waitFor(t, "the promoted job to be acked", queueDrained(t, w))
}

// Delivery and outbound-message jobs whose rows no longer exist are acked, not
// retried forever. Each reaching an empty queue proves the right handler ran.
func TestWorkerRoutesDeliveryAndMessageJobs(t *testing.T) {
	cfg := workerCfg(t)
	w := testWorker(t, cfg)
	data, _ := json.Marshal(map[string]string{"delivery_id": uuid.NewString()})
	jobs := map[string]dqueue.Payload{
		"inbound delivery":  {EventID: uuid.New(), OrgID: uuid.New()},
		"outbound message":  {EventID: uuid.New(), OrgID: uuid.New(), Kind: "message", Data: data},
		"malformed message": {EventID: uuid.New(), OrgID: uuid.New(), Kind: "message", Data: []byte(`{}`)},
	}
	for name, p := range jobs {
		if err := w.dq.Enqueue(context.Background(), p); err != nil {
			t.Fatalf("%s: enqueue: %v", name, err)
		}
	}
	stop := startWorker(t, cfg, w, time.Second)
	// The malformed message is dead-lettered by the outbound handler; the other
	// two are acked. Exactly one dead entry and nothing left pending or leased.
	waitFor(t, "all three jobs to be handled", func() bool {
		s, err := w.dq.Stats(context.Background())
		return err == nil && s.Pending == 0 && s.Processing == 0 && s.Dead == 1
	})
	stop()
	s, _ := w.dq.Stats(context.Background())
	if s.Dead != 1 || s.Processing != 0 {
		t.Errorf("after drain dead=%d processing=%d, want 1 and 0", s.Dead, s.Processing)
	}
}

// A panic in a handler must not kill the worker: the poisoned job is
// dead-lettered and the next job still runs.
func TestWorkerContainsHandlerPanics(t *testing.T) {
	cfg := workerCfg(t)
	cfg.Worker.Concurrency = 1
	w := testWorker(t, cfg)
	ctx := context.Background()

	bad := newStubSender()
	bad.doPanic = true
	w.email.Sender = bad
	w.h.Queries = nil        // inbound handler panics on first use
	w.outbound.Queries = nil // outbound handler too
	data, _ := json.Marshal(map[string]string{"delivery_id": uuid.NewString()})
	enqueueMail(t, w, "panic-"+uuid.NewString()+"@example.test")
	for _, p := range []dqueue.Payload{
		{EventID: uuid.New(), OrgID: uuid.New()},
		{EventID: uuid.New(), OrgID: uuid.New(), Kind: "message", Data: data},
	} {
		if err := w.dq.Enqueue(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	stop := startWorker(t, cfg, w, time.Second)
	waitFor(t, "all three panicking jobs to be dead-lettered", func() bool {
		s, err := w.dq.Stats(ctx)
		return err == nil && s.Dead == 3 && s.Pending == 0 && s.Processing == 0
	})
	stop()
}

// SIGTERM stops picking new work but lets an in-flight delivery finish on a
// context that is NOT cancelled by the signal.
func TestWorkerDrainsInFlightJobOnShutdown(t *testing.T) {
	cfg := workerCfg(t)
	w := testWorker(t, cfg)
	s := newStubSender()
	s.block = make(chan struct{})
	w.email.Sender = s
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runWorker(ctx, cfg, w, 30*time.Second, workerRecoverInterval) }()
	release := sync.OnceFunc(func() { close(s.block) })
	t.Cleanup(func() { cancel(); release() })

	enqueueMail(t, w, "drain-"+uuid.NewString()+"@example.test")
	recvMsg(t, s.sent) // the send is now in flight
	cancel()           // SIGTERM

	// Still draining: runWorker must not have returned while the send is held.
	select {
	case <-done:
		t.Fatal("runWorker returned with a delivery still in flight")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-s.ctxErrOut:
		if err != nil {
			t.Errorf("in-flight send ctx err = %v, want nil: the signal must not cancel a delivery", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight send never resumed")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runWorker did not return after the in-flight delivery finished")
	}
	if !queueDrained(t, w)() {
		t.Error("the drained delivery was not acked")
	}
}

// A hung delivery cannot block shutdown: after the drain window its context is
// cancelled and runWorker returns.
func TestWorkerForceCancelsAfterDrainWindow(t *testing.T) {
	cfg := workerCfg(t)
	w := testWorker(t, cfg)
	s := newStubSender()
	s.block = make(chan struct{}) // never closed: only ctx cancel releases the send
	w.email.Sender = s
	stop := startWorker(t, cfg, w, 100*time.Millisecond)
	enqueueMail(t, w, "hung-"+uuid.NewString()+"@example.test")
	recvMsg(t, s.sent)
	stop()
	select {
	case err := <-s.ctxErrOut:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("hung send ctx err = %v, want context.Canceled after the drain window", err)
		}
	default:
		t.Error("the hung send was never released")
	}
}

// Handler errors are logged, not swallowed, and a failing job stays leased for
// the recoverer instead of being lost.
func TestWorkerLogsHandlerErrors(t *testing.T) {
	cfg := workerCfg(t)
	cfg.Worker.Concurrency = 1
	w := testWorker(t, cfg)
	logs := &logBuf{}
	w.log = logs.logger()
	q := closedQueries(t)
	w.h.Queries = q
	w.outbound.Queries = q
	data, _ := json.Marshal(map[string]string{"delivery_id": uuid.NewString()})
	for _, p := range []dqueue.Payload{
		{EventID: uuid.New(), OrgID: uuid.New()},
		{EventID: uuid.New(), OrgID: uuid.New(), Kind: "message", Data: data},
	} {
		if err := w.dq.Enqueue(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	stop := startWorker(t, cfg, w, time.Second)
	waitFor(t, "both handler errors to be logged", func() bool {
		out := logs.String()
		return strings.Contains(out, "outbound process") && strings.Contains(out, "process event_id")
	})
	stop()
	out := logs.String()
	if !strings.Contains(out, "load event") || strings.Count(out, "closed pool") < 2 {
		t.Errorf("handler errors logged without their cause:\n%s", out)
	}
	if s, _ := w.dq.Stats(context.Background()); s.Processing != 2 || s.Dead != 0 {
		t.Errorf("processing=%d dead=%d, want both failed jobs left leased (2) and none dead", s.Processing, s.Dead)
	}
}

// A queue error is logged and the pull loop keeps running rather than exiting.
func TestWorkerSurvivesQueueErrors(t *testing.T) {
	cfg := workerCfg(t)
	cfg.Worker.Concurrency = 1
	w := testWorker(t, cfg)
	logs := &logBuf{}
	w.log = logs.logger()
	dead := testRedis(t)
	_ = dead.Close() // every command now fails with "client is closed"
	w.dq = dqueue.NewClient(dead)

	stop := startWorker(t, cfg, w, time.Second)
	waitFor(t, "the fairpick error to be logged", func() bool { return strings.Contains(logs.String(), "fairpick") })
	// A second occurrence proves the loop retried instead of returning.
	waitFor(t, "the loop to retry after the error", func() bool { return strings.Count(logs.String(), "fairpick") >= 2 })
	stop()
	if !strings.Contains(logs.String(), "client is closed") {
		t.Errorf("fairpick error logged without its cause:\n%s", logs.String())
	}
}

// cancelOnFairPick is a redis hook that cancels the worker's context from inside
// the FairPick script call (identified by its deadline argument, now plus the
// 150s lease; the other scripts are passed roughly now) and then
// fails the call, so the cancel deterministically lands in a failing FairPick.
type cancelOnFairPick struct {
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (h *cancelOnFairPick) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *cancelOnFairPick) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *cancelOnFairPick) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		for _, a := range cmd.Args() {
			if n, ok := a.(int64); ok && n > time.Now().UnixMilli()+100000 {
				h.fired.Store(true)
				h.cancel()
				return errors.New("boom")
			}
		}
		return next(ctx, cmd)
	}
}

// A FairPick that fails because the worker is shutting down is not an error to
// log and retry: the pull loop must just return.
func TestWorkerPullLoopReturnsQuietlyWhenCancelledDuringFairPick(t *testing.T) {
	cfg := workerCfg(t)
	cfg.Worker.Concurrency = 1
	w := testWorker(t, cfg)
	logs := &logBuf{}
	w.log = logs.logger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hook := &cancelOnFairPick{cancel: cancel}
	rdb := testRedis(t)
	rdb.AddHook(hook)
	w.dq = dqueue.NewClient(rdb).WithPrefix("wktest-" + uuid.NewString())

	done := make(chan struct{})
	go func() { defer close(done); runWorker(ctx, cfg, w, time.Second, workerRecoverInterval) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runWorker did not return")
	}
	if !hook.fired.Load() {
		t.Fatal("the hook never saw a FairPick call")
	}
	if strings.Contains(logs.String(), "fairpick") {
		t.Errorf("a cancelled FairPick was logged as an error:\n%s", logs.String())
	}
}

// --- workerCmd ---

func TestWorkerCmdRejectsBadConfig(t *testing.T) {
	t.Setenv("DSTREAM_LOG_LEVEL", "error")
	t.Setenv("DSTREAM_WORKER_CONCURRENCY", "not-a-number")
	_, _, err := execCmd(t, workerCmd())
	if err == nil || !strings.Contains(err.Error(), "unmarshal config") {
		t.Fatalf("err = %v, want the config unmarshal failure", err)
	}
}

func TestWorkerCmdFailsWhenDatabaseUnreachable(t *testing.T) {
	useTestDB(t)
	t.Setenv("DSTREAM_DB_URL", "postgres://dstream:dstream@127.0.0.1:1/none?sslmode=disable")
	t.Setenv("DSTREAM_LOG_LEVEL", "error")
	_, _, err := execCmd(t, workerCmd())
	if err == nil || !strings.Contains(err.Error(), "ping db") {
		t.Fatalf("err = %v, want a ping db failure", err)
	}
}

// The real command runs until SIGTERM, then drains and returns nil.
func TestWorkerCmdRunsUntilSigterm(t *testing.T) {
	workerCfg(t) // env for the test DB and Redis, or skip
	// Knowingly runs against the shared default dqueue ring and DB (the command
	// builds its own deps, so there is no prefix hook): acceptable because this
	// test enqueues nothing and asserts only that the worker exits cleanly.
	// Our own handler guarantees an early SIGTERM can never kill the test
	// binary, whatever the timing against the command's own registration.
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGTERM)
	t.Cleanup(func() { signal.Stop(sigs) })

	done := make(chan error, 1)
	go func() {
		_, _, err := execCmd(t, workerCmd())
		done <- err
	}()
	deadline := time.After(30 * time.Second)
	for first := true; ; first = false {
		select {
		case err := <-done:
			if first {
				t.Fatalf("worker exited before it was signalled: %v", err)
			}
			if err != nil {
				t.Fatalf("worker returned %v on SIGTERM, want a clean nil", err)
			}
			return
		case <-deadline:
			t.Fatal("worker did not exit after SIGTERM")
		case <-time.After(100 * time.Millisecond):
			// Re-sent until the command is up and has registered its handler.
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}
	}
}

func TestBuildWorkerTracing(t *testing.T) {
	cfg := workerCfg(t)
	cfg.Tracing.Enabled = true
	cfg.Tracing.OTLPEndpoint = "http://127.0.0.1:1"
	cfg.Tracing.SampleRatio = 1
	// Init installs a global provider; put the previous one back afterwards.
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	logs := &logBuf{}
	// The exporter connects lazily, so an unreachable collector must not fail the build.
	_, cleanup, err := buildWorker(context.Background(), cfg, logs.logger())
	if err != nil {
		t.Fatalf("buildWorker: %v", err)
	}
	t.Cleanup(cleanup)
	if out := logs.String(); !strings.Contains(out, "tracing enabled") || !strings.Contains(out, "127.0.0.1:1") {
		t.Errorf("no tracing-enabled line naming the endpoint:\n%s", out)
	}
}

// A worker that dies mid-delivery leaves its event leased. The recover tick must
// give it back to the queue; with no pull loops running, nothing else can.
func TestWorkerRecoversExpiredLeasesOnTick(t *testing.T) {
	cfg := workerCfg(t)
	cfg.Worker.Concurrency = 0
	w := testWorker(t, cfg)
	ctx := context.Background()
	enqueueMail(t, w, "recover-"+uuid.NewString()+"@example.test")
	// Lease it for 1ms and never ack: a crashed worker's footprint.
	if _, _, ok, err := w.dq.FairPick(ctx, 1); err != nil || !ok {
		t.Fatalf("FairPick = ok %v err %v, want the event leased", ok, err)
	}
	if s, _ := w.dq.Stats(ctx); s.Processing != 1 {
		t.Fatalf("processing = %d before the worker starts, want 1", s.Processing)
	}

	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); runWorker(rctx, cfg, w, time.Second, 20*time.Millisecond) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runWorker did not return after cancel")
		}
	})

	waitFor(t, "the expired lease to be recovered", func() bool {
		s, err := w.dq.Stats(ctx)
		return err == nil && s.Processing == 0
	})
	if s, _ := w.dq.Stats(ctx); s.Pending+s.Scheduled != 1 || s.Dead != 0 {
		t.Errorf("stats = %+v, want the one event back in pending/scheduled, not lost or dead", s)
	}
}

// --- helpers ---

func TestMessageDeliveryID(t *testing.T) {
	want := uuid.New()
	cases := []struct {
		name    string
		data    string
		want    uuid.UUID
		wantErr string
	}{
		{"well-formed", `{"delivery_id":"` + want.String() + `"}`, want, ""},
		{"missing id", `{"other":"x"}`, uuid.Nil, "invalid UUID length: 0"},
		{"malformed id", `{"delivery_id":"not-a-uuid"}`, uuid.Nil, "invalid UUID length: 10"},
		{"not json", `nope`, uuid.Nil, "invalid character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := messageDeliveryID(dqueue.Payload{Data: []byte(c.data)})
			if c.wantErr == "" {
				if err != nil || got != c.want {
					t.Fatalf("got (%v, %v), want (%v, nil)", got, err, c.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
			}
			if got != uuid.Nil {
				t.Errorf("id = %v on error, want uuid.Nil", got)
			}
		})
	}
}

func TestTickFiresAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int64
	done := make(chan struct{})
	go func() { defer close(done); tick(ctx, 5*time.Millisecond, func() { n.Add(1) }) }()

	waitFor(t, "tick to fire", func() bool { return n.Load() >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not return after its context was cancelled")
	}
}
