package cli

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/store"
)

// fnHook is a go-redis hook around the REAL client: it intercepts commands the
// handler issues so error and panic paths can be provoked against real Redis.
type fnHook struct {
	f func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error
}

func (h fnHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, n, a string) (net.Conn, error) { return next(ctx, n, a) }
}

func (h fnHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { return h.f(ctx, cmd, next) }
}

func (h fnHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func hookedRedis(t *testing.T, f func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error) *redis.Client {
	t.Helper()
	base := testRedis(t)
	opts := base.Options()
	base.Close()
	c := redis.NewClient(opts)
	c.AddHook(fnHook{f})
	t.Cleanup(func() { c.Close() })
	return c
}

// A BLPOP that errors, then one that returns a short reply, must not stop the
// dispatch loop: the next real event is still delivered.
func TestConnectDispatchLoopSurvivesBLPopFaults(t *testing.T) {
	var blpops atomic.Int32
	rdb := hookedRedis(t, func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() != "blpop" {
			return next(ctx, cmd)
		}
		switch blpops.Add(1) {
		case 1:
			return errors.New("injected blpop error")
		case 2:
			cmd.(*redis.StringSliceCmd).SetVal([]string{"short"})
			return nil
		}
		return next(ctx, cmd)
	})
	e := newEnv(t, func(h *Handlers) { h.Redis = rdb })
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	c := e.dial(org, src)
	helloFor(t, c, src)
	e.pushEvent(src, ev)
	if f := read(t, c); !strings.Contains(f, `"event_id":"`+ev.String()+`"`) {
		t.Fatalf("event frame = %s", f)
	}
	if blpops.Load() < 3 {
		t.Fatalf("loop made only %d BLPOP calls", blpops.Load())
	}
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
}

// A BLPOP failing because the session ctx was cancelled ends the loop (and so
// the tunnel) instead of spinning. NOTE: this reaches the `ctx.Err() != nil`
// return but does not pin it; turning it into `continue` ends the loop via the
// loop-top ctx check just the same.
func TestConnectBLPopErrorAfterCancelEndsTunnel(t *testing.T) {
	rdb := hookedRedis(t, func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() == "blpop" {
			<-ctx.Done()
			return ctx.Err()
		}
		return next(ctx, cmd)
	})
	e := newEnv(t, func(h *Handlers) { h.Redis = rdb })
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	c := e.dial(org, src)
	helloFor(t, c, src)
	c.Close(websocket.StatusNormalClosure, "")
	// No wake-up push needed: the hook honours ctx, as a real cancelled BLPOP would.
	poll(t, "tunnel teardown", func() bool { return sessionsActive(t) == base })
	poll(t, "session key released", func() bool { return e.rdb.Exists(context.Background(), SessionKey(src)).Val() == 0 })
}

// A panic in the ping goroutine is contained: logged, tunnel keeps serving.
func TestConnectPingPanicIsContained(t *testing.T) {
	var sets atomic.Int32
	rdb := hookedRedis(t, func(ctx context.Context, cmd redis.Cmder, next redis.ProcessHook) error {
		if cmd.Name() == "set" && sets.Add(1) == 2 { // 1 = registration, 2 = first ping
			panic("injected ping panic")
		}
		return next(ctx, cmd)
	})
	e := newEnv(t, func(h *Handlers) { h.Redis = rdb; h.pingEvery = 20 * time.Millisecond })
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	c := e.dial(org, src)
	helloFor(t, c, src)
	poll(t, "ping panic logged", func() bool {
		l := e.log.String()
		return strings.Contains(l, "goroutine=ping") && strings.Contains(l, "injected ping panic")
	})
	e.pushEvent(src, ev)
	if f := read(t, c); !strings.Contains(f, `"event_id":"`+ev.String()+`"`) {
		t.Fatalf("tunnel stopped serving after the ping panic: %s", f)
	}
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
}

// hookDB wraps the real pool and lets a test intercept the event load.
type hookDB struct {
	store.DBTX
	onEventLoad func(ctx context.Context)
}

func (h hookDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "name: GetEventForDelivery") {
		h.onEventLoad(ctx)
	}
	return h.DBTX.QueryRow(ctx, sql, args...)
}

// Panics while dispatching are contained per event and release their
// semaphore slot: after maxConcurrentDispatch panicking events, a real event
// is still dispatched (a leaked slot per panic would park it forever).
func TestConnectDispatchPanicIsContained(t *testing.T) {
	var loads atomic.Int32
	db := hookDB{DBTX: testPool(t), onEventLoad: func(context.Context) {
		if loads.Add(1) <= maxConcurrentDispatch {
			panic("injected dispatch panic")
		}
	}}
	e := newEnv(t, func(h *Handlers) { h.Queries = store.New(db) })
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	ev := e.seedEvent(org, src, []byte(`{}`), "")
	c := e.dial(org, src)
	helloFor(t, c, src)
	first := uuid.New()
	e.pushEvent(src, first)
	for i := 1; i < maxConcurrentDispatch; i++ {
		e.pushEvent(src, uuid.New())
	}
	e.pushEvent(src, ev)
	if f := read(t, c); !strings.Contains(f, `"event_id":"`+ev.String()+`"`) {
		t.Fatalf("real event not dispatched after the panics: %s", f)
	}
	l := e.log.String()
	if !strings.Contains(l, "goroutine=dispatch") || !strings.Contains(l, "event_id="+first.String()) || !strings.Contains(l, "injected dispatch panic") {
		t.Fatalf("dispatch panic not logged: %s", l)
	}
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
}

// With maxConcurrentDispatch events in flight the loop parks on the semaphore;
// closing the tunnel must release it without starting the extra event.
func TestConnectSemaphoreParkedLoopUnwindsOnClose(t *testing.T) {
	var started atomic.Int32
	db := hookDB{DBTX: testPool(t), onEventLoad: func(ctx context.Context) {
		started.Add(1)
		<-ctx.Done() // an unanswered event: unwinds only when the tunnel closes
	}}
	e := newEnv(t, func(h *Handlers) { h.Queries = store.New(db) })
	org := e.seedOrg()
	src := e.seedSource(org, "s")
	base := sessionsActive(t)
	c := e.dial(org, src)
	helloFor(t, c, src)

	for i := 0; i < maxConcurrentDispatch+1; i++ {
		e.pushEvent(src, uuid.New())
	}
	// 64 in flight, and the 65th already popped (list drained) so the loop is on the semaphore.
	poll(t, "64 events in flight and the 65th popped", func() bool {
		return started.Load() == maxConcurrentDispatch && e.rdb.LLen(context.Background(), DispatchKey(src)).Val() == 0
	})
	c.Close(websocket.StatusNormalClosure, "")
	e.waitSessions(src, base)
	if got := started.Load(); got != maxConcurrentDispatch {
		t.Fatalf("%d events started; the 65th must never run", got)
	}
}
