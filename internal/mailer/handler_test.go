package mailer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Vivekagent47/dstream/internal/dqueue"
)

type fakeSender struct {
	err   error
	calls int
}

func (f *fakeSender) Send(ctx context.Context, m Message) error {
	f.calls++
	return f.err
}

func testQueue(t *testing.T) (*dqueue.Client, *redis.Client) {
	t.Helper()
	addr := os.Getenv("DSTREAM_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skip("no redis at " + addr)
	}
	if keys, _ := rdb.Keys(context.Background(), "mailtest:*").Result(); len(keys) > 0 {
		rdb.Del(context.Background(), keys...)
	}
	return dqueue.NewClient(rdb).WithPrefix("mailtest"), rdb
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func leaseOne(t *testing.T, q *dqueue.Client) (string, dqueue.Payload) {
	t.Helper()
	raw, p, ok, err := q.FairPick(context.Background(), 10000)
	if err != nil || !ok {
		t.Fatalf("fairpick ok=%v err=%v", ok, err)
	}
	return raw, p
}

func TestEmailHandlerSendOKAcks(t *testing.T) {
	q, rdb := testQueue(t)
	ctx := context.Background()
	if err := Enqueue(ctx, q, "magic_link", "a@b.com", map[string]any{"Link": "https://x/y"}, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	raw, p := leaseOne(t, q)
	fs := &fakeSender{}
	h := EmailHandler{Sender: fs, Log: discardLog()}
	if err := h.Process(ctx, p, raw, q); err != nil {
		t.Fatal(err)
	}
	if fs.calls != 1 {
		t.Fatalf("want 1 send, got %d", fs.calls)
	}
	if n, _ := rdb.ZCard(ctx, "mailtest:processing").Result(); n != 0 {
		t.Fatalf("want acked (0 processing), got %d", n)
	}
}

func TestEmailHandlerRetrySchedules(t *testing.T) {
	q, rdb := testQueue(t)
	ctx := context.Background()
	_ = Enqueue(ctx, q, "magic_link", "a@b.com", map[string]any{"Link": "https://x/y"}, uuid.Nil)
	raw, p := leaseOne(t, q)
	h := EmailHandler{Sender: &fakeSender{err: errors.New("smtp down")}, Log: discardLog()}
	if err := h.Process(ctx, p, raw, q); err != nil {
		t.Fatal(err)
	}
	members, _ := rdb.ZRange(ctx, "mailtest:scheduled", 0, -1).Result()
	if len(members) != 1 {
		t.Fatalf("want 1 scheduled, got %d", len(members))
	}
	var sp dqueue.Payload
	_ = json.Unmarshal([]byte(members[0]), &sp)
	if sp.Attempt != 1 {
		t.Fatalf("want attempt 1, got %d", sp.Attempt)
	}
	if n, _ := rdb.ZCard(ctx, "mailtest:processing").Result(); n != 0 {
		t.Fatalf("want leased member acked (0 processing), got %d", n)
	}
}

func TestEmailHandlerDeadLettersAtMax(t *testing.T) {
	q, rdb := testQueue(t)
	ctx := context.Background()
	p, _ := buildEmailTask("magic_link", "a@b.com", map[string]any{"Link": "x"}, uuid.Nil)
	p.Attempt = len(emailBackoff) // no retries left
	if err := q.Enqueue(ctx, p); err != nil {
		t.Fatal(err)
	}
	raw, gp := leaseOne(t, q)
	h := EmailHandler{Sender: &fakeSender{err: errors.New("smtp down")}, Log: discardLog()}
	if err := h.Process(ctx, gp, raw, q); err != nil {
		t.Fatal(err)
	}
	if dead, _ := rdb.LRange(ctx, "mailtest:dead", 0, -1).Result(); len(dead) != 1 {
		t.Fatalf("want 1 dead, got %d", len(dead))
	}
}

func TestEmailHandlerNilSenderLogsAndAcks(t *testing.T) {
	q, rdb := testQueue(t)
	ctx := context.Background()
	_ = Enqueue(ctx, q, "magic_link", "a@b.com", map[string]any{"Link": "x"}, uuid.Nil)
	raw, p := leaseOne(t, q)
	h := EmailHandler{Sender: nil, Log: discardLog()}
	if err := h.Process(ctx, p, raw, q); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.ZCard(ctx, "mailtest:processing").Result(); n != 0 {
		t.Fatalf("want acked, got %d", n)
	}
}
